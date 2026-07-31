package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
}

func NewZrokAdmin(cfg config.Server, st *store.Store, log *slog.Logger,
	vaultOf func() (*vault.Vault, error)) *ZrokAdmin {

	return &ZrokAdmin{
		cfg:     cfg,
		store:   st,
		log:     log.With("component", "zrok"),
		vaultOf: vaultOf,
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
	return mux
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

	// Enrolled is whether *either* root has an account. It is what the panel switches on: with
	// one enrolled there is nothing to sign up for, which is the rule this surface enforces.
	Enrolled bool `json:"enrolled"`

	// HasAccountToken is whether the vault holds a token, so re-enrolling needs no email. Never
	// the token itself.
	HasAccountToken bool `json:"has_account_token"`

	// VaultLocked explains why HasAccountToken cannot be answered.
	VaultLocked bool `json:"vault_locked"`

	// ExposerKind is the configured exposer, so the panel can say that an enrolled environment
	// is not being used — `exposer.kind: local` with zrok enrolled is a working setup that
	// publishes nothing through it.
	ExposerKind string `json:"exposer_kind"`

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
	}
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
	st.InForce = expose.ZrokScopeInForce()

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
	if expose.ZrokScopeInForce() == scope {
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

// disable removes this host's environment from the account.
func (a *ZrokAdmin) disable(w http.ResponseWriter, r *http.Request) {
	if err := expose.ZrokDisable(r.Context()); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.log.Warn("the zrok environment was disabled from the dashboard")
	a.render(w, r, http.StatusOK, "The zrok environment was disabled. Every preview URL published "+
		"through it stops answering until the daemon republishes; the reserved names survive, so "+
		"the URLs come back unchanged.")
}

// enrol enables the root this process is using and records that choice.
func (a *ZrokAdmin) enrol(ctx context.Context, token vault.Secret, description string) error {
	if description == "" {
		description = "docpreview"
	}
	if err := expose.ZrokEnable(ctx, token, description); err != nil {
		return err
	}
	scope := expose.ZrokScopeInForce()
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
