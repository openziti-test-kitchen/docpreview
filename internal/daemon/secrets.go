package daemon

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/vault"
)

// SecretsAdmin is the credential surface behind the setup page.
//
// It exists because the alternative was a three-command terminal ceremony —
// mint a master key, remember to persist it, pipe a value into `vault set` —
// performed in the right order before anything worked. That is a bad first
// five minutes, and the failure mode of getting it wrong is a daemon that
// starts and then cannot open its own vault.
//
// # Why this is allowed to exist at all
//
// The design note in docs/design/05-secrets.md says a credential-write endpoint
// on an unauthenticated surface is worse than no UI. That still holds, so this
// is gated: every listener must be loopback. See Available.
//
// On a loopback-only daemon the boundary is the same one that already protects
// `docpreview vault set` — anyone who can reach 127.0.0.1 can run the binary.
// This adds no reachability that a shell did not already have. The moment a
// listener is not loopback, it does, and it refuses to serve.
//
// # What it never does
//
// Read a value back. Not on list, not on error. The single exception is
// generate, which returns what it just minted in the response to the call that
// minted it — see the comment there for why that is not the same thing.
type SecretsAdmin struct {
	path string
	cfg  config.Server
	log  *slog.Logger

	// rearm re-resolves everything derived from vault contents: the redactor,
	// which is compiled from the secret values, and the GitHub client, which
	// reads its private key and webhook secret at construction. A secret changed
	// at runtime that does not rearm is a secret that appears verbatim in the
	// next build log, or a rotation that silently does not take effect.
	//
	// The argument is the key that changed, empty when the vault itself was just
	// unlocked and every key is new at once.
	rearm func(changed string)

	mu sync.Mutex
	v  *vault.Vault // nil until unlocked
}

func NewSecretsAdmin(cfg config.Server, log *slog.Logger, rearm func(changed string)) *SecretsAdmin {
	a := &SecretsAdmin{
		path:  cfg.VaultPath(),
		cfg:   cfg,
		log:   log.With("component", "secrets"),
		rearm: rearm,
	}
	// If a key source or the environment already supplies a key, the vault is
	// open from the start and the page skips straight to the secrets. Nothing
	// here fails: no key is the ordinary state of a daemon waiting to be
	// unlocked, which is what this surface is for.
	if v, err := vault.OpenFrom(a.path, cfg.KeySource()); err == nil {
		a.v = v
	}
	return a
}

// isLocalRequest reports whether a request came from this machine, with nothing
// in between.
//
// Available checks where the daemon *listens*; this checks where a request
// actually *came from*. Both are needed and neither substitutes for the other.
// The gap between them is a tunnel: `zrok2 share public http://127.0.0.1:8471`
// makes every route internet-reachable while the listener is still loopback, so
// Available says yes and the credential API is on the internet.
//
// Two conditions, and the second is the one that closes that gap:
//
//   - RemoteAddr must be a loopback address. Under a tunnel it is — the daemon
//     sees the connection from the local tunnel process — so this alone is not
//     enough.
//   - The request must carry no forwarding header. Anything that proxies to the
//     daemon adds `X-Forwarded-For` or `Forwarded`, including docpreview's own
//     `webhook-only` proxy. A caller can add one itself, but that only ever
//     makes this stricter, which is the right direction to be wrong in.
//
// A tunnel that strips forwarding headers would defeat this. That is why the
// recommended arrangement is to tunnel the `webhook-only` proxy rather than the
// daemon: then the credential API is not reachable at all and this check is a
// second line rather than the only one.
func isLocalRequest(r *http.Request) (bool, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isLoopback(host) {
		return false, "credentials can only be managed from the machine running docpreview, " +
			"and this request came from " + host
	}
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return false, "this request was forwarded by a proxy (" + h + " is set), so it did not " +
				"originate on this machine; credentials can only be managed locally"
		}
	}
	return true, ""
}

// Available reports whether this daemon may serve the secrets surface at all.
//
// Every listener must be loopback. A ziti listener is arguably a stronger
// boundary than loopback — the overlay authenticates the dialer — but the
// admin surface does not yet check the dialing identity, so "enrolled at all"
// would be the whole authorization. That is not enough for credential writes.
//
// This is a property of the daemon, not of a request. See isLocalRequest for the
// per-request half, and why one without the other is not sufficient.
func (a *SecretsAdmin) Available() (bool, string) {
	if len(a.cfg.Listeners) == 0 {
		return false, "no listeners"
	}
	for _, l := range a.cfg.Listeners {
		if l.Ziti != nil {
			return false, "a ziti listener is configured; the admin surface does not yet " +
				"check the dialing identity, so credential writes are refused"
		}
		host, _, err := net.SplitHostPort(l.TCP)
		if err != nil {
			return false, "unparseable listener " + l.TCP
		}
		if !isLoopback(host) {
			return false, fmt.Sprintf("the ingress listens on %s; secrets can only be managed "+
				"from a loopback-only daemon", l.TCP)
		}
	}
	return true, ""
}

func vaultExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// state is what the page renders. Names and flags; never a value.
type secretsState struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	// CanWrite is whether this caller may change anything. False renders the
	// panel read-only rather than hiding it: someone looking at the dashboard
	// from another machine should be able to see that two credentials are set
	// without being offered buttons that will 403.
	CanWrite    bool   `json:"can_write"`
	ReadOnlyWhy string `json:"read_only_why,omitempty"`

	Locked    bool         `json:"locked"`
	VaultPath string       `json:"vault_path"`
	HasVault  bool         `json:"has_vault"`
	Entries   []secretView `json:"entries"`
}

// Secret groups, which are a security boundary and not a display preference.
//
// GroupDaemon holds what docpreview uses to talk to a platform or an exposer: a
// GitHub App private key, a Bitbucket access token, a webhook secret. **None of them
// ever reaches a build.** GroupBuild is the opposite — every one of those is injected
// into every build as an environment variable and is readable by whatever the pull
// request's own build script chooses to do.
//
// One flat list put a GitHub App private key three rows above a Docusaurus API key
// with nothing between them, which is how six tokens came to be stored in the belief
// that storing them was enough. The rule that separates them is
// vault.IsBuildEnvKey: shell-shaped names are build variables, dotted names are the
// daemon's own.
const (
	GroupDaemon = "daemon"
	GroupBuild  = "build"
	GroupUnused = "unused"
)

// secretView is one row: what it is for, and whether it is set.
type secretView struct {
	Key      string `json:"key"`
	Set      bool   `json:"set"`
	Label    string `json:"label"`
	Hint     string `json:"hint"`
	Required bool   `json:"required"`
	// EnvVar is set for build secrets: the variable this value is injected as.
	EnvVar string `json:"env_var,omitempty"`

	// Group is which section this row belongs in. See the constants above: the
	// distinction is what reads the value, which is why it comes from the server
	// rather than being guessed at from the key's shape by the page.
	Group string `json:"group"`
}

// snapshot describes the vault for the given request.
//
// It takes the request because read-only-ness is a property of the caller, not
// of the daemon: the same vault is writable from this machine and not from
// anywhere else, and the page has to be told which it is looking at.
func (a *SecretsAdmin) snapshot(r *http.Request) secretsState {
	ok, why := a.Available()

	a.mu.Lock()
	v := a.v
	a.mu.Unlock()

	st := secretsState{
		Available: ok,
		Reason:    why,
		Locked:    v == nil,
		VaultPath: a.path,
		HasVault:  vaultExists(a.path),
	}

	// Mirror what gated will actually decide, so the page never offers an action
	// that is going to 403.
	switch local, lwhy := isLocalRequest(r); {
	case !ok:
		st.CanWrite, st.ReadOnlyWhy = false, why
	case !local:
		st.CanWrite, st.ReadOnlyWhy = false, lwhy
	default:
		st.CanWrite = true
	}
	if v == nil {
		return st
	}

	have := map[string]bool{}
	for _, k := range v.Keys() {
		have[k] = true
	}

	// The known keys first, in a fixed order, so the page reads as a checklist
	// rather than a dump. A setup screen that lists what you have is less
	// useful than one that lists what you still need.
	for _, k := range knownKeys() {
		st.Entries = append(st.Entries, secretView{
			Key: k.key, Set: have[k.key], Label: k.label, Hint: k.hint,
			Required: k.required(a.cfg), Group: GroupDaemon,
		})
		delete(have, k.key)
	}

	// Then anything build.secrets references, so a missing one is visible here
	// rather than only as a startup failure.
	var envKeys []string
	for env := range a.cfg.Build.Secrets {
		envKeys = append(envKeys, env)
	}
	sort.Strings(envKeys)
	for _, env := range envKeys {
		key := a.cfg.Build.Secrets[env]
		st.Entries = append(st.Entries, secretView{
			Key: key, Set: have[key], Label: env, Required: true, EnvVar: env,
			Group: GroupBuild,
			Hint:  "injected into every build as " + env + ", and redacted from every log",
		})
		delete(have, key)
	}

	// Then whatever else is in there, and each one says what it is for.
	//
	// A shell-shaped key is injected into every build under its own name — that is the
	// rule vault.IsBuildEnvKey states, and saying so here is not decoration. Without it
	// this list was a set of names with no indication whether any of them did anything,
	// which is exactly how six tokens came to be stored in the belief that storing them
	// was enough.
	var rest []string
	for k := range have {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, k := range rest {
		view := secretView{Key: k, Set: true, Label: k, Group: GroupBuild}
		if vault.IsBuildEnvKey(k) {
			// EnvVar alone. The page turns this into a one-word chip and states the rule
			// once above the list: as a per-row sentence it wrapped to three lines on every
			// entry, and a list of six tokens became a wall of the same paragraph repeated.
			view.EnvVar = k
		} else {
			// Not shell-shaped, not mapped by build.secrets, and not one of the known
			// keys: nothing reads it. Better said than left looking configured — and
			// its own section, because a dead key listed among live ones is the thing
			// somebody skims past.
			view.Group = GroupUnused
			view.Hint = "nothing on this daemon reads this key. A build variable has to be " +
				"named like one — upper case, digits and underscore."
		}
		st.Entries = append(st.Entries, view)
	}

	return st
}

type knownKey struct {
	key, label, hint string
	required         func(config.Server) bool
}

func knownKeys() []knownKey {
	usingGitHub := func(c config.Server) bool { return c.GitHub.AppID != 0 }
	return []knownKey{
		{vault.KeyGitHubPrivateKey, "GitHub App private key",
			"the .pem GitHub generated — paste the whole file, BEGIN line included", usingGitHub},
		{vault.KeyGitHubWebhookSec, "GitHub webhook secret",
			"the value in the App's Webhook secret field", usingGitHub},
		{vault.KeyFrontdoorToken, "Frontdoor API token",
			"only for exposer.kind: frontdoor",
			func(c config.Server) bool { return c.Exposer.Kind == "frontdoor" }},

		// Bitbucket. Listed even when it is not enabled, so the Generate button for
		// the webhook secret exists before the operator has committed to the config —
		// the secret has to be generated *first* and pasted into Bitbucket's form,
		// and a key that is not listed has no button.
		{vault.KeyBitbucketHookSec, "Bitbucket webhook secret",
			"generate it here, then paste the same value into Repository settings → Webhooks",
			usingBitbucket},
		{vault.KeyBitbucketAccessToken, "Bitbucket access token",
			"a repository, project or workspace access token — scopes: repository, pullrequest:write",
			func(c config.Server) bool {
				return usingBitbucket(c) && c.Bitbucket.Auth != config.BitbucketAuthAPIToken
			}},
		{vault.KeyBitbucketEmail, "Bitbucket account email",
			"only for bitbucket.auth: api_token — the account the API token belongs to",
			func(c config.Server) bool {
				return usingBitbucket(c) && c.Bitbucket.Auth == config.BitbucketAuthAPIToken
			}},
		{vault.KeyBitbucketAPIToken, "Bitbucket API token",
			"only for bitbucket.auth: api_token",
			func(c config.Server) bool {
				return usingBitbucket(c) && c.Bitbucket.Auth == config.BitbucketAuthAPIToken
			}},
	}
}

func usingBitbucket(c config.Server) bool { return c.Bitbucket.Enabled }

// Handler routes the secrets API.
//
// Absolute patterns, not StripPrefix. Stripping "/api/secrets" from a request
// for exactly "/api/secrets" leaves an empty path, which ServeMux answers with
// a 307 to "/" — so the listing endpoint redirected to the dashboard instead of
// returning anything.
func (a *SecretsAdmin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/secrets", a.get)
	mux.HandleFunc("GET /api/secrets/{$}", a.get)
	mux.HandleFunc("POST /api/secrets/unlock", a.gated(a.unlock))
	mux.HandleFunc("PUT /api/secrets/{key}", a.gated(a.put))
	mux.HandleFunc("POST /api/secrets/{key}/generate", a.gated(a.generate))
	mux.HandleFunc("DELETE /api/secrets/{key}", a.gated(a.del))
	return mux
}

// generate mints a value, stores it, and returns it — once.
//
// This is the only endpoint that ever emits a secret, and it is not a read: it
// returns what it just created, in the response to the call that created it,
// and there is no way to ask for it again.
//
// It exists because the alternative did not work. A webhook secret has to be
// identical in two places — GitHub's form and this vault — and a UI that can
// only accept values requires the operator to produce one elsewhere, keep it on
// a clipboard across an app-creation flow, and paste it twice. Told to "paste
// the value you generated earlier", the honest answer is "from where?".
//
// So: press a button, copy what appears, paste it into GitHub. The same shape
// GitHub itself uses for personal access tokens, for the same reason.
func (a *SecretsAdmin) generate(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !validKey(key) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a key is letters, digits, dot, dash and underscore"})
		return
	}

	a.mu.Lock()
	v := a.v
	a.mu.Unlock()
	if v == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the vault is locked"})
		return
	}

	// 32 bytes from crypto/rand. GitHub's own guidance is "a random string with
	// high entropy"; this is the same size as the HMAC-SHA256 it keys.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	value := base64.StdEncoding.EncodeToString(buf)

	if err := v.Set(key, vault.NewSecretString(value)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := v.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	a.rearm(key)
	a.log.Info("secret generated", "key", key)

	writeJSON(w, http.StatusOK, struct {
		secretsState
		// Named to make the contract obvious at the call site: this field is
		// present exactly once, in this response, and nowhere else.
		ShownOnce string `json:"shown_once"`
	}{a.snapshot(r), value})
}

// gated refuses every mutating call unless the daemon is loopback-only *and*
// this particular request came from the machine itself.
//
// Both, because they fail in different ways. Available is about configuration
// and catches a daemon bound to 0.0.0.0. isLocalRequest is about this request
// and catches a tunnel, which Available cannot see at all.
//
// The read path is deliberately not gated: it returns no values, and a page that
// cannot explain why it is read-only is worse than one that can.
func (a *SecretsAdmin) gated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, why := a.Available(); !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": why})
			return
		}
		if ok, why := isLocalRequest(r); !ok {
			a.log.Warn("refused a remote credential request",
				"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": why})
			return
		}
		next(w, r)
	}
}

func (a *SecretsAdmin) get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.snapshot(r))
}

func (a *SecretsAdmin) unlock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	v, err := vault.OpenWithKey(a.path, body.Key)
	if err != nil {
		// The message from age does not distinguish a wrong key from a corrupt
		// file, and neither should this: both mean "you cannot get in".
		a.log.Warn("vault unlock refused")
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "that key does not open this vault"})
		return
	}

	// Commit a brand-new vault to disk immediately.
	//
	// Open on a missing file returns an empty vault in memory and writes
	// nothing — reasonably, since there is nothing to write. But the button
	// says "Create", and it did not: the file first appeared when you stored a
	// secret, so a restart before that silently discarded the passphrase and
	// the page offered to create a vault all over again. It looks exactly like
	// the vault was wiped.
	//
	// Saving an empty vault costs one small file and makes the promise true.
	if !vaultExists(a.path) {
		if err := v.Save(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "could not create the vault: " + err.Error()})
			return
		}
		a.log.Info("vault created from the setup page", "path", a.path)
	}

	a.mu.Lock()
	a.v = v
	a.mu.Unlock()

	a.log.Info("vault unlocked from the setup page")
	// Empty: every key just became readable at once, not one of them.
	a.rearm("")
	writeJSON(w, http.StatusOK, a.snapshot(r))
}

func (a *SecretsAdmin) put(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !validKey(key) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a key is letters, digits, dot, dash and underscore"})
		return
	}

	var body struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Trailing whitespace from a paste is the commonest way a credential is
	// wrong in a way nothing reports. A PEM keeps its internal newlines; only
	// the ends are trimmed.
	body.Value = strings.TrimSpace(body.Value)
	if body.Value == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty value"})
		return
	}

	a.mu.Lock()
	v := a.v
	a.mu.Unlock()
	if v == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the vault is locked"})
		return
	}

	if err := v.Set(key, vault.NewSecretString(body.Value)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := v.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// The value, and therefore the redactor, has changed.
	a.rearm(key)
	a.log.Info("secret stored", "key", key, "bytes", len(body.Value))
	writeJSON(w, http.StatusOK, a.snapshot(r))
}

func (a *SecretsAdmin) del(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	a.mu.Lock()
	v := a.v
	a.mu.Unlock()
	if v == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the vault is locked"})
		return
	}

	if err := v.Delete(key); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err := v.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	a.rearm(key)
	a.log.Info("secret deleted", "key", key)
	writeJSON(w, http.StatusOK, a.snapshot(r))
}

// Vault returns the open vault, or nil. Used by the daemon to re-resolve
// build.secrets after a change.
func (a *SecretsAdmin) Vault() *vault.Vault {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

func validKey(k string) bool {
	if k == "" || len(k) > 128 {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func decodeJSON(r *http.Request, into any) error {
	// 1 MiB: a PEM private key is a few kilobytes, and this is the only
	// endpoint that accepts a body from a browser.
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
