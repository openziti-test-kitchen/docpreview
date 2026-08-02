package daemon

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// maxWebhookBody caps a webhook payload.
//
// GitHub's own limit is 25 MB. Accepting that from an endpoint that is exposed
// to the internet means an attacker with a valid signature — or without one,
// since the body must be fully read before it can be verified — can make the
// process allocate 25 MB per request. Two megabytes is far more than any
// pull_request payload and small enough to be harmless in bulk.
const maxWebhookBody = 2 << 20

// CommentReader exposes the comments docpreview owns on the local platform.
type CommentReader interface {
	ListComments(ctx context.Context) ([]store.Comment, error)
}

// Ingress is the HTTP surface: webhook endpoints plus health and status.
type Ingress struct {
	daemon   *Daemon
	comments CommentReader
	log      *slog.Logger

	// secrets is the setup surface. Nil when the caller did not wire one, in
	// which case the routes are absent rather than refusing — a daemon with no
	// credential management should not advertise one.
	secrets *SecretsAdmin

	// projects is the project admin surface, on the same terms.
	projects *ProjectsAdmin

	// zrok is the zrok account and environment surface, on the same terms. Nil is the state for
	// an installation using another exposer, where a signup panel would be an invitation to
	// enrol against a service nothing here would publish through.
	zrok *ZrokAdmin

	// console is the login gate. Nil when no store was wired, in which case there is no
	// login and the locality rules decide everything, exactly as before this existed.
	console *console

	// google resolves the OAuth application's credentials, per request because the vault may
	// be locked at startup and unlocked later. Nil when no Google sign-in is wired.
	google GoogleCredentials

	// mu guards clients, which the setup page can add to after the daemon is
	// already serving. See SetClient.
	mu      sync.Mutex
	clients map[model.Platform]scm.Client
}

// SetClient installs the client for a platform, replacing any already there.
//
// The mirror of Daemon.SetClient, and needed for the same reason: a GitHub
// client cannot exist until the vault is unlocked, and the page that unlocks it
// is served by this Ingress. Until then /webhook/github answers 501, which is
// the truth — nothing could be verified.
func (i *Ingress) SetClient(platform model.Platform, c scm.Client) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.clients == nil {
		i.clients = map[model.Platform]scm.Client{}
	}
	i.clients[platform] = c
}

// client resolves the client for a platform under the lock.
func (i *Ingress) client(platform model.Platform) (scm.Client, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	c, ok := i.clients[platform]
	return c, ok
}

// WithSecrets attaches the credential admin surface.
func (i *Ingress) WithSecrets(a *SecretsAdmin) *Ingress {
	i.secrets = a
	return i
}

// WithProjects attaches the project admin surface.
func (i *Ingress) WithProjects(a *ProjectsAdmin) *Ingress {
	i.projects = a
	return i
}

// WithZrok attaches the zrok account and environment surface.
func (i *Ingress) WithZrok(a *ZrokAdmin) *Ingress {
	i.zrok = a
	return i
}

// WithLogin attaches the password gate, reading and writing its hashes in this store.
//
// Optional, so a caller that wires no store gets the previous behaviour rather than a daemon
// that cannot be reached: no login, and the locality and identity rules deciding everything.
func (i *Ingress) WithLogin(st *store.Store) (*Ingress, error) {
	c, err := newConsole(st)
	if err != nil {
		return i, err
	}
	i.console = c
	return i, nil
}

// NewIngress builds the HTTP surface.
func NewIngress(d *Daemon, clients map[model.Platform]scm.Client, comments CommentReader, log *slog.Logger) *Ingress {
	return &Ingress{
		daemon: d,
		// Copied, not aliased: Daemon holds its own copy of the same map, and
		// SetClient on either must not race the other's reads.
		clients:  maps.Clone(clients),
		comments: comments,
		log:      log.With("component", "ingress"),
	}
}

// Handler returns the configured mux, wrapped in the login gate.
func (i *Ingress) Handler() http.Handler {
	return i.requireLogin(i.routes())
}

// routes builds the mux itself.
//
// Separate from Handler so the middleware wraps *everything* by construction rather than by
// each route remembering to ask. A route added later is protected without its author knowing
// this exists, which is the only arrangement that stays true — the exemptions are a short list
// in one function, see openPath.
func (i *Ingress) routes() http.Handler {
	mux := http.NewServeMux()

	// The login form and the session it issues. Registered unconditionally: a daemon with no
	// password serves a page saying so, which is a better answer to somebody who went looking
	// for it than a 404.
	mux.HandleFunc("GET /login", i.login)
	mux.HandleFunc("POST /login", i.login)
	mux.HandleFunc("POST /logout", i.logout)

	// Google sign-in, which grants viewer and never admin. Both halves are GET because the
	// browser is redirected into them.
	mux.HandleFunc("GET /auth/google", i.googleStart)
	mux.HandleFunc("GET /auth/google/callback", i.googleCallback)

	mux.HandleFunc("POST /webhook/github", i.webhook(model.PlatformGitHub))
	mux.HandleFunc("POST /webhook/bitbucket", i.webhook(model.PlatformBitbucket))
	mux.HandleFunc("POST /webhook/local", i.webhook(model.PlatformLocal))
	mux.HandleFunc("GET /healthz", i.healthz)
	mux.HandleFunc("GET /readyz", i.readyz)
	mux.HandleFunc("GET /status", i.status)

	// The stand-in for a pull request page. On GitHub the comment lives on the
	// pull request; on the local platform there is nowhere else to put it, and
	// seeing it update in place is the whole point of the exercise.
	mux.HandleFunc("GET /pr", i.prIndex)
	mux.HandleFunc("GET /pr/", i.prIndex)

	// Server-sent events, one connection per open tab instead of a poll from each. See stream.go.
	mux.HandleFunc("GET /events", i.streamStatus)
	mux.HandleFunc("GET /logs/{preview}/stream", i.streamLog)
	mux.HandleFunc("GET /logs/{preview}", i.listLogs)
	mux.HandleFunc("GET /logs/{preview}/download", i.downloadLog)
	mux.HandleFunc("GET /logs/{preview}/download/{build}", i.downloadLog)

	mux.HandleFunc("GET /{$}", i.dashboard)

	// What the page asks before drawing the Secrets and Projects links. Registered
	// unconditionally so the page gets an answer rather than a 404 it has to
	// interpret, and it reports false for a surface that is not wired. See admin.go.
	mux.HandleFunc("GET /api/admin", i.admin)

	// /v2 was the second dashboard while the two layouts sat side by side. It
	// won and replaced the original, so the path redirects rather than 404s —
	// it was linked from the old page's footer and is in browser histories.
	mux.HandleFunc("GET /v2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
	})

	// An exposer that serves previews under a path on this listener rather than
	// at its own host. Registered without a method so HEAD works, which is what
	// link checkers and `curl -I` use.
	if pe, ok := i.daemon.Exposer().(expose.PathExposer); ok {
		mux.Handle(expose.MountPrefix, pe.Handler())
	}

	if i.secrets != nil {
		mux.Handle("/api/secrets", i.secrets.Handler())
		mux.Handle("/api/secrets/", i.secrets.Handler())

		// Credential management gets its own address rather than a panel on the
		// operations dashboard. Two reasons beyond it reading oddly: a URL can be
		// bookmarked and linked in a runbook, and a distinct path is something a
		// proxy or a future authentication layer can gate on its own — a panel
		// inside "/" cannot be.
		//
		// The same document, switched by pathname. The page is one embedded file
		// and splitting it would mean two copies of the styles and the fetch
		// helper to keep in step.
		mux.HandleFunc("GET /secrets", i.dashboard)
		mux.HandleFunc("GET /secrets/{$}", i.dashboard)
	}

	// The zrok panel lives on /secrets, beside the credentials, because the account token is one
	// and because "how does this reach the internet" and "what credentials does it hold" are the
	// same visit. Mounted here rather than only declared in the admin's own mux, for the reason
	// spelled out on /api/settings/ below.
	if i.zrok != nil {
		mux.Handle("/api/zrok", i.zrok.Handler())
		mux.Handle("/api/zrok/", i.zrok.Handler())
	}

	if i.projects != nil {
		mux.Handle("/api/projects", i.projects.Handler())
		mux.Handle("/api/projects/", i.projects.Handler())

		// The build cache clear, which is keyed on a preview rather than a project
		// but is served by the same admin so there is one gate rather than two.
		mux.Handle("/api/cache", i.projects.Handler())
		mux.Handle("/api/cache/", i.projects.Handler())

		// Checking a container image, for the same reason: it runs docker with an
		// operator-supplied argument, so it belongs behind a gate that already exists
		// rather than a third copy of the same two checks.
		mux.Handle("/api/images/", i.projects.Handler())

		// Cancelling a build, for the same reason: one gate, already written.
		mux.Handle("/api/builds/", i.projects.Handler())

		// Installation-wide settings — today the name prefix, which decides the public
		// hostname of every preview. Same gate for the same reason as the rest of this
		// list, and mounted here rather than only registered inside the admin's own mux:
		// this is where a path becomes reachable, and a route the admin declares but the
		// ingress does not forward answers 404 while looking perfectly wired.
		mux.Handle("/api/settings/", i.projects.Handler())

		// Its own address, for the same reasons /secrets has one: it can be linked
		// from a runbook, and a proxy or a future authentication layer can gate one
		// path where it cannot gate a panel inside "/".
		mux.HandleFunc("GET /projects", i.dashboard)
		mux.HandleFunc("GET /projects/{$}", i.dashboard)
	}
	return mux
}

// dashboardHTML is the operator dashboard.
//
// Embedded rather than served from disk so the binary stays the only artifact
// that has to be deployed, which is the whole premise of the project. It
// subscribes to /events for state and to /logs/{preview}/stream for live build
// output — see stream.go. There is no build step and no dependency to fetch.
//
// One row per project, not per preview: a project with a dozen open pull
// requests otherwise produced a dozen rows all naming the same project, and the
// list stopped being scannable at the point it started being useful. The row's
// newest preview is shown and the rest are a dropdown inside it. Expanding a
// row tails that preview's build log in place, one stream at a time.
//
//go:embed dashboard.html
var dashboardHTML []byte

func (i *Ingress) dashboard(w http.ResponseWriter, _ *http.Request) {
	serveHTML(w, dashboardHTML)
}

func serveHTML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// webhook returns a handler for one platform's deliveries.
func (i *Ingress) webhook(platform model.Platform) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client, ok := i.client(platform)
		if !ok {
			http.Error(w, "platform not configured", http.StatusNotImplemented)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
		if err != nil {
			http.Error(w, "reading body", http.StatusBadRequest)
			return
		}
		if len(body) > maxWebhookBody {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}

		events, err := client.VerifyWebhook(r.Context(), r.Header, body)
		if err != nil {
			if errors.Is(err, scm.ErrBadSignature) {
				// Do not echo the reason: a caller probing for a valid secret
				// learns nothing from "signature mismatch" that they should
				// not have to guess.
				i.log.Warn("rejected webhook", "platform", platform, "remote", r.RemoteAddr, "error", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			i.log.Error("processing webhook", "platform", platform, "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Acknowledge before doing the work. GitHub gives a webhook ten seconds
		// to respond and marks the delivery failed after that; enqueuing a
		// build takes microseconds, but the report that follows it is a network
		// round trip back to GitHub, which is exactly the kind of thing that
		// occasionally takes eleven seconds.
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "accepted\n")

		for _, ev := range events {
			ev := ev
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				if err := i.daemon.Handle(ctx, ev); err != nil {
					i.log.Error("handling event",
						"kind", ev.Kind, "pr", ev.PR.String(), "delivery", ev.Delivery, "error", err)
				}
			}()
		}
	}
}

// prIndex renders every comment docpreview currently owns.
//
// This is deliberately plain and deliberately auto-refreshing. What it is for
// is watching a single comment change state — queued, building, ready — and
// seeing the revision counter climb while the comment count stays at one. That
// is the behaviour the whole marker-and-upsert design exists to produce, and on
// a hosted platform you would have to trust the edit history to see it.
func (i *Ingress) prIndex(w http.ResponseWriter, r *http.Request) {
	comments, err := i.comments.ListComments(r.Context())
	if err != nil {
		i.log.Error("listing comments", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	fmt.Fprint(w, `<!doctype html><meta charset=utf-8>
<meta http-equiv=refresh content=2>
<title>docpreview — pull requests</title>
<style>
 body{font:15px/1.5 ui-monospace,Consolas,monospace;max-width:52rem;margin:2rem auto;padding:0 1rem}
 .c{border:1px solid #8884;border-radius:6px;padding:1rem;margin:1rem 0}
 .h{display:flex;justify-content:space-between;font-weight:700;margin-bottom:.5rem}
 .r{opacity:.6;font-weight:400}
 pre{white-space:pre-wrap;word-break:break-word;margin:0;font:inherit}
 a{color:inherit}
 .n{opacity:.6}
</style>
<h1>Pull requests</h1>
`)

	if len(comments) == 0 {
		fmt.Fprint(w, `<p class=n>No comments yet. Push to a repository under `+
			`<code>local.repos_dir</code>, or POST to <code>/webhook/local</code>.</p>`)
		return
	}

	for _, c := range comments {
		fmt.Fprintf(w, `<div class=c><div class=h><span>%s#%d <span class=r>%s</span></span>`+
			`<span class=r>revision %d &middot; %s</span></div><pre>%s</pre></div>`,
			html.EscapeString(c.PR.Repo.Name), c.PR.Number,
			html.EscapeString(c.PR.Branch),
			c.Revision,
			c.UpdatedAt.Format("15:04:05"),
			html.EscapeString(c.Body))
	}

	// The revision counter is the evidence. One comment edited five times is
	// the design working; five comments would be the bug it exists to avoid.
	fmt.Fprintf(w, `<p class=n>%d comment(s). This page refreshes every 2s.</p>`, len(comments))
}

func (i *Ingress) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}

// readyStage is the recovery stage as /readyz reports it: named separately from
// StartupProgress and copied field by field, so a field added there — Items was the one that
// mattered — does not silently become public here.
type readyStage struct {
	Stage string `json:"stage"`
	Note  string `json:"note"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// readyReport is the whole of what an unauthenticated caller may learn: how busy the daemon is,
// and never what it is busy with.
type readyReport struct {
	Starting bool        `json:"starting"`
	Startup  *readyStage `json:"startup,omitempty"`
	Pending  int         `json:"pending"`
	Running  int         `json:"running"`
	Ready    int         `json:"ready"`

	// Instance is the process start stamp, so a caller can tell the daemon it restarted from
	// one that was already running and answered first.
	Instance string `json:"instance"`
}

// readyz reports whether recovery has finished and whether the queue is quiet.
//
// `/status` sits behind the login, but the thing that waits for a restart is a script, not a person:
// `Restart-Docpreview.ps1` and the demo harness both poll until the daemon says it has recovered. Gating
// `/status` alone would lock them out, turning a restart into a wait loop that never returns.
//
// Handing the scripts a password puts one in a shell history and in every runbook. Leaving `/status` open
// instead would contradict the whole point of the gate, since that payload enumerates every open
// documentation pull request across every project.
//
// So this is a separate, deliberately thin surface: counts and a stage name, and nothing that
// identifies a repository, a branch, a pull request or a URL. A caller learns that the daemon is
// busy, not what it is busy with. That is what makes it safe to leave open, and it is the rule to
// check any addition here against.
func (i *Ingress) readyz(w http.ResponseWriter, r *http.Request) {
	st, err := i.daemon.Status(r.Context())
	if err != nil {
		i.log.Error("building readiness", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Counted here rather than returning the rows, because the count is the whole of what a
	// waiting script needs and the rows are the part that must not be public.
	ready := 0
	for _, p := range st.Previews {
		if p.State == "ready" && p.URL != "" {
			ready++
		}
	}

	// The stage, without its Items. Those read "adopted a-customer-connect-docs-feature-…",
	// which names a repository and a branch — exactly what this endpoint must not say.
	var stage *readyStage
	if p := st.Startup; p != nil {
		stage = &readyStage{Stage: p.Stage, Note: p.Note, Done: p.Done, Total: p.Total}
	}

	writeJSON(w, http.StatusOK, readyReport{
		Starting: st.Starting,
		Startup:  stage,
		Pending:  st.Pending,
		Running:  st.Running,
		Ready:    ready,
		Instance: st.Instance,
	})
}

// status reports the live previews.
//
// This endpoint is bound to the ingress listener, which is loopback by default
// and only reaches the internet if the operator deliberately shares it. It
// carries no secrets — preview URLs, branch names, and states — but it does
// enumerate every open documentation pull request, so do not share it publicly
// alongside the webhook endpoint without thinking about that.
func (i *Ingress) status(w http.ResponseWriter, r *http.Request) {
	st, err := i.daemon.Status(r.Context())
	if err != nil {
		i.log.Error("building status", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Whose session this is, which only the request knows. The page needs it to decide whether
	// to offer a sign-out: the cookie is HttpOnly, so JavaScript cannot look.
	st.Role = string(roleOfContext(r.Context()))
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		i.log.Error("writing status", "error", err)
	}
}
