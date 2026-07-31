package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// unlinker removes a preview and records that its pull request must not be built
	// again, reporting whether the preview was found. linker is the other direction, by
	// pull request number, because a pull request nothing has built yet has no preview
	// id to name it by.
	unlinker func(ctx context.Context, previewID string) (bool, error)
	linker   func(ctx context.Context, repo model.Repo, number int) error

	// brancher builds a branch with no pull request behind it, returning the branch it
	// built — which the caller does not necessarily know, since an empty branch means "this
	// repository's default" and that is read from the platform rather than assumed.
	//
	// A function rather than the daemon, like every other one of these, so this admin
	// depends on the store and the config only.
	brancher func(ctx context.Context, repo model.Repo, branch string) (string, error)

	// prefix reads and writes the per-installation name prefix. Two functions rather than
	// one accessor, because the read is on every page load and the write is gated.
	readPrefix func() string
	setPrefix  func(ctx context.Context, prefix string) error

	// scmChecker verifies that a project's source-control credential reaches its
	// repository, returning what it found. Nil for a daemon with no platform that can be
	// checked, in which case the route answers 501 rather than pretending.
	scmChecker func(ctx context.Context, repo model.Repo) (string, error)

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

// WithLinking installs the unlink and link actions. Both or neither: a page that can
// unlink a pull request and not link it back offers a one-way door.
func (a *ProjectsAdmin) WithLinking(
	unlink func(context.Context, string) (bool, error),
	link func(context.Context, model.Repo, int) error,
) *ProjectsAdmin {
	a.unlinker, a.linker = unlink, link
	return a
}

// WithBrancher installs the branch-preview build: the permanent preview of a project's
// default branch, started when the project is created and rebuildable from its card.
func (a *ProjectsAdmin) WithBrancher(fn func(context.Context, model.Repo, string) (string, error)) *ProjectsAdmin {
	a.brancher = fn
	return a
}

// WithNamePrefix installs the read and write of the per-installation name prefix.
func (a *ProjectsAdmin) WithNamePrefix(read func() string,
	set func(context.Context, string) error,
) *ProjectsAdmin {
	a.readPrefix, a.setPrefix = read, set
	return a
}

// WithSCMChecker installs the credential test behind the projects page's Test button.
func (a *ProjectsAdmin) WithSCMChecker(fn func(context.Context, model.Repo) (string, error)) *ProjectsAdmin {
	a.scmChecker = fn
	return a
}

// WithDocker records what the startup probe found.
func (a *ProjectsAdmin) WithDocker(ok bool, why string) *ProjectsAdmin {
	a.dockerOK, a.dockerWhy = ok, why
	return a
}

// namePrefix is the installation's prefix, or empty on a daemon that has no reader wired —
// which is every test that builds this admin directly.
func (a *ProjectsAdmin) namePrefix() string {
	if a.readPrefix == nil {
		return ""
	}
	return a.readPrefix()
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

	// A project's own source-control credential, which is not one of its variables and is
	// deliberately not routed under /secrets/. Bitbucket needs this because an access
	// token there is scoped to a repository unless an administrator allows wider ones.
	mux.HandleFunc("PUT /api/projects/{platform}/{owner}/{repo}/scm/{name}", a.gated(a.setSCM))
	mux.HandleFunc("POST /api/projects/{platform}/{owner}/{repo}/scm-test", a.gated(a.testSCM))

	// Build what is already open. A project added here has no webhook behind it, so
	// without this the answer to "I added it, now what" is "wait for somebody to push".
	mux.HandleFunc("POST /api/projects/{platform}/{owner}/{repo}/scan", a.gated(a.scan))

	// Cancelling a build. Keyed on a preview rather than a project, like the cache
	// controls, and here for the same reason: one gate rather than a third copy of it.
	mux.HandleFunc("POST /api/builds/{preview}/cancel", a.gated(a.cancelBuild))
	mux.HandleFunc("POST /api/builds/{preview}/rebuild", a.gated(a.rebuild))

	// Unlinking is keyed on the preview, because that is what the operator is looking
	// at when they decide they do not want it. Linking is keyed on the project and a
	// number, because a pull request nothing has built has no preview.
	mux.HandleFunc("POST /api/builds/{preview}/unlink", a.gated(a.unlink))
	mux.HandleFunc("POST /api/projects/{platform}/{owner}/{repo}/link", a.gated(a.link))

	// The branch preview. Fired automatically when a project is created; this route is for
	// rebuilding it, and for a project that was added before the daemon could reach the
	// platform to ask what its default branch is called.
	mux.HandleFunc("POST /api/projects/{platform}/{owner}/{repo}/branch", a.gated(a.branch))

	// The installation's name prefix. Not under /api/projects/{...} because it belongs to
	// the daemon rather than to a project, but served by this admin so it inherits the same
	// two gates — it decides the public hostname of every preview, which is not a thing a
	// remote caller may change.
	mux.HandleFunc("PUT /api/settings/prefix", a.gated(a.setNamePrefix))

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
// One rule, one implementation, shared with the credential surface — see
// listenersAllowAdmin. A project row decides what command runs on the build host and the
// vault decides which credentials it runs with; guarding them differently would mean
// guarding the weaker of the two.
func (a *ProjectsAdmin) available() (bool, string) {
	return listenersAllowAdmin(a.cfg.Listeners, "projects")
}

func (a *ProjectsAdmin) gated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// An authenticated admin skips the listener check, for the reason spelled out on
		// SecretsAdmin.gated: that check is a proxy for "we cannot tell who this is", and a
		// session answers the question directly.
		if roleOfContext(r.Context()) == RoleAdmin {
			next(w, r)
			return
		}
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

	// Note is something that happened alongside the request and did not fail it — today,
	// only a default-branch preview that could not be started when a project was created.
	//
	// Carried on the state rather than raised as an error because the project *was* saved:
	// answering 500 would tell the operator their work was lost when it was not. The page
	// toasts this, so the one thing that did not work is still said out loud.
	Note string `json:"note,omitempty"`
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

	// SCM names which of this project's own source-control credentials are stored —
	// "scm.access_token", "scm.email", "scm.api_token". Names only, like every other
	// credential surface here: nothing reads a value back.
	//
	// Present so the form can say "set" rather than showing an empty box that gives no
	// way to tell a stored token from a missing one.
	SCM []string `json:"scm,omitempty"`

	// Ignored are the pull requests unlinked on this repository, lowest number first.
	//
	// Displayed rather than only enforced. An ignore that nothing shows is
	// indistinguishable from a build system that has quietly stopped noticing a pull
	// request, and this list is also the only place a mistaken unlink can be undone.
	Ignored []store.IgnoredPR `json:"ignored,omitempty"`

	// Branch is this project's permanent branch preview — the state of its default branch,
	// which is the thing an operator looks at most and the one preview that is not about a
	// review.
	//
	// Nil when there is none yet: a project added before the daemon could reach the platform,
	// or one whose first build has not finished. The card offers to start it in that case,
	// which is why the absence has to be distinguishable from a preview that exists and is
	// failing.
	Branch *branchView `json:"branch,omitempty"`
}

// branchView is the branch preview as the projects page needs it: enough to render a link
// and say whether it works, and nothing else.
type branchView struct {
	Name      string `json:"name"`
	PreviewID string `json:"preview_id"`
	URL       string `json:"url,omitempty"`
	State     string `json:"state"`
	Commit    string `json:"commit,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
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

	// SCMGlobal names the workspace-wide source-control credentials that are stored —
	// "bitbucket.access_token", "bitbucket.email", "bitbucket.api_token". Names only.
	//
	// The form needs it to say which credential a project will actually use: with one
	// stored, a blank per-project field means "inherit this", and with none it means "this
	// repository cannot be cloned". Those are opposite meanings for the same empty box,
	// and the page cannot tell them apart without being told.
	SCMGlobal []string `json:"scm_global,omitempty"`

	// Frameworks is the preset table, so the dropdown and the placeholders under it come
	// from the same source the build uses. A copy in the page would drift, and the way it
	// would drift is a form promising one build command and the build running another.
	Frameworks []config.Framework `json:"frameworks,omitempty"`

	// Framework is the preset a *new* project's form starts on. Not applied to a stored
	// blank, which would change what every existing project builds.
	Framework string `json:"framework,omitempty"`

	// Prefix is what every preview hostname this installation publishes starts with.
	//
	// On this page because it is the setting that lets two installations share one exposer
	// account, and the moment somebody needs it is the moment they are standing up the
	// second one — not a moment when they want to edit a YAML file on a host they have just
	// built. Empty means no prefix, which is every existing installation.
	Prefix string `json:"prefix"`

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

// branchPreviewFor picks a repository's branch preview out of the preview list.
//
// Nil when there is none, which the card renders as an offer to start one rather than as an
// empty space. A repository has at most one today — the default branch's — but the search is
// written as "the first branch preview of this repository" rather than assuming that, since
// building an arbitrary branch is the same code path and nothing stops a second one existing.
//
// Matched on IsBranch rather than on a branch name, because the name is whatever the platform
// said its default was and this code does not get to assume it is called `main`.
func branchPreviewFor(previews []store.Preview, repo model.Repo) *branchView {
	for _, p := range previews {
		if !p.PR.IsBranch() || p.PR.Repo != repo {
			continue
		}
		return &branchView{
			Name:      p.PR.Branch,
			PreviewID: p.PreviewID,
			URL:       p.URL,
			State:     string(p.State),
			Commit:    p.Commit,
			UpdatedAt: p.UpdatedAt.Format(time.RFC3339),
		}
	}
	return nil
}

func (a *ProjectsAdmin) snapshot(ctx context.Context, r *http.Request) (projectsState, error) {
	projects, err := a.store.ListProjects(ctx)
	if err != nil {
		return projectsState{}, err
	}

	// The branch preview lives in the previews table, not in the project row: it is a
	// preview like any other, and duplicating its URL onto the project would give the page
	// two sources for one fact that a failed republish could make disagree.
	//
	// A failure is not worth failing the page for — the cards render without the link.
	previews, err := a.store.ListPreviews(ctx)
	if err != nil {
		a.log.Warn("listing previews for the branch links", "error", err)
	}

	v := a.openVault()
	st := projectsState{
		Projects: []projectView{},
		Defaults: projectDefaults{
			Driver: a.cfg.Build.Driver, Image: a.cfg.Build.Image,
			Timeout:         a.cfg.Build.Timeout.String(),
			DockerAvailable: a.dockerOK, AllowLocalDriver: a.cfg.Build.AllowLocalDriver,
			Frameworks: config.Frameworks(),
			Framework:  config.FrameworkDefault,
			Images:     knownImages(),
			Prefix:     a.namePrefix(),
		},
		SecretsAvailable: a.vault != nil,
		VaultLocked:      v == nil,
	}
	if !a.dockerOK {
		st.Defaults.DockerDetail = a.dockerWhy
	}
	// Which workspace-wide credentials exist, so an empty per-project box can say whether
	// it means "inherit" or "nothing will work". Names only — nothing reads a value back.
	if v != nil {
		for _, k := range []string{vault.KeyBitbucketAccessToken, vault.KeyBitbucketEmail,
			vault.KeyBitbucketAPIToken} {
			if _, err := v.Get(k); err == nil {
				st.Defaults.SCMGlobal = append(st.Defaults.SCMGlobal, k)
			}
		}
	}
	for _, p := range projects {
		view := projectView{Project: p, Secrets: []string{}}
		if v != nil {
			prefix := vault.ProjectSecretPrefix(p.Platform, p.Owner, p.Repo)
			for _, k := range v.KeysWithPrefix(prefix) {
				name := strings.TrimPrefix(k, prefix)
				// The two kinds share a prefix and are listed separately, because one is
				// injected into every build and the other is what the daemon clones with.
				// Before this split, a project's access token appeared in the page's list
				// of build variables — which is both wrong and alarming.
				if vault.IsProjectSCMKey(name) {
					view.SCM = append(view.SCM, name)
					continue
				}
				view.Secrets = append(view.Secrets, name)
			}
		}
		// A failure here is not worth failing the page for: the list is informational,
		// and the enforcement that matters happens in the build path.
		repo := model.Repo{Platform: model.Platform(p.Platform), Owner: p.Owner, Name: p.Repo}
		ignored, err := a.store.ListIgnored(r.Context(), repo)
		if err != nil {
			a.log.Warn("listing unlinked pull requests", "project", p.Key(), "error", err)
		}
		view.Ignored = ignored
		view.Branch = branchPreviewFor(previews, repo)
		st.Projects = append(st.Projects, view)
	}

	st.GlobalSecrets = []string{}
	for env := range a.cfg.Build.Secrets {
		st.GlobalSecrets = append(st.GlobalSecrets, env)
	}
	sort.Strings(st.GlobalSecrets)

	// The admin-session shortcut first, mirroring gated. Without it a logged-in admin on a
	// daemon that listens on more than loopback would be shown a read-only page whose every
	// button then worked, which is as misleading as the reverse.
	switch ok, why := a.available(); {
	case roleOfContext(r.Context()) == RoleAdmin:
		st.CanWrite = true
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
		Private      *bool  `json:"private"`
		Framework    string `json:"framework"`
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

	// The framework preset. Validated against the table rather than stored as typed: an id
	// this binary does not know silently falls back to the repository's own configuration
	// at build time, which is a project that looks configured and is not.
	p.Framework = strings.TrimSpace(body.Framework)
	if p.Framework != config.FrameworkNone {
		if _, ok := config.FrameworkByID(p.Framework); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("unknown framework preset %q", p.Framework)})
			return
		}
	}
	// A pointer, like Enabled: PUT is a whole-row upsert, and a plain bool would read a
	// field the caller omitted as "false" — silently unmarking a private repository on
	// any save from something that does not know about this field.
	if body.Private != nil {
		p.Private = *body.Private
	}

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

	// Was there a row here before? Asked before the upsert, because afterwards there
	// always is.
	//
	// This is what distinguishes creating a project from editing one, and only the first
	// should start a build. PUT is a whole-row upsert with no separate create, so without
	// this every save of an existing project would queue another build of its default
	// branch — a rebuild on every typo correction.
	_, existing := a.store.ProjectFor(r.Context(), p.Platform, p.Owner, p.Repo)
	isNew := errors.Is(existing, store.ErrNoProject)

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

	// A new project gets a preview of its default branch, immediately.
	//
	// Here rather than in the page, so it happens however the project was created — a
	// browser, a script, a restored configuration. Adding a project used to build only what
	// was already under review, so a repository with no open pull requests produced nothing
	// at all and the answer to "I added it, now what" was to wait for somebody to push.
	//
	// Inline rather than in a goroutine, and best-effort: it is two API calls and an enqueue,
	// which is worth the wait on project creation, and a failure here must not fail the save
	// — the project row is correct and the branch build can be asked for again from its card.
	branchNote := ""
	if isNew && p.Enabled && a.brancher != nil {
		branch, err := a.brancher(r.Context(), model.Repo{
			Platform: model.Platform(p.Platform), Owner: p.Owner, Name: p.Repo,
		}, "")
		switch {
		case err != nil:
			a.log.Warn("could not start the default branch preview",
				"project", p.Key(), "error", err)
			branchNote = err.Error()
		default:
			a.log.Info("queued a default branch preview", "project", p.Key(), "branch", branch)
		}
	}

	st, err := a.snapshot(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
		return
	}
	if branchNote != "" {
		st.Note = branchNote
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

// unlink removes a preview and stops its pull request being rediscovered.
//
// A destructive action with no undo beyond linking it again, so it is gated like every
// other write and the page asks before calling it. What comes back names both halves of
// what happened, because "removed" alone leaves the operator wondering whether the next
// push brings it back.
func (a *ProjectsAdmin) unlink(w http.ResponseWriter, r *http.Request) {
	preview := r.PathValue("preview")
	if !previewIDPattern.MatchString(preview) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "not a preview id: expected twelve hex characters, got " + preview,
		})
		return
	}
	if a.unlinker == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot unlink pull requests"})
		return
	}

	found, err := a.unlinker(r.Context(), preview)
	if err != nil {
		// The ignore is written before the teardown, so a failure here means the pull
		// request will not be rebuilt and something of the preview may remain. Say so:
		// the operator's next step is to look at the log, not to press the button again.
		a.log.Error("unlinking a preview", "preview", preview, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": err.Error() + " — the pull request will not be rebuilt, " +
				"but some of the preview may remain; see the daemon log"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "there is no preview " + preview + " any more"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unlinked": true})
}

// link builds one pull request by number, and un-ignores it if it was unlinked.
func (a *ProjectsAdmin) link(w http.ResponseWriter, r *http.Request) {
	p, bad := projectFromRequest(r)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": bad})
		return
	}
	var body struct {
		Number int `json:"number"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if body.Number < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "which pull request? give a number, as in 19"})
		return
	}
	if a.linker == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot look up a pull request by number"})
		return
	}

	repo := model.Repo{Platform: model.Platform(p.Platform), Owner: p.Owner, Name: p.Repo}
	if err := a.linker(r.Context(), repo, body.Number); err != nil {
		// 200 with an error, as scan does: the message is the operator's next step —
		// usually that the number is closed or does not exist — and this is a button on
		// a page that stays open, not a form submission.
		a.log.Warn("linking a pull request", "project", p.Key(), "number", body.Number, "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"queued": false, "error": err.Error()})
		return
	}
	a.log.Info("linked a pull request", "project", p.Key(), "number", body.Number)
	writeJSON(w, http.StatusOK, map[string]any{"queued": true, "number": body.Number})
}

// setNamePrefix changes what every published name starts with.
//
// Stored in the database rather than written back into config.yml, because that file is
// hand-written and its comments are the most valuable thing in it — a daemon that rewrote it
// to save one string would delete them.
func (a *ProjectsAdmin) setNamePrefix(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prefix string `json:"prefix"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.setPrefix == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot change the name prefix"})
		return
	}
	// Refused rather than sanitized, and refused here as well as at config load: this is
	// reachable without a browser, and a prefix that is not a hostname label produces names
	// the exposer rejects one build at a time.
	if err := a.setPrefix(r.Context(), body.Prefix); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	st, err := a.snapshot(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// branch builds a preview of a branch, with no pull request behind it.
//
// An absent or empty `branch` means the repository's default, read from the platform. That
// is the case this exists for: the operator wants "a preview of main that always works" and
// does not necessarily know whether this repository calls it `main` or `master`.
func (a *ProjectsAdmin) branch(w http.ResponseWriter, r *http.Request) {
	p, bad := projectFromRequest(r)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": bad})
		return
	}
	// An empty body is legitimate and means the default branch, so a decode failure is only
	// an error when there was something there to decode.
	var body struct {
		Branch string `json:"branch"`
	}
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if a.brancher == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot build a branch preview"})
		return
	}

	repo := model.Repo{Platform: model.Platform(p.Platform), Owner: p.Owner, Name: p.Repo}
	branch, err := a.brancher(r.Context(), repo, strings.TrimSpace(body.Branch))
	if err != nil {
		// 200 with an error, as scan and link do: the message is the operator's next step —
		// an App not installed there, a credential that cannot read the repository, an empty
		// repository with no default branch — and this is a button on a page that stays open.
		a.log.Warn("building a branch preview", "project", p.Key(), "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"queued": false, "error": err.Error()})
		return
	}
	a.log.Info("queued a branch preview", "project", p.Key(), "branch", branch)
	writeJSON(w, http.StatusOK, map[string]any{"queued": true, "branch": branch})
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

// setSCM stores a project's own source-control credential.
//
// Separate from setSecret, and separate in the vault, because these two things look alike
// and are not: a project *variable* is handed to every build of that repository, and this
// is the credential the daemon clones and comments with. Sharing the handler would mean one
// validation rule for both, and the rule that keeps the credential out of a build's
// environment is the shape of its name.
//
// PUT with an empty value deletes, so one route covers "set", "rotate" and "clear". A
// credential is a single field on a form and a DELETE route for it would be a second
// button whose only difference is that it needs no value.
func (a *ProjectsAdmin) setSCM(w http.ResponseWriter, r *http.Request) {
	p, bad := projectFromRequest(r)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": bad})
		return
	}

	name := r.PathValue("name")
	if !vault.IsProjectSCMKey(name) {
		// A closed list, so a typo cannot become a credential the daemon looks for and
		// nothing sets — which fails as a build that cannot clone, with nothing to say
		// the name was wrong.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown credential %q; expected one of %s, %s, %s",
				name, vault.SCMAccessToken, vault.SCMEmail, vault.SCMAPIToken)})
		return
	}

	v := a.openVault()
	if v == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the vault is locked; unlock it at /secrets first"})
		return
	}

	var body struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Trailing whitespace from a paste is the commonest way a token is wrong in a way
	// nothing reports.
	body.Value = strings.TrimSpace(body.Value)

	key := vault.ProjectSCMKey(p.Platform, p.Owner, p.Repo, name)
	if body.Value == "" {
		if err := v.Delete(key); err != nil {
			a.log.Error("clearing a project credential", "project", p.Key(), "name", name, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		a.log.Info("project credential cleared", "project", p.Key(), "name", name)
		a.respond(w, r)
		return
	}

	if err := v.Set(key, vault.NewSecretString(body.Value)); err != nil {
		a.log.Error("storing a project credential", "project", p.Key(), "name", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// The name and the length, never the value. No rearm: the client resolves this per
	// call, so it applies to the next delivery without a restart.
	a.log.Info("project credential stored", "project", p.Key(), "name", name, "bytes", len(body.Value))
	a.respond(w, r)
}

// testSCM checks that a project's credential can actually reach its repository.
//
// A token is pasted once and otherwise unverifiable, and both ways of getting it wrong are
// quiet: too few scopes fails twenty seconds into a clone, and read-without-write fails at
// the comment, after a successful build. So the answer is available before either.
func (a *ProjectsAdmin) testSCM(w http.ResponseWriter, r *http.Request) {
	p, bad := projectFromRequest(r)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": bad})
		return
	}
	if a.scmChecker == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot test credentials for " + p.Platform})
		return
	}

	detail, err := a.scmChecker(r.Context(), model.Repo{
		Platform: model.Platform(p.Platform), Owner: p.Owner, Name: p.Repo,
	})
	if err != nil {
		// The platform's own message, which is the useful half: "credentials lack one or
		// more required privilege scopes" names the fix in a way this code cannot.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": detail})
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
	// A dotted name is almost always somebody putting a platform credential where the build
	// variables are, and the generic message sent them back to the same box to try again.
	// `bitbucket.access_token` is not a build variable and must not become one: it is what
	// the daemon clones with, and a build that could read it is a build that could print it.
	if strings.Contains(env, ".") {
		return fmt.Sprintf("%q is not a build variable — a dotted name is a platform "+
			"credential. A Bitbucket token goes under Edit → Bitbucket credential on this "+
			"project, where it is stored for cloning and never given to a build. Build "+
			"variables are named like BB_REPO_TOKEN_ONPREM.", env)
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
