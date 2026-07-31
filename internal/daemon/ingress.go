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

// Handler returns the configured mux.
func (i *Ingress) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/github", i.webhook(model.PlatformGitHub))
	mux.HandleFunc("POST /webhook/bitbucket", i.webhook(model.PlatformBitbucket))
	mux.HandleFunc("POST /webhook/local", i.webhook(model.PlatformLocal))
	mux.HandleFunc("GET /healthz", i.healthz)
	mux.HandleFunc("GET /status", i.status)

	// The stand-in for a pull request page. On GitHub the comment lives on the
	// pull request; on the local platform there is nowhere else to put it, and
	// seeing it update in place is the whole point of the exercise.
	mux.HandleFunc("GET /pr", i.prIndex)
	mux.HandleFunc("GET /pr/", i.prIndex)

	// Server-sent events, replacing what used to be a one-second poll from
	// every open tab. See stream.go.
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
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		i.log.Error("writing status", "error", err)
	}
}
