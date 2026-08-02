package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/user"
	"strings"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/store"
	"github.com/netfoundry/docpreview/internal/vault"
)

// ZrokAdmin is the zrok account and environment surface on the dashboard.
//
// # Why this exists
//
// Getting from "docpreview is installed" to "a preview URL is in a pull request" needed a second
// binary, a web browser, an email, and two commands with a token pasted between them — and the
// daemon's error for every one of those going wrong was the same sentence about running
// `zrok2 enable`. This is that sequence, in the page that reports whether it worked.
//
// # What it will not do
//
// **It will not sign up twice.** A second account means a second set of reserved names, and every
// preview URL already advertised in a pull request comment lives on the first. So both roots are
// checked and either being enrolled refuses the request.
//
// **It never returns the account token.** Not on the state call, not after registering. The token
// creates and deletes every share on the account; the operator has no reason to see it, and every
// place it could be shown is a place it can leak from. It goes into the vault, and zrok's own
// environment directory keeps the copy it needs.
//
// # The gate
//
// The same one as the credential surface, and for the same reason: enrolling an environment spends
// the account's quota, and disabling one takes every published preview URL down. So it is
// admin-only, `listenersAllowAdmin` for the daemon and `isLocalRequest` for the request.
type ZrokAdmin struct {
	cfg   config.Server
	store *store.Store
	log   *slog.Logger

	// vaultOf resolves the vault per call rather than capturing it, because the vault may be
	// locked when the daemon starts and the page that unlocks it is served by this daemon. A
	// locked vault does not stop enrolment — it only means the account token cannot be kept.
	vaultOf func() (*vault.Vault, error)

	// scopeOf reports which zrok directory this process loaded.
	//
	// A field rather than a direct call to expose.ZrokScopeInForce, because that reads a
	// process-wide global which is deliberately one-way: rebinding it under a running exposer
	// would leave the daemon holding one environment while writing to another. One-way is right
	// in production and untestable, so the seam is here — and the alternative was an exported
	// reset hook, which is a production API existing only for tests.
	scopeOf func() expose.ZrokScope

	// testExposer checks one exposer's credential against the service it is for. Nil when the
	// caller wired none, in which case the panel offers no test rather than a button that
	// cannot answer.
	testExposer func(ctx context.Context, kind string) error

	// enrollZitiID turns a one-time enrolment JWT into an identity file, returning its path.
	enrollZitiID func(ctx context.Context, jwt, name string) (string, error)
}

func NewZrokAdmin(cfg config.Server, st *store.Store, log *slog.Logger,
	vaultOf func() (*vault.Vault, error)) *ZrokAdmin {

	return &ZrokAdmin{
		cfg:     cfg,
		store:   st,
		log:     log.With("component", "zrok"),
		vaultOf: vaultOf,
		scopeOf: expose.ZrokScopeInForce,
	}
}

// Available reports whether this daemon may serve the zrok surface at all.
func (a *ZrokAdmin) Available() (bool, string) {
	return listenersAllowAdmin(a.cfg.Listeners, "the zrok environment")
}

func (a *ZrokAdmin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/zrok", a.get)
	mux.HandleFunc("GET /api/zrok/{$}", a.get)
	mux.HandleFunc("POST /api/zrok/use", a.gated(a.use))
	mux.HandleFunc("POST /api/zrok/invite", a.gated(a.invite))
	mux.HandleFunc("POST /api/zrok/register", a.gated(a.register))
	mux.HandleFunc("POST /api/zrok/enable", a.gated(a.enable))
	mux.HandleFunc("POST /api/zrok/disable", a.gated(a.disable))

	// The other exposers. Under /api/zrok because this is one panel and one gate — the routes
	// were named before the panel grew from "zrok" to "how previews reach the internet", and
	// renaming them would break the one thing an operator may have scripted.
	mux.HandleFunc("POST /api/zrok/exposer", a.gated(a.setExposer))
	mux.HandleFunc("POST /api/zrok/frontdoor", a.gated(a.setFrontdoor))
	mux.HandleFunc("POST /api/zrok/frontdoor/test", a.gated(a.testFrontdoor))
	mux.HandleFunc("POST /api/zrok/ziti/enroll", a.gated(a.enrollZiti))
	return mux
}

// WithExposerTester lets the panel check a credential against the service it is for.
//
// A function rather than a call into expose, because building an exposer is wiring's job and this
// package deliberately knows nothing about how one is assembled — the same reasoning that keeps
// the SCM credential check on a callback.
func (a *ZrokAdmin) WithExposerTester(f func(ctx context.Context, kind string) error) *ZrokAdmin {
	a.testExposer = f
	return a
}

// WithZitiEnroller installs the identity enrolment, which turns a one-time JWT into an identity
// file on disk and returns where it wrote it.
func (a *ZrokAdmin) WithZitiEnroller(f func(ctx context.Context, jwt, name string) (string, error)) *ZrokAdmin {
	a.enrollZitiID = f
	return a
}

// gated refuses a write that is neither local nor from an admin session.
//
// A copy of the secrets surface's gate rather than a shared helper, because the two report
// different nouns and the reason text is the whole value of the refusal.
func (a *ZrokAdmin) gated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if roleOfContext(r.Context()) == RoleAdmin {
			next(w, r)
			return
		}
		if ok, why := a.Available(); !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": why})
			return
		}
		if ok, why := isLocalRequest(r); !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": why})
			return
		}
		next(w, r)
	}
}

// exposerInfo describes one exposer other than zrok: whether it is the one in use, whether what
// it needs is present, and what to read.
//
// Deliberately thin. These three cannot be configured from a browser — a ziti identity is a file
// produced by `ziti edge enroll`, a Frontdoor token is a vault entry, and `local` needs nothing —
// so the panel reports rather than offers. Reporting is still worth doing: without it, "which exposer is
// this daemon using, and is it ready" is answerable only from `doctor`.
type exposerInfo struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`

	// InUse is whether exposer.kind names this one.
	InUse bool `json:"in_use"`

	// Ready is whether it could publish: the credential or the identity it needs is present.
	// Meaningless for `local`, which needs nothing, so Needs is empty there.
	Ready bool `json:"ready"`

	// Needs names the missing piece, empty when nothing is missing.
	Needs string `json:"needs,omitempty"`

	// What it is, in one sentence.
	What string `json:"what"`

	// Doc is where to read more, as a path in the repository rather than a URL.
	//
	// Not a link, deliberately: the documentation site is not served by this daemon, so an
	// href would 404 from the one page it appears on. A path can be opened in an editor or
	// found on GitHub, which is what somebody configuring an exposer is doing anyway.
	Doc string `json:"doc,omitempty"`

	// Setup is what the page can offer for this exposer, empty when nothing can be done from a
	// browser. One of "frontdoor" or "ziti"; `local` needs nothing configured at all.
	Setup string `json:"setup,omitempty"`

	// Detail is what is configured, for the ones that have something to report — the Frontdoor
	// tenant, the ziti service and identity path. Never a credential.
	Detail string `json:"detail,omitempty"`
}

// otherExposers describes the three that are not zrok.
//
// The readiness checks are the cheap half of what `doctor` does — is the credential there, is the
// identity file there — and deliberately not the network half. A panel that dialled a controller
// on every page load would be a page that takes seconds to render and reports transient failures
// as configuration errors.
// effectiveZiti and effectiveFrontdoor are the config with the stored settings laid over it.
//
// The panel must report what the *next start* will use, not what this process loaded. Startup
// applies these settings once and never re-reads them — swapping an exposer under a running daemon
// would leave published previews pointing at one that no longer owns them — so a panel reading only
// `a.cfg` reports a state that is already out of date the moment anything here writes a setting.
//
// Reading only `a.cfg` after enrolling a ziti identity would still say "needs
// exposer.ziti.identity_file" and leave Enable disabled, even though the identity path was just stored
// — a deadlock, since the restart that would pick it up is only offered after enabling.
func (a *ZrokAdmin) effectiveZiti(ctx context.Context) config.ZitiConfig {
	z := a.cfg.Exposer.Ziti
	for _, f := range []struct {
		key   string
		apply func(string)
	}{
		{store.SettingZitiIdentityFile, func(v string) { z.IdentityFile = v }},
		{store.SettingZitiService, func(v string) { z.Service = v }},
		{store.SettingZitiDomain, func(v string) { z.Domain = v }},
	} {
		if v, _, err := a.store.Setting(ctx, f.key); err == nil {
			if v = strings.TrimSpace(v); v != "" {
				f.apply(v)
			}
		}
	}
	return z
}

func (a *ZrokAdmin) effectiveFrontdoor(ctx context.Context) config.FrontdoorConfig {
	fd := a.cfg.Exposer.Frontdoor
	for _, f := range []struct {
		key   string
		apply func(string)
	}{
		{store.SettingFrontdoorAPIBase, func(v string) { fd.APIBase = v }},
		{store.SettingFrontdoorFrontend, func(v string) { fd.Frontend = v }},
		{store.SettingFrontdoorEnvZID, func(v string) { fd.EnvZID = v }},
		{store.SettingFrontdoorAgentHost, func(v string) { fd.AgentReachableHost = v }},
	} {
		if v, _, err := a.store.Setting(ctx, f.key); err == nil {
			if v = strings.TrimSpace(v); v != "" {
				f.apply(v)
			}
		}
	}
	return fd
}

func (a *ZrokAdmin) otherExposers(ctx context.Context) []exposerInfo {
	kind := a.cfg.Exposer.Kind

	// Frontdoor needs five things and the credential is only one of them. Reported as the
	// *first* missing one rather than a list: a panel that names five gaps at once reads as a
	// wall, and they are filled in in this order anyway.
	fd := a.effectiveFrontdoor(ctx)
	frontdoorHasToken := false
	if v, err := a.vaultOf(); err == nil {
		if _, err := v.Get(vault.KeyFrontdoorToken); err == nil {
			frontdoorHasToken = true
		}
	}
	frontdoorNeeds := ""
	switch {
	case !frontdoorHasToken:
		frontdoorNeeds = "the API token"
	case fd.APIBase == "":
		frontdoorNeeds = "the gateway URL, including the Frontdoor id"
	case fd.Frontend == "":
		frontdoorNeeds = "the frontend id"
	case fd.EnvZID == "":
		frontdoorNeeds = "the agent's ziti identity id"
	case fd.AgentReachableHost == "":
		frontdoorNeeds = "the address the agent can reach this daemon on"
	}
	frontdoorReady := frontdoorNeeds == ""

	z := a.effectiveZiti(ctx)
	zitiNeeds := ""
	switch {
	case z.IdentityFile == "":
		zitiNeeds = "exposer.ziti.identity_file — an identity from `ziti edge enroll`"
	case z.Service == "":
		zitiNeeds = "exposer.ziti.service — the wildcard service to bind"
	case fileMissing(z.IdentityFile):
		zitiNeeds = "the identity file named in the config is not on disk: " + z.IdentityFile
	}

	frontdoorDetail := ""
	if fd.APIBase != "" {
		frontdoorDetail = fd.APIBase
		if fd.Frontend != "" {
			frontdoorDetail += ", frontend " + fd.Frontend
		}
	}

	zitiDetail := ""
	if z.Service != "" {
		zitiDetail = "service " + z.Service
		if z.Domain != "" {
			zitiDetail += ", domain " + z.Domain
		}
	}

	return []exposerInfo{
		{
			Kind: "frontdoor", Label: "NetFoundry Frontdoor",
			InUse: kind == "frontdoor",
			Ready: frontdoorReady, Needs: frontdoorNeeds,
			What: "Public preview URLs through a NetFoundry Frontdoor tenant. The agent dials " +
				"out, so this one binds a real local TCP port.",
			Doc:    "www/docs/runbooks/frontdoor.md",
			Setup:  "frontdoor",
			Detail: frontdoorDetail,
		},
		{
			Kind: "ziti", Label: "OpenZiti",
			InUse: kind == "ziti",
			Ready: zitiNeeds == "", Needs: zitiNeeds,
			What: "Previews reachable only through a tunneler with an enrolled identity. " +
				"Nothing is on the internet, and there is no clickable link in a pull request.",
			// No runbook of its own; the exposer comparison is where it is explained, and
			// `docpreview configure ziti` is what provisions a whole network.
			Doc:    "www/docs/exposers.md",
			Setup:  "ziti",
			Detail: zitiDetail,
		},
		{
			Kind: "local", Label: "This daemon only",
			InUse: kind == "local",
			// Ready unconditionally: it serves previews from the listener this page arrived
			// on, so if you are reading this it works.
			Ready: true,
			What: "Previews served from the daemon's own listener, under /preview/. No account " +
				"and no tunnel — and no URL that works from anywhere else.",
			// Nothing to set up. Its only setting is being the exposer, which every section
			// offers through the same control.
		},
	}
}

// fileMissing reports whether a configured path is absent, treating an unreadable path as absent
// too: either way the exposer cannot start.
func fileMissing(path string) bool {
	if path == "" {
		return true
	}
	_, err := os.Stat(path)
	return err != nil
}

// exposerFrontdoor and exposerZiti are the settable, non-secret parts of those two exposers.
type exposerFrontdoor struct {
	APIBase   string `json:"api_base,omitempty"`
	Frontend  string `json:"frontend,omitempty"`
	EnvZID    string `json:"env_z_id,omitempty"`
	AgentHost string `json:"agent_reachable_host,omitempty"`

	// HasToken says whether the credential is stored, without saying anything about it.
	HasToken bool `json:"has_token"`
}

type exposerZiti struct {
	IdentityFile string `json:"identity_file,omitempty"`
	Service      string `json:"service,omitempty"`
	Domain       string `json:"domain,omitempty"`
}

// zrokState is what the panel renders.
type zrokState struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	// CanWrite mirrors the credential surface: a reader who cannot change anything still sees
	// which environment is in use, because a panel that vanishes reads as a broken feature.
	CanWrite    bool   `json:"can_write"`
	ReadOnlyWhy string `json:"read_only_why,omitempty"`

	// InForce is the scope this process actually loaded, which can differ from Stored on a
	// daemon that has not been restarted since the choice changed. Both are shown, because "I
	// changed it and nothing happened" is the question that difference answers.
	InForce expose.ZrokScope `json:"in_force,omitempty"`
	Stored  expose.ZrokScope `json:"stored,omitempty"`

	System  expose.ZrokRootInfo `json:"system"`
	Project expose.ZrokRootInfo `json:"project"`

	MustChoose bool `json:"must_choose"`

	// RunAs is the account this daemon runs as, which is what makes the machine-wide
	// environment the machine-wide environment.
	//
	// `~/.zrok2` is per *user*, not per machine. A daemon under a service account with its own home
	// directory can report nothing enrolled while `zrok2 overview` in the operator's own terminal shows
	// a working environment, and both statements are true. So the panel names the account.
	RunAs string `json:"run_as,omitempty"`

	// Enrolled is whether *either* root has an account. It is what the panel switches on: with
	// one enrolled there is nothing to sign up for, which is the rule this surface enforces.
	Enrolled bool `json:"enrolled"`

	// HasAccountToken is whether the vault holds a token, so re-enrolling needs no email. Never
	// the token itself.
	HasAccountToken bool `json:"has_account_token"`

	// VaultLocked explains why HasAccountToken cannot be answered.
	VaultLocked bool `json:"vault_locked"`

	// ExposerKind is the exposer this process is actually publishing with.
	//
	// The one loaded at startup, not the one most recently chosen. Those differ for as long as it
	// takes somebody to restart, and the difference is the whole reason the next field exists:
	// pressing Enable and seeing nothing move reads as a button that does nothing.
	ExposerKind string `json:"exposer_kind"`

	// ExposerStored is the exposer recorded from the dashboard, empty when the config file is
	// still deciding.
	//
	// Reported separately so the panel can say "enabled at the next restart" rather than either
	// lying about the current state or appearing inert. Exactly the shape the zrok directory
	// choice needed, arriving from the other direction.
	ExposerStored string `json:"exposer_stored,omitempty"`

	// Frontdoor and Ziti are the non-secret half of those exposers' configuration, so the forms
	// on the panel can show what is already set rather than presenting five empty boxes on a
	// working installation.
	//
	// No credential in either. Frontdoor's API token is a vault entry and never leaves it; what
	// is here is a gateway URL, two ids and an address, all of which the operator reads off a
	// console and none of which is a secret.
	Frontdoor exposerFrontdoor `json:"frontdoor"`
	Ziti      exposerZiti      `json:"ziti"`

	// Others is every exposer this daemon is not using, and what each would need.
	//
	// Here because the panel is "exposer configuration" rather than "zrok": the question an
	// operator arrives with is how previews get out, and a page that only ever mentions zrok
	// answers that for one of four choices. Only zrok can be configured from here — the rest
	// report what they need and where the runbook is, which is more than the page said before.
	Others []exposerInfo `json:"others"`

	// DefaultAPIEndpoint is the hosted service, shown as the placeholder on the signup form so
	// nobody has to know it.
	DefaultAPIEndpoint string `json:"default_api_endpoint"`
}

func (a *ZrokAdmin) snapshot(r *http.Request) (zrokState, error) {
	ok, why := a.Available()
	st := zrokState{
		Available:          ok,
		Reason:             why,
		ExposerKind:        a.cfg.Exposer.Kind,
		DefaultAPIEndpoint: expose.DefaultZrokAPIEndpoint,
		Others:             a.otherExposers(r.Context()),
	}

	if stored, _, err := a.store.Setting(r.Context(), store.SettingExposerKind); err == nil {
		st.ExposerStored = strings.TrimSpace(stored)
	}

	fd := a.effectiveFrontdoor(r.Context())
	st.Frontdoor = exposerFrontdoor{
		APIBase:   fd.APIBase,
		Frontend:  fd.Frontend,
		EnvZID:    fd.EnvZID,
		AgentHost: fd.AgentReachableHost,
	}
	if v, err := a.vaultOf(); err == nil {
		if _, err := v.Get(vault.KeyFrontdoorToken); err == nil {
			st.Frontdoor.HasToken = true
		}
	}
	z := a.effectiveZiti(r.Context())
	st.Ziti = exposerZiti{IdentityFile: z.IdentityFile, Service: z.Service, Domain: z.Domain}
	st.CanWrite, st.ReadOnlyWhy = isLocalRequest(r)
	if !ok {
		st.CanWrite = false
		st.ReadOnlyWhy = why
	}

	stored, err := a.storedScope(r.Context())
	if err != nil {
		return st, err
	}
	st.Stored = stored
	st.InForce = a.scopeOf()
	if u, err := user.Current(); err == nil {
		st.RunAs = u.Username
	}

	state, err := expose.InspectZrokRoots(a.cfg.ZrokDir(), stored)
	if err != nil {
		return st, err
	}
	st.System = state.System
	st.Project = state.Project
	st.MustChoose = state.MustChoose
	st.Enrolled = state.System.Enabled || state.Project.Enabled

	if v, err := a.vaultOf(); err != nil {
		st.VaultLocked = true
	} else if _, err := v.Get(vault.KeyZrokAccountToken); err == nil {
		st.HasAccountToken = true
	}
	return st, nil
}

func (a *ZrokAdmin) storedScope(ctx context.Context) (expose.ZrokScope, error) {
	v, _, err := a.store.Setting(ctx, store.SettingZrokScope)
	if err != nil {
		return "", err
	}
	return expose.ZrokScope(strings.TrimSpace(v)), nil
}

func (a *ZrokAdmin) get(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "")
}

// render answers with the current state plus an optional note.
//
// Every mutation replies with the whole state rather than with "ok", so the panel never has to
// guess what changed — the same shape the credential surface uses.
func (a *ZrokAdmin) render(w http.ResponseWriter, r *http.Request, code int, note string) {
	st, err := a.snapshot(r)
	if err != nil {
		a.log.Error("building the zrok state", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "could not read the zrok environment: " + err.Error()})
		return
	}
	out := struct {
		zrokState
		Note string `json:"note,omitempty"`
	}{zrokState: st, Note: note}
	writeJSON(w, code, out)
}

// use records which environment the daemon should adopt.
//
// It does not switch this process. zrok's root directory is a process-wide global read by every
// LoadRoot, and rebinding it under a running exposer would leave the daemon holding one
// environment while writing to another. So the answer says a restart is needed, and the panel
// shows stored and in-force side by side until it happens.
func (a *ZrokAdmin) use(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the request"})
		return
	}
	scope := expose.ZrokScope(strings.ToLower(strings.TrimSpace(body.Scope)))
	if scope != expose.ZrokSystem && scope != expose.ZrokProject {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("%q is not a zrok environment: use %q or %q",
				body.Scope, expose.ZrokSystem, expose.ZrokProject)})
		return
	}

	if err := a.store.SetSetting(r.Context(), store.SettingZrokScope, string(scope)); err != nil {
		a.log.Error("recording the zrok environment choice", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "could not record the choice: " + err.Error()})
		return
	}
	a.log.Info("zrok environment choice recorded", "scope", scope)

	note := fmt.Sprintf("Using the %s zrok environment from the next restart.", scope)
	if a.scopeOf() == scope {
		note = fmt.Sprintf("Using the %s zrok environment.", scope)
	}
	a.render(w, r, http.StatusOK, note)
}

// invite asks zrok to email a registration link.
func (a *ZrokAdmin) invite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email       string `json:"email"`
		APIEndpoint string `json:"api_endpoint"`
		InviteToken string `json:"invite_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the request"})
		return
	}
	if strings.TrimSpace(body.Email) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no email address"})
		return
	}
	if err := a.refuseIfEnrolled(r); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	if err := expose.ZrokInvite(r.Context(), body.APIEndpoint, body.Email, body.InviteToken); err != nil {
		a.log.Warn("zrok refused an invite", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.log.Info("zrok invite requested", "email", body.Email)
	a.render(w, r, http.StatusOK, "zrok is emailing "+body.Email+
		" a registration link. Open it, then paste the link below.")
}

// register turns the emailed link into an account and enrols this host.
//
// Both halves in one call because the account token exists only in the register response: a call
// that created the account and stopped would leave a credential in a JSON body, which is the one
// place this surface must never put it.
func (a *ZrokAdmin) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Link        string `json:"link"`
		Password    string `json:"password"`
		APIEndpoint string `json:"api_endpoint"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the request"})
		return
	}
	if err := a.refuseIfEnrolled(r); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	token, err := expose.ZrokRegister(r.Context(), body.APIEndpoint, body.Link, body.Password)
	if err != nil {
		a.log.Warn("zrok refused a registration", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.log.Info("zrok account created")

	note := "The zrok account was created. "
	stored := a.keepAccountToken(token)
	if !stored {
		note += "The vault is locked, so the account token could not be stored — " +
			"zrok keeps its own copy and previews still work. "
	}

	if err := a.enrol(r.Context(), token, body.Description); err != nil {
		// The account exists whatever happens here, and saying so is the difference between
		// "try again" and "you now have an account you cannot use".
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "the account was created but this host could not be enrolled: " + err.Error()})
		return
	}
	a.render(w, r, http.StatusOK, note+"This host is enrolled.")
}

// enable enrols this host against a token the operator already has.
//
// The token comes from the request when one is given, and from the vault otherwise — the second is
// the path after a registration, and after moving the installation to another machine.
func (a *ZrokAdmin) enable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountToken string `json:"account_token"`
		Description  string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the request"})
		return
	}

	var token vault.Secret
	if strings.TrimSpace(body.AccountToken) != "" {
		token = vault.NewSecretString(strings.TrimSpace(body.AccountToken))
		a.keepAccountToken(token)
	} else {
		v, err := a.vaultOf()
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "the vault is locked and no account token was given; " +
					"unlock it or paste the token"})
			return
		}
		token, err = v.MustGet(vault.KeyZrokAccountToken)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "no zrok account token is stored; paste one, or sign up above"})
			return
		}
	}

	if err := a.enrol(r.Context(), token, body.Description); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.render(w, r, http.StatusOK, "This host is enrolled.")
}

// disable removes this installation's own zrok environment from the account.
//
// **Only this installation's.** The machine-wide `~/.zrok2` belongs to the zrok CLI and to
// whatever else that account is used for — a share somebody left running, another tool, a
// colleague's scripts. Deleting it from a documentation-preview dashboard would take those with
// it, and nobody would think to look here for the cause. `zrok2 disable` is the tool for that,
// run by the person who knows what depends on it.
//
// Enforced here and not only hidden on the page, because a hidden button is a preference and this
// is a rule: the request is refused whatever sends it.
func (a *ZrokAdmin) disable(w http.ResponseWriter, r *http.Request) {
	if a.scopeOf() == expose.ZrokSystem {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this daemon is using the machine's zrok environment, which is shared " +
				"with the zrok CLI and with anything else on that account. docpreview will " +
				"not remove it — run 'zrok2 disable' if that is what you want"})
		return
	}
	if err := expose.ZrokDisable(r.Context()); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.log.Warn("the zrok environment was disabled from the dashboard")
	a.render(w, r, http.StatusOK, "The zrok environment was disabled. Every preview URL published "+
		"through it stops answering until the daemon republishes; the reserved names survive, so "+
		"the URLs come back unchanged.")
}

// setExposer records which exposer this daemon should use.
//
// Stored rather than written into the config file, which is hand-written and whose comments are the
// part that survives being copied to another machine. Read once at startup, because the exposer is
// built during wiring and swapping one under a running daemon would leave every published preview
// pointing at an exposer that no longer owns it.
//
// It refuses an exposer that cannot work. Recording one that has no credential produces a daemon
// that will not start, and the page that would fix it is served by that daemon — the boot-order
// trap this codebase keeps rediscovering, arriving this time through a dropdown.
func (a *ZrokAdmin) setExposer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the request"})
		return
	}
	kind := strings.ToLower(strings.TrimSpace(body.Kind))

	switch kind {
	case "zrok2":
		st, err := a.snapshot(r)
		if err == nil && !st.Enrolled {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "no zrok environment is enrolled yet, so this daemon would not start; " +
					"set one up first"})
			return
		}
	case "frontdoor", "ziti":
		for _, o := range a.otherExposers(r.Context()) {
			if o.Kind == kind && o.Needs != "" {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": kind + " needs " + o.Needs + "; this daemon would not start"})
				return
			}
		}
	case "local":
		// Nothing to check. It serves previews from the listener this request arrived on.
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("%q is not an exposer: zrok2, frontdoor, ziti or local", body.Kind)})
		return
	}

	if err := a.store.SetSetting(r.Context(), store.SettingExposerKind, kind); err != nil {
		a.log.Error("recording the exposer", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "could not record it: " + err.Error()})
		return
	}
	a.log.Info("exposer recorded from the dashboard", "exposer", kind, "was", a.cfg.Exposer.Kind)

	note := "Previews will be published with " + kind + " from the next restart."
	if kind == a.cfg.Exposer.Kind {
		note = "Already publishing with " + kind + "."
	}
	a.render(w, r, http.StatusOK, note)
}

// setFrontdoor stores the Frontdoor API token.
//
// A field here as well as under Secrets, because the operator setting up an exposer is on this
// panel and sending them to a different one for the credential it needs is how a setup flow loses
// people. It is the same vault entry either way.
func (a *ZrokAdmin) setFrontdoor(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token     string `json:"token"`
		APIBase   string `json:"api_base"`
		Frontend  string `json:"frontend"`
		EnvZID    string `json:"env_z_id"`
		AgentHost string `json:"agent_reachable_host"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the request"})
		return
	}

	// The token, when one was typed. Blank means "leave what is there", which is what makes the
	// form re-submittable to correct one of the other four without re-pasting a credential.
	if token := strings.TrimSpace(body.Token); token != "" {
		v, err := a.vaultOf()
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "the vault is locked, so the token cannot be stored; unlock it above"})
			return
		}
		if err := v.Set(vault.KeyFrontdoorToken, vault.NewSecretString(token)); err != nil {
			a.log.Error("storing the Frontdoor token", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		a.log.Info("Frontdoor API token stored from the dashboard")
	}

	// The four settings. Written individually rather than as a blob so that clearing one is
	// possible and a partial form does not wipe the rest.
	for key, val := range map[string]string{
		store.SettingFrontdoorAPIBase:   strings.TrimSpace(body.APIBase),
		store.SettingFrontdoorFrontend:  strings.TrimSpace(body.Frontend),
		store.SettingFrontdoorEnvZID:    strings.TrimSpace(body.EnvZID),
		store.SettingFrontdoorAgentHost: strings.TrimSpace(body.AgentHost),
	} {
		if val == "" {
			continue
		}
		if err := a.store.SetSetting(r.Context(), key, val); err != nil {
			a.log.Error("recording a Frontdoor setting", "key", key, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "could not record " + key + ": " + err.Error()})
			return
		}
	}

	a.render(w, r, http.StatusOK, "Saved. These take effect at the next restart — test the "+
		"token first, since a Frontdoor that refuses it stops every publish.")
}

// testFrontdoor asks the Frontdoor tenant whether the stored token works.
//
// Worth a round trip where the zrok panel deliberately avoids one: this is a button somebody
// pressed, not a page load, and the alternative to answering now is discovering the token is wrong
// from a build that clones for twenty seconds and then fails to publish.
func (a *ZrokAdmin) testFrontdoor(w http.ResponseWriter, r *http.Request) {
	if a.testExposer == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot test an exposer credential"})
		return
	}
	if err := a.testExposer(r.Context(), "frontdoor"); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.render(w, r, http.StatusOK, "Frontdoor accepted the token.")
}

// enrollZiti turns a one-time enrolment JWT into an identity file.
//
// The equivalent of `ziti edge enroll`, through the ziti SDK rather than a second binary. The token
// is one-time in the strict sense: the controller consumes it, and the private key generated during
// enrolment exists only in the file this writes. So a failure after the controller has accepted it
// is unrecoverable, which is why the file is written before anything else is reported.
func (a *ZrokAdmin) enrollZiti(w http.ResponseWriter, r *http.Request) {
	if a.enrollZitiID == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this daemon cannot enrol a ziti identity"})
		return
	}
	var body struct {
		JWT     string `json:"jwt"`
		Name    string `json:"name"`
		Service string `json:"service"`
		Domain  string `json:"domain"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the request"})
		return
	}
	if strings.TrimSpace(body.JWT) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "no enrolment token; paste the JWT from `ziti edge create identity`"})
		return
	}

	// Recovered, because this one calls into a third-party SDK that panics on bad input rather than
	// returning an error — `enroll.Enroll` with no KeyAlg reaches a switch with no default, and an
	// unrecovered panic would take the request down with a stack trace in the log and no response at
	// all. A panic here is a bug, and the operator should be told which bug rather than watching the
	// connection close.
	path, err := func() (p string, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				a.log.Error("the ziti enrolment panicked", "panic", rec)
				err = fmt.Errorf("the enrolment failed inside the ziti SDK: %v "+
					"(this is a bug in docpreview, and the token may now be spent)", rec)
			}
		}()
		return a.enrollZitiID(r.Context(), body.JWT, strings.TrimSpace(body.Name))
	}()
	if err != nil {
		a.log.Warn("ziti enrolment failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.log.Info("ziti identity enrolled from the dashboard", "file", path)

	// The service and domain are config, not credentials, and they are what the exposer needs
	// besides the identity. Stored so the enrolment is a complete act rather than one that ends
	// in "now edit config.yml".
	for key, val := range map[string]string{
		store.SettingZitiIdentityFile: path,
		store.SettingZitiService:      strings.TrimSpace(body.Service),
		store.SettingZitiDomain:       strings.TrimSpace(body.Domain),
	} {
		if val == "" {
			continue
		}
		if err := a.store.SetSetting(r.Context(), key, val); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "the identity enrolled but " + key + " could not be recorded: " + err.Error()})
			return
		}
	}

	a.render(w, r, http.StatusOK, "The identity is enrolled at "+path+
		". Select OpenZiti as the exposer, then restart.")
}

// enrol enables the root this process is using and records that choice.
func (a *ZrokAdmin) enrol(ctx context.Context, token vault.Secret, description string) error {
	if description == "" {
		description = "docpreview"
	}
	if err := expose.ZrokEnable(ctx, token, description); err != nil {
		return err
	}
	scope := a.scopeOf()
	if scope == "" {
		// Nothing called UseZrokRoot, which for the daemon cannot happen — setup does it before
		// the exposer exists. Recorded as the project root rather than left blank, because a
		// blank setting means "decide at startup" and the decision has now been made.
		scope = expose.ZrokProject
	}
	if err := a.store.SetSetting(ctx, store.SettingZrokScope, string(scope)); err != nil {
		return fmt.Errorf("enrolled, but the choice could not be recorded: %w", err)
	}
	a.log.Info("zrok environment enrolled", "scope", scope)
	return nil
}

// refuseIfEnrolled is the "do not sign up twice" rule.
func (a *ZrokAdmin) refuseIfEnrolled(r *http.Request) error {
	stored, err := a.storedScope(r.Context())
	if err != nil {
		return err
	}
	state, err := expose.InspectZrokRoots(a.cfg.ZrokDir(), stored)
	if err != nil {
		return err
	}
	switch {
	case state.Project.Enabled:
		return errors.New("this installation already has a zrok environment; " +
			"there is nothing to sign up for")
	case state.System.Enabled:
		return errors.New("this machine already has a zrok environment. Use it with " +
			"\"Use the machine's\", or disable it first if docpreview should have its own account")
	}
	return nil
}

// keepAccountToken stores the token when the vault is open, reporting whether it did.
//
// A locked vault is not an error here. The account exists by the time this is called, and refusing
// to continue would leave one whose token is in no readable place at all.
func (a *ZrokAdmin) keepAccountToken(token vault.Secret) bool {
	v, err := a.vaultOf()
	if err != nil {
		a.log.Warn("could not store the zrok account token: the vault is locked")
		return false
	}
	if err := v.Set(vault.KeyZrokAccountToken, token); err != nil {
		a.log.Error("could not store the zrok account token", "error", err)
		return false
	}
	return true
}
