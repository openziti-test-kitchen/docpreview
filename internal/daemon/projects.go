package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/store"
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
}

func NewProjectsAdmin(st *store.Store, cfg config.Server, log *slog.Logger) *ProjectsAdmin {
	return &ProjectsAdmin{store: st, cfg: cfg, log: log.With("component", "projects")}
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
	CanWrite    bool            `json:"can_write"`
	ReadOnlyWhy string          `json:"read_only_why,omitempty"`
	Projects    []store.Project `json:"projects"`

	// Defaults are the server-wide values a project inherits when it states none,
	// so the form can show what an empty field will actually do rather than
	// leaving it blank and unexplained.
	Defaults projectDefaults `json:"defaults"`
}

type projectDefaults struct {
	Driver string `json:"driver"`
	Image  string `json:"image"`
}

func (a *ProjectsAdmin) snapshot(ctx context.Context, r *http.Request) (projectsState, error) {
	projects, err := a.store.ListProjects(ctx)
	if err != nil {
		return projectsState{}, err
	}

	st := projectsState{
		Projects: projects,
		Defaults: projectDefaults{Driver: a.cfg.Build.Driver, Image: a.cfg.Build.Image},
	}
	if st.Projects == nil {
		st.Projects = []store.Project{}
	}

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

	if p.Driver != "" && p.Driver != "local" && p.Driver != "docker" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `driver must be "local", "docker", or empty for the server default`})
		return
	}
	// A base URL that does not start and end with "/" produces a preview whose
	// assets 404, and the build verifies it against the built output — so the
	// error belongs here, where somebody is looking at a form, rather than twenty
	// seconds into a build.
	if p.BaseURL != "" && (!strings.HasPrefix(p.BaseURL, "/") || !strings.HasSuffix(p.BaseURL, "/")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `base_url must start and end with "/", e.g. "/" or "/docs/"`})
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
