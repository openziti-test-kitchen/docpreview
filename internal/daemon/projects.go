package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/pipeline"
	"github.com/netfoundry/docpreview/internal/store"
	"github.com/netfoundry/docpreview/internal/vault"
)

// ProjectsAdmin is the surface for adding and editing projects.
//
// # Why this is gated as hard as credentials
//
// A project row decides what command runs on the build host and whether it runs
// in a container. That is a more direct route to executing code here than the
// vault is — a credential has to be used by something, a build command *is* the
// something. So this reuses the same two gates as SecretsAdmin: every listener
// loopback, and the request itself originating on this machine.
//
// The read path is open, like the secrets read path, and for the same reason: a
// page that cannot say why it is read-only reads as broken. Reads return the
// whole row including the build command, which is not a secret — it is the thing
// an operator most needs to see to know what a project will do.
type ProjectsAdmin struct {
	store *store.Store
	cfg   config.Server
	log   *slog.Logger

	// vault resolves the open vault, or nil while it is locked. A function, not the
	// vault, because the page that unlocks it is served by this daemon — capturing
	// the value at construction would make every project's secrets permanently
	// missing on a daemon that booted locked, which is the ordinary case.
	vault func() *vault.Vault

	// dockerOK and dockerWhy are the startup probe's answer, so the form can grey out
	// a driver that would refuse rather than offering it. Captured once: docker
	// appearing later is not worth a probe per page load, and the log already said
	// what the daemon found.
	dockerOK  bool
	dockerWhy string

	// scanner queues a build for every open pull request on a repository, returning how
	// many. A function rather than the daemon, so this admin keeps depending on nothing
	// but the store and the config — and so a test can count queued builds without a
	// GitHub client.
	scanner func(ctx context.Context, repo model.Repo) (int, error)

	// canceller abandons the build running for a preview, reporting whether there was
	// one. Same reasoning as scanner: this admin depends on the store and the config, not
	// on the daemon.
	canceller func(ctx context.Context, previewID string) bool

	// rebuilder queues a preview's recorded commit again, reporting whether the preview
	// was found.
	rebuilder func(ctx context.Context, previewID string) (bool, error)

	// The docker volume operations behind the cache controls, injectable because they are
	// destructive and reach the real docker daemon.
	//
	// A test that called the real ones deleted the cache volumes of every live preview on
	// the machine it ran on — which is what happened the first time this shipped, and is
	// why they are fields rather than direct calls. Defaulted in NewProjectsAdmin.
	listVolumes   func(ctx context.Context) ([]string, error)
	removeVolumes func(ctx context.Context, previewID string) error
}

func NewProjectsAdmin(st *store.Store, cfg config.Server, log *slog.Logger) *ProjectsAdmin {
	return &ProjectsAdmin{
		store: st, cfg: cfg, log: log.With("component", "projects"),
		listVolumes:   listCacheVolumes,
		removeVolumes: pipeline.RemoveCacheVolumes,
	}
}

// WithVolumeOps replaces the docker volume calls. For tests, which must not delete the
// cache volumes of whatever is running on the machine they happen to run on.
func (a *ProjectsAdmin) WithVolumeOps(
	list func(context.Context) ([]string, error),
	remove func(context.Context, string) error,
) *ProjectsAdmin {
	a.listVolumes, a.removeVolumes = list, remove
	return a
}

// WithVault gives this admin the ability to manage a project's own environment
// variables. Without it the secrets half of the page reports itself unavailable
// rather than absent, for the same reason the read-only banner exists.
func (a *ProjectsAdmin) WithVault(fn func() *vault.Vault) *ProjectsAdmin {
	a.vault = fn
	return a
}

// WithScanner installs the open-pull-request scan.
func (a *ProjectsAdmin) WithScanner(fn func(context.Context, model.Repo) (int, error)) *ProjectsAdmin {
	a.scanner = fn
	return a
}

// WithCanceller installs the build cancellation.
func (a *ProjectsAdmin) WithCanceller(fn func(context.Context, string) bool) *ProjectsAdmin {
	a.canceller = fn
	return a
}

// WithRebuilder installs the per-preview rebuild.
func (a *ProjectsAdmin) WithRebuilder(fn func(context.Context, string) (bool, error)) *ProjectsAdmin {
	a.rebuilder = fn
	return a
}

// WithDocker records what the startup probe found.
func (a *ProjectsAdmin) WithDocker(ok bool, why string) *ProjectsAdmin {
	a.dockerOK, a.dockerWhy = ok, why
	return a
}

func (a *ProjectsAdmin) openVault() *vault.Vault {
	if a.vault == nil {
		return nil
	}
	return a.vault()
}

// Handler routes the projects API.
//
// Absolute patterns for the same reason SecretsAdmin uses them: stripping the
// prefix from a request for exactly "/api/projects" leaves an empty path, which
// ServeMux answers with a redirect to "/".
func (a *ProjectsAdmin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/projects", a.list)
	mux.HandleFunc("GET /api/projects/{$}", a.list)
	mux.HandleFunc("PUT /api/projects/{platform}/{owner}/{repo}", a.gated(a.save))
	mux.HandleFunc("DELETE /api/projects/{platform}/{owner}/{repo}", a.gated(a.remove))

	// A project's own environment variables. Routed under the project rather than
	// under /api/secrets so the scope is expressed by the URL and cannot be spelled
	// wrong: the vault key is composed here from path values that are already
	// validated as a project identity, and the global secrets route rejects the
	// slash that this namespace is built on.
	mux.HandleFunc("PUT /api/projects/{platform}/{owner}/{repo}/secrets/{env}", a.gated(a.setSecret))
	mux.HandleFunc("DELETE /api/projects/{platform}/{owner}/{repo}/secrets/{env}", a.gated(a.delSecret))

	// Build what is already open. A project added here has no webhook behind it, so
	// without this the answer to "I added it, now what" is "wait for somebody to push".
	mux.HandleFunc("POST /api/projects/{platform}/{owner}/{repo}/scan", a.gated(a.scan))

	// Cancelling a build. Keyed on a preview rather than a project, like the cache
	// controls, and here for the same reason: one gate rather than a third copy of it.
	mux.HandleFunc("POST /api/builds/{preview}/cancel", a.gated(a.cancelBuild))
	mux.HandleFunc("POST /api/builds/{preview}/rebuild", a.gated(a.rebuild))

	// Checking an image runs a docker command with an operator-supplied argument and
	// makes a registry round trip, so it is gated like every other write even though it
	// changes nothing here. Unauthenticated, it would be a way to make this host probe
	// arbitrary registries.
	mux.HandleFunc("POST /api/images/inspect", a.gated(a.inspectImage))

	// The build cache is per preview, not per project, so this is not really a
	// projects route — it lives here to inherit this admin's gate rather than
	// grow a third surface with the same two checks to keep in step.
	mux.HandleFunc("DELETE /api/cache/{preview}", a.gated(a.clearCache))
	mux.HandleFunc("DELETE /api/cache", a.gated(a.clearAllCaches))
	mux.HandleFunc("DELETE /api/cache/{$}", a.gated(a.clearAllCaches))
	return mux
}

// available reports whether this daemon may serve the write side at all.
//
// The same rule as the credential surface, deliberately sharing its reasoning
// rather than restating it: on a loopback-only daemon the boundary is the one that
// already protects the binary, and anyone who can reach 127.0.0.1 can edit the
// database directly.
func (a *ProjectsAdmin) available() (bool, string) {
	if len(a.cfg.Listeners) == 0 {
		return false, "no listeners"
	}
	for _, l := range a.cfg.Listeners {
		if l.Ziti != nil {
			return false, "a ziti listener is configured; the admin surface does not yet " +
				"check the dialing identity, so changing what a build runs is refused"
		}
		host, _, err := net.SplitHostPort(l.TCP)
		if err != nil {
			return false, "unparseable listener " + l.TCP
		}
		if !isLoopback(host) {
			return false, fmt.Sprintf("the ingress listens on %s; projects can only be edited "+
				"from a loopback-only daemon", l.TCP)
		}
	}
	return true, ""
}

func (a *ProjectsAdmin) gated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, why := a.available(); !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": why})
			return
		}
		if ok, why := isLocalRequest(r); !ok {
			a.log.Warn("refused a remote project change",
				"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": why})
			return
		}
		next(w, r)
	}
}

// projectsState is what the page renders.
type projectsState struct {
	CanWrite    bool          `json:"can_write"`
	ReadOnlyWhy string        `json:"read_only_why,omitempty"`
	Projects    []projectView `json:"projects"`

	// Defaults are the server-wide values a project inherits when it states none,
	// so the form can show what an empty field will actually do rather than
	// leaving it blank and unexplained.
	Defaults projectDefaults `json:"defaults"`

	// GlobalSecrets are the environment variable names every project already gets
	// from the server config. Names only. The page shows them as inherited, because
	// "this project has no secrets" and "this project has none of its own" look
	// identical otherwise and only one of them means a build will fail.
	GlobalSecrets []string `json:"global_secrets"`

	// VaultLocked distinguishes "no secrets" from "cannot see them". A locked vault
	// renders the panel with an unlock link rather than an empty list, which would
	// read as data loss.
	VaultLocked bool `json:"vault_locked"`

	// SecretsAvailable is false when no vault is wired at all, which is a different
	// thing again: not locked, not empty, just not a feature on this daemon.
	SecretsAvailable bool `json:"secrets_available"`
}

// projectView is a project row plus the names of the secrets scoped to it.
//
// Names, never values, and for the same reason the credential page returns none: a
// project row is readable from anywhere the dashboard is, while writing is loopback
// only. See docs/design/05-secrets.md.
type projectView struct {
	store.Project

	// Secrets are the environment variable names this project injects, sorted.
	Secrets []string `json:"secrets"`
}

type projectDefaults struct {
	Driver string `json:"driver"`
	Image  string `json:"image"`

	// Timeout is the server-wide build.timeout, as the placeholder of the per-project
	// field — so an empty box says what it will actually do instead of leaving the
	// operator to find the number in a config file.
	Timeout string `json:"timeout,omitempty"`

	// DockerAvailable is the startup probe's answer, and DockerDetail is docker's own
	// message when it is no. The form uses them to disable a choice that would refuse,
	// because "docker — not available: the docker command is not on PATH" in a dropdown
	// answers the question, where a build failing an hour later does not.
	DockerAvailable bool   `json:"docker_available"`
	DockerDetail    string `json:"docker_detail,omitempty"`

	// AllowLocalDriver mirrors build.allow_local_driver. The local driver runs a pull
	// request's own build scripts on the host, so it is off unless the operator wrote
	// it down — and the form must not offer what the build will refuse.
	AllowLocalDriver bool `json:"allow_local_driver"`

	// Images are the container images this project knows work, offered as suggestions
	// beside a free-text field rather than as a closed list: the set of images that
	// can run a Docusaurus build is not ours to bound, and a private registry mirror
	// is the normal case in an enterprise.
	Images []string `json:"images"`
}

// Field limits. Enforced server-side because the API is reachable without a browser,
// and a browser's maxlength is a courtesy rather than a constraint.
const (
	maxNotes       = 5000
	maxDisplayName = 120
	maxAvatarRunes = 2

	// Generous for an icon and far too small for anything else: this project's own
	// logo is 2.4 KB of SVG, and the value travels with every projects payload.
	maxAvatarBytes = 16 * 1024
)

// validAvatar accepts two shapes and refuses everything else, returning why.
//
// **Two characters or an emoji**, which is the zero-effort case and what a monogram
// falls back to.
//
// **An inlined image**, as a `data:` URI, for a project that has a real logo. Inlined
// rather than linked, and that is the whole rule: a remote `src` would announce every
// project on the page to whoever hosts the image, every time anybody opened the
// dashboard — on a loopback daemon whose entire premise is that it has no outbound
// dependency. So `http://` and `https://` are refused rather than fetched.
//
// SVG and PNG only. An SVG loaded through `<img src>` cannot run script — that is a
// property of the img element rather than of the file — and the page renders it that
// way for exactly this reason. The size cap is what stops a vault-sized blob living in
// the project row and being sent with every page load.
func validAvatar(a string) string {
	if a == "" {
		return ""
	}
	if strings.HasPrefix(a, "data:") {
		if !strings.HasPrefix(a, "data:image/svg+xml") && !strings.HasPrefix(a, "data:image/png") {
			return "an inlined avatar must be data:image/svg+xml or data:image/png"
		}
		if len(a) > maxAvatarBytes {
			return fmt.Sprintf("an inlined avatar is capped at %d bytes, got %d",
				maxAvatarBytes, len(a))
		}
		return ""
	}
	if strings.Contains(a, "://") {
		return "an avatar cannot be a URL: this dashboard fetches nothing from the " +
			"internet, so inline the image as a data: URI instead"
	}
	// Counted in runes, because an emoji is several bytes and one glyph and a byte cap
	// would refuse the intended use.
	if n := len([]rune(a)); n > maxAvatarRunes {
		return fmt.Sprintf("an avatar is at most %d characters or emoji, got %d",
			maxAvatarRunes, n)
	}
	return ""
}

// knownImages are the suggestions in the image field.
//
// Node images first, because a Docusaurus build needs node and these are the ones
// that have actually been used here. The Debian-based ones are the default for a
// reason worth stating: alpine's musl breaks any dependency shipping a prebuilt
// glibc binary, which for a documentation site usually means sharp or esbuild, and
// the failure is a compile error deep in an install log rather than anything that
// names the image.
func knownImages() []string {
	return []string{
		// The default first, and it is the full image rather than -slim because a build
		// that clones another repository needs git and the slim ones do not have it.
		config.DefaultImage,
		"node:22-bookworm",
		"node:20-bookworm",
		// Smaller, and only usable by a build that clones nothing. The picker annotates
		// them rather than hiding them: it is a real choice, with a condition on it.
		"node:24-bookworm-slim",
		"node:22-bookworm-slim",
		"node:24-alpine",
		"mcr.microsoft.com/devcontainers/javascript-node:24",
		"registry.access.redhat.com/ubi9/nodejs-22",
	}
}

func (a *ProjectsAdmin) snapshot(ctx context.Context, r *http.Request) (projectsState, error) {
	projects, err := a.store.ListProjects(ctx)
	if err != nil {
		return projectsState{}, err
	}

	v := a.openVault()
	st := projectsState{
		Projects: []projectView{},
		Defaults: projectDefaults{
			Driver: a.cfg.Build.Driver, Image: a.cfg.Build.Image,
			Timeout:         a.cfg.Build.Timeout.String(),
			DockerAvailable: a.dockerOK, AllowLocalDriver: a.cfg.Build.AllowLocalDriver,
			Images: knownImages(),
		},
		SecretsAvailable: a.vault != nil,
		VaultLocked:      v == nil,
	}
	if !a.dockerOK {
		st.Defaults.DockerDetail = a.dockerWhy
	}
	for _, p := range projects {
		view := projectView{Project: p, Secrets: []string{}}
		if v != nil {
			prefix := vault.ProjectSecretPrefix(p.Platform, p.Owner, p.Repo)
			for _, k := range v.KeysWithPrefix(prefix) {
				view.Secrets = append(view.Secrets, strings.TrimPrefix(k, prefix))
			}
		}
		st.Projects = append(st.Projects, view)
	}

	st.GlobalSecrets = []string{}
	for env := range a.cfg.Build.Secrets {
		st.GlobalSecrets = append(st.GlobalSecrets, env)
	}
	sort.Strings(st.GlobalSecrets)

	switch ok, why := a.available(); {
	case !ok:
		st.ReadOnlyWhy = why
	default:
		if local, lwhy := isLocalRequest(r); !local {
			st.ReadOnlyWhy = lwhy
		} else {
			st.CanWrite = true
		}
	}
	return st, nil
}

func (a *ProjectsAdmin) list(w http.ResponseWriter, r *http.Request) {
	st, err := a.snapshot(r.Context(), r)
	if err != nil {
		a.log.Error("listing projects", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *ProjectsAdmin) save(w http.ResponseWriter, r *http.Request) {
	p, bad := projectFromRequest(r)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": bad})
		return
	}

	var body struct {
		Enabled      *bool  `json:"enabled"`
		BuildDir     string `json:"build_dir"`
		BuildCommand string `json:"build_command"`
		BuildOutput  string `json:"build_output"`
		BaseURL      string `json:"base_url"`
		DetectScript string `json:"detect_script"`
		Driver       string `json:"driver"`
		Image        string `json:"image"`
		Notes        string `json:"notes"`
		DisplayName  string `json:"display_name"`
		Avatar       string `json:"avatar"`
		Timeout      string `json:"timeout"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Enabled defaults to true for a new project. Adding one and having it do
	// nothing until a second, separate action would be a trap.
	p.Enabled = true
	if body.Enabled != nil {
		p.Enabled = *body.Enabled
	}

	p.BuildDir = strings.TrimSpace(body.BuildDir)
	p.BuildCommand = strings.TrimSpace(body.BuildCommand)
	p.BuildOutput = strings.TrimSpace(body.BuildOutput)
	p.BaseURL = strings.TrimSpace(body.BaseURL)
	p.DetectScript = strings.TrimSpace(body.DetectScript)
	p.Driver = strings.TrimSpace(body.Driver)
	p.Image = strings.TrimSpace(body.Image)
	p.Notes = strings.TrimSpace(body.Notes)
	p.DisplayName = strings.TrimSpace(body.DisplayName)
	p.Avatar = strings.TrimSpace(body.Avatar)
	p.Timeout = strings.TrimSpace(body.Timeout)

	// Checked here, once, rather than at the start of every build. A build that
	// discovers the timeout is unusable has already cloned the repository, and the only
	// thing it can do about it is pick a different number — so the value is refused
	// while somebody is looking at the field they typed it into.
	if why := validTimeout(p.Timeout); why != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": why})
		return
	}

	if p.Driver != "" && p.Driver != config.DriverLocal && p.Driver != config.DriverDocker {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `driver must be "local", "docker", or empty for the server default`})
		return
	}
	// Refused here as well as at build time, so an operator finds out while looking at
	// the form rather than from a build that fails an hour later. The build-time check
	// stays, because a row saved before the setting changed is still a row.
	if p.Driver == config.DriverLocal && !a.cfg.Build.AllowLocalDriver {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "the local driver is not enabled on this daemon: it runs a pull " +
				"request's own build scripts on this host. Set build.allow_local_driver: " +
				"true in the server config if that is what you want"})
		return
	}
	// Long enough for a paragraph of why this project is unusual, short enough that
	// nobody pastes a log into it. The limit is here rather than only in the browser
	// because the API is reachable without one.
	if len(p.Notes) > maxNotes {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("notes are capped at %d characters, got %d", maxNotes, len(p.Notes))})
		return
	}
	if why := validAvatar(p.Avatar); why != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": why})
		return
	}
	if len(p.DisplayName) > maxDisplayName {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("a name is capped at %d characters", maxDisplayName)})
		return
	}
	// A base URL that does not start and end with "/" produces a preview whose assets
	// 404, and the build verifies it against the built output — so this is enforced
	// before a form is saved rather than twenty seconds into a build.
	//
	// Enforced by fixing it, not by refusing it. The slashes are mandatory, this code
	// knows exactly where they go, and "/docs" is not ambiguous — so rejecting it
	// makes the operator retype a value the server could have corrected. That is the
	// whole of the validation that was here, and it produced a page of identical
	// errors while somebody guessed at the punctuation.
	p.BaseURL = normalizeBaseURL(p.BaseURL)
	// What is left is not a punctuation slip and cannot be guessed at: a query or a
	// fragment is not part of a path prefix, and whitespace inside one is a copy-paste
	// accident that would produce a URL nobody can type.
	if strings.ContainsAny(p.BaseURL, " \t?#") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `a base URL is a path like "/docs/" — no spaces, query or fragment`})
		return
	}

	if err := a.store.SaveProject(r.Context(), p); err != nil {
		a.log.Error("saving project", "project", p.Key(), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// The command is logged on purpose. It is the operator's own value, it is what
	// will execute on this host, and a change to it with no record is the kind of
	// thing nobody can reconstruct afterwards.
	a.log.Info("project saved", "project", p.Key(), "driver", p.Driver,
		"command", p.BuildCommand, "enabled", p.Enabled)

	st, err := a.snapshot(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *ProjectsAdmin) remove(w http.ResponseWriter, r *http.Request) {
	p, bad := projectFromRequest(r)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": bad})
		return
	}
	if err := a.store.DeleteProject(r.Context(), p.Platform, p.Owner, p.Repo); err != nil {
		a.log.Error("deleting project", "project", p.Key(), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	a.log.Info("project deleted", "project", p.Key())

	st, err := a.snapshot(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// scan asks the platform what is open on a repository and queues a build for each.
//
// Reported rather than silent, and counted, because "queued 3 builds" and "queued 0
// builds — the App is not installed there" are the two answers somebody who just added a
// project needs to tell apart. A repository with no open pull requests is the third, and
// is not a failure.
//
// It goes through the daemon's ordinary build path, so a scanned pull request is
// indistinguishable from a pushed one: same supersede rules, same commit lock, same
// queued comment on the pull request.
func (a *ProjectsAdmin) scan(w http.ResponseWriter, r *http.Request) {
	p, bad := projectFromRequest(r)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": bad})
		return
	}
	if a.scanner == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot scan for open pull requests"})
		return
	}

	queued, err := a.scanner(r.Context(), model.Repo{
		Platform: model.Platform(p.Platform), Owner: p.Owner, Name: p.Repo,
	})
	if err != nil {
		// The project itself saved. This is a separate action that failed, and its
		// message is the operator's next step — usually "install the App there".
		a.log.Warn("scanning for open pull requests", "project", p.Key(), "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"queued": 0, "error": err.Error()})
		return
	}
	a.log.Info("queued builds for open pull requests", "project", p.Key(), "queued", queued)
	writeJSON(w, http.StatusOK, map[string]any{"queued": queued})
}

// cancelBuild abandons the build running for a preview.
//
// Gated like a write, because it is one: it stops work and reports the pull request as
// failed. Answering whether anything was running rather than 404ing on "nothing to
// cancel" — the button is offered from a page that may be a second out of date, and a
// build that finished on its own between the render and the click is not an error.
func (a *ProjectsAdmin) cancelBuild(w http.ResponseWriter, r *http.Request) {
	preview := r.PathValue("preview")
	if !previewIDPattern.MatchString(preview) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "not a preview id: expected twelve hex characters, got " + preview,
		})
		return
	}
	if a.canceller == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot cancel builds"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": a.canceller(r.Context(), preview)})
}

// rebuild queues a preview's recorded commit again.
func (a *ProjectsAdmin) rebuild(w http.ResponseWriter, r *http.Request) {
	preview := r.PathValue("preview")
	if !previewIDPattern.MatchString(preview) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "not a preview id: expected twelve hex characters, got " + preview,
		})
		return
	}
	if a.rebuilder == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot rebuild"})
		return
	}

	found, err := a.rebuilder(r.Context(), preview)
	if err != nil {
		a.log.Error("rebuilding a preview", "preview", preview, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !found {
		// Gone rather than broken: a preview torn down between the page being drawn and
		// the button being pressed is the ordinary way this happens.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "there is no preview " + preview + " any more"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": true})
}

// inspectImage answers whether a container image can be resolved, so the form can
// refuse a typo while the operator is still looking at it.
//
// Not a validation step on save. A registry can be unreachable for a minute, and a
// project whose image is briefly unresolvable is still the project the operator meant to
// write down — refusing the save would lose their work over a network blip. The form
// warns, the save proceeds, and the build is where an unresolvable image finally fails.
func (a *ProjectsAdmin) inspectImage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Image string `json:"image"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !a.dockerOK {
		writeJSON(w, http.StatusOK, map[string]any{
			"found": false, "checked": false,
			"detail": "docker is not available on this host, so nothing can be checked: " +
				a.dockerWhy,
		})
		return
	}

	st, err := pipeline.InspectImage(r.Context(), body.Image)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"found": st.Found, "local": st.Local, "checked": true, "detail": st.Detail,
	})
}

// setSecret stores one environment variable for one project.
//
// The project does not have to exist first. A row and its credentials are two halves
// of the same setup, and refusing here would force an order on the operator for no
// reason the operator can see — the secret is simply unused until a project claims
// it, exactly like a vault entry nothing reads.
func (a *ProjectsAdmin) setSecret(w http.ResponseWriter, r *http.Request) {
	p, env, v, bad, code := a.secretRequest(r)
	if bad != "" {
		writeJSON(w, code, map[string]string{"error": bad})
		return
	}

	var body struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Same trim as the global surface: trailing whitespace from a paste is the
	// commonest way a token is wrong in a way nothing reports.
	body.Value = strings.TrimSpace(body.Value)
	if body.Value == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty value"})
		return
	}

	key := vault.ProjectSecretKey(p.Platform, p.Owner, p.Repo, env)
	if err := v.Set(key, vault.NewSecretString(body.Value)); err != nil {
		a.log.Error("storing a project secret", "project", p.Key(), "env", env, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// No rearm. The resolver reads the vault at the start of each build, so this
	// applies to the next one — see Daemon.SetProjectSecrets. The variable name is
	// logged and the value is not; the name is the operator's own and is what makes
	// this line worth having.
	a.log.Info("project secret stored", "project", p.Key(), "env", env, "bytes", len(body.Value))
	a.respond(w, r)
}

func (a *ProjectsAdmin) delSecret(w http.ResponseWriter, r *http.Request) {
	p, env, v, bad, code := a.secretRequest(r)
	if bad != "" {
		writeJSON(w, code, map[string]string{"error": bad})
		return
	}

	if err := v.Delete(vault.ProjectSecretKey(p.Platform, p.Owner, p.Repo, env)); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	a.log.Info("project secret deleted", "project", p.Key(), "env", env)
	a.respond(w, r)
}

// secretRequest validates the parts every secret call needs. The returned reason is
// empty on success; code is the status to answer with when it is not.
func (a *ProjectsAdmin) secretRequest(r *http.Request) (
	p store.Project, env string, v *vault.Vault, bad string, code int,
) {
	p, bad = projectFromRequest(r)
	if bad != "" {
		return p, "", nil, bad, http.StatusBadRequest
	}

	env = strings.TrimSpace(r.PathValue("env"))
	if why := validEnvName(env); why != "" {
		return p, env, nil, why, http.StatusBadRequest
	}

	if a.vault == nil {
		return p, env, nil, "this daemon has no credential store wired", http.StatusNotImplemented
	}
	if v = a.openVault(); v == nil {
		return p, env, nil, "the vault is locked; unlock it at /secrets first", http.StatusConflict
	}
	return p, env, v, "", 0
}

// respond answers with a fresh snapshot, so one round trip both changes something
// and returns the state the page should now show.
func (a *ProjectsAdmin) respond(w http.ResponseWriter, r *http.Request) {
	st, err := a.snapshot(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// validEnvName enforces the shell-safe form, returning why not.
//
// Stricter than the global key rule on purpose, because this name is not a lookup
// key — it becomes an environment variable in a process that runs a build script. A
// name with a dot or a dash in it is settable in Go's exec.Cmd and unreadable from
// sh, so the operator would set a value the build could never see. The leading digit
// is refused for the same reason.
//
// Uppercase is not required by any shell, but every convention for an injected
// credential follows it and the build scripts this exists for use it throughout.
func validEnvName(env string) string {
	if env == "" {
		return "an environment variable name is required"
	}
	if len(env) > 128 {
		return "that environment variable name is too long"
	}
	for i, r := range env {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return "an environment variable name is upper-case letters, digits and " +
				"underscore, and cannot start with a digit — e.g. BB_REPO_TOKEN_ONPREM"
		}
	}
	return ""
}

// maxProjectTimeout is the longest a project may cap one of its builds at.
//
// Not a safety limit — the operator writes this value and could raise the server
// default instead. It is a typo limit: "45" parses as 45 nanoseconds and "45h" is a
// day and a half of a worker held by one pull request, and both are far more likely
// to be a slip than an intention.
const maxProjectTimeout = 6 * time.Hour

// validTimeout checks a project's build timeout, returning why not.
//
// Empty is valid and means the server-wide build.timeout, which is the answer for
// almost every project.
func validTimeout(s string) string {
	if s == "" {
		return ""
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return `a build timeout is a duration like "45m", "90s" or "2h" — ` +
			"leave it empty to use the server default"
	}
	// A bare number parses as nanoseconds, so "45" becomes 45ns and every build of
	// that project dies before docker is even invoked. Naming the unit in the error is
	// what tells somebody the number they typed was not the number they meant.
	if d <= 0 {
		return "a build timeout must be positive — leave it empty to use the server default"
	}
	if d < time.Minute {
		return fmt.Sprintf("%q is %s, shorter than any real build — a duration needs "+
			`a unit, e.g. "45m"`, s, d)
	}
	if d > maxProjectTimeout {
		return fmt.Sprintf("a build timeout is capped at %s", maxProjectTimeout)
	}
	return ""
}

// normalizeBaseURL puts a base URL into the only form that works, rather than
// refusing the forms that do not.
//
// Docusaurus and every check in this codebase want a leading and a trailing slash.
// "/docs", "docs/" and "docs" all mean one thing, and it is not in doubt — so they
// are corrected. Empty stays empty: blank means "defer to the repository", which is
// a different answer from "/".
//
// Interior doubled slashes are collapsed because "//docs/" is a protocol-relative
// URL to a host named "docs" in every browser, which is a very confusing way for a
// preview to fail.
func normalizeBaseURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	// A pasted absolute URL keeps its path and loses the rest. Somebody copying
	// "https://docs.example.com/docs/" out of a browser means the "/docs/" part; the
	// host is this preview's own and not theirs to set. Before slash collapsing,
	// which would otherwise eat the "//" in the scheme.
	if i := strings.Index(base, "://"); i >= 0 {
		rest := base[i+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			base = rest[slash:]
		} else {
			base = "/"
		}
	}
	for strings.Contains(base, "//") {
		base = strings.ReplaceAll(base, "//", "/")
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base
}

// projectFromRequest reads the identity out of the path, returning a reason when
// it is not usable.
//
// Validated rather than trusted: these three values become a primary key and are
// compared against what a webhook reports, so a stray slash or an empty segment
// produces a row no delivery can ever match — a project that looks configured and
// silently never applies.
func projectFromRequest(r *http.Request) (store.Project, string) {
	p := store.Project{
		Platform: strings.TrimSpace(r.PathValue("platform")),
		Owner:    strings.TrimSpace(r.PathValue("owner")),
		Repo:     strings.TrimSpace(r.PathValue("repo")),
	}
	for _, f := range []struct{ name, value string }{
		{"platform", p.Platform}, {"owner", p.Owner}, {"repo", p.Repo},
	} {
		if f.value == "" {
			return p, f.name + " is required"
		}
		if strings.ContainsAny(f.value, "/\\ ") {
			return p, f.name + " cannot contain a slash or a space"
		}
	}
	switch p.Platform {
	case "github", "bitbucket", "local":
	default:
		return p, `platform must be "github", "bitbucket" or "local"`
	}
	return p, ""
}
