package expose

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	httptransport "github.com/go-openapi/runtime/client"
	"github.com/openziti/zrok/v2/environment"
	"github.com/openziti/zrok/v2/environment/env_core"
	"github.com/openziti/zrok/v2/rest_client_zrok/account"
	"github.com/openziti/zrok/v2/sdk/golang/sdk"

	"github.com/netfoundry/docpreview/internal/vault"
)

// Enrolling a zrok account and an environment from inside docpreview.
//
// # Why this is here rather than left to the zrok CLI
//
// Everything below is a thing an operator could do with `zrok2 invite`, `zrok2 enable` and a web
// browser. It is here because the first thirty minutes of using docpreview were: install a second
// binary, find the invite page, read an email, run two commands with a token pasted between them,
// and only then discover whether the daemon agrees that any of it worked. Every one of those steps
// can fail quietly, and the daemon's own error for all of them was the same sentence about running
// `zrok2 enable`.
//
// # The two roots, and why the choice is stored
//
// zrok keeps its account token and enrolled identity in a directory — `~/.zrok2` by default. Which
// directory is decided by a **process-wide global** in the zrok library, `environment.SetRootDirName`,
// read by every `LoadRoot`. So it is set once, before anything loads a root, and never again.
//
// docpreview supports two: the machine's, and one of its own beside the vault. Both existing and
// both being enabled is the ordinary case on a developer's machine — the operator ran
// `zrok2 enable` months ago for something else — and there is no safe way to guess between them.
// Publishing from the wrong account is not a cosmetic mistake: `Reap` deletes every share it
// recognises as its own at startup, so a daemon that silently adopted the machine-wide
// environment would delete the shares of whatever else was using it.
//
// So the choice is explicit, stored, and reported. `InspectZrokRoots` says what exists; the
// operator picks; `UseZrokRoot` applies it.

// ZrokScope names which zrok environment directory is in force.
type ZrokScope string

const (
	// ZrokSystem is the machine-wide environment, `~/.zrok2` — what the zrok CLI uses and what
	// an operator who has already run `zrok2 enable` expects to keep working.
	ZrokSystem ZrokScope = "system"

	// ZrokProject is this installation's own, beside the vault. Preferred for a new
	// installation, and required for a container, where a home directory is not durable.
	ZrokProject ZrokScope = "project"
)

// ZrokRootDirName is the directory zrok uses inside a home directory. Duplicated from
// env_v0_4's unexported default so that the system path can be *reported* without changing the
// global that selects it — the whole point of the inspection path is to describe both roots
// without switching to either.
const ZrokRootDirName = ".zrok2"

// zrokRootMu serialises anything that touches zrok's process-wide root-directory global.
//
// The global is the reason this file has a mutex at all. Inspection has to look at a root that is
// not the one in force, which means setting the global, loading, and putting it back — and two of
// those at once would leave it pointing somewhere nobody asked for.
var zrokRootMu sync.Mutex

// zrokRootChosen records that UseZrokRoot has run, so a second call with a different answer is
// an error rather than a silent switch. Rebinding the global after the exposer has loaded its
// root would leave the process holding one environment while writing to another.
var zrokRootChosen struct {
	set   bool
	scope ZrokScope
	path  string
}

// SystemZrokDir is where the zrok CLI keeps its environment on this machine.
func SystemZrokDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory to locate zrok's environment: %w", err)
	}
	return filepath.Join(home, ZrokRootDirName), nil
}

// ZrokRootInfo is what can be said about one zrok environment directory without enrolling
// anything.
//
// Deliberately carries no token. The account token is in that directory in plaintext, and an
// endpoint that reported it would put it in a browser, a log and a support paste — see the note
// on vault.Secret. What a reader needs is whether it works and which account it is, and the email
// answers the second.
type ZrokRootInfo struct {
	// Path is the directory itself, shown because "which zrok am I using" is otherwise
	// unanswerable from the dashboard.
	Path string `json:"path"`

	// Exists is whether the directory is there at all. Distinct from Enabled: a root that
	// exists but is not enabled is a half-finished setup, and one that does not exist is a
	// clean slate.
	Exists bool `json:"exists"`

	// Enabled is whether an environment is enrolled — an account token and a ziti identity.
	Enabled bool `json:"enabled"`

	// APIEndpoint is which zrok service it is enrolled against. Two roots pointing at
	// different controllers is a real configuration and a confusing one, so it is shown.
	APIEndpoint string `json:"api_endpoint,omitempty"`

	// Namespace is the default namespace, which is what a share is published into when the
	// config names none.
	Namespace string `json:"namespace,omitempty"`

	// Why explains an unusable root, in the operator's terms. Empty when there is nothing
	// wrong.
	Why string `json:"why,omitempty"`

	// Unsupported is set when the environment is enrolled and docpreview still cannot use it:
	// an on-disk format from an older zrok, or an enrolment against the v1 service.
	//
	// Separate from Why, which is about a root that is not set up. This one is about a root
	// that *is* set up and does not meet the requirement — the state that otherwise reads as
	// working right up until the first publish fails.
	Unsupported string `json:"unsupported,omitempty"`
}

// ZrokEnvState is both roots plus the decision between them.
type ZrokEnvState struct {
	// Scope is which root is in force in this process. Empty before UseZrokRoot has run.
	Scope ZrokScope `json:"scope,omitempty"`

	System  ZrokRootInfo `json:"system"`
	Project ZrokRootInfo `json:"project"`

	// MustChoose is true when both roots are enabled and nothing has been stored. This is the
	// state that must stop and ask rather than pick: either answer publishes from a different
	// account, and one of them may be an account something else is already reaping.
	MustChoose bool `json:"must_choose"`
}

// Enabled reports whether the root now in force can publish.
func (s ZrokEnvState) Enabled() bool {
	switch s.Scope {
	case ZrokProject:
		return s.Project.Enabled
	case ZrokSystem:
		return s.System.Enabled
	default:
		return false
	}
}

// InspectZrokRoots describes both zrok environments.
//
// projectDir is config.Server.ZrokDir(). scope is the stored choice, or empty when none has been
// made — it is echoed back rather than consulted, because this function must describe both roots
// whichever one is in force.
//
// It changes zrok's root-directory global twice and restores it, under the package mutex. That is
// unpleasant and it is the only way: the library offers no way to load a root by path.
func InspectZrokRoots(projectDir string, scope ZrokScope) (ZrokEnvState, error) {
	systemDir, err := SystemZrokDir()
	if err != nil {
		return ZrokEnvState{}, err
	}

	zrokRootMu.Lock()
	defer zrokRootMu.Unlock()

	// Restored to whatever is in force, not to zrok's default: if UseZrokRoot has already
	// pointed the process at the project root, leaving the global on the system root would send
	// the next publish to the other account.
	restore := ZrokRootDirName
	if zrokRootChosen.set {
		restore = zrokRootChosen.path
	}
	defer environment.SetRootDirName(restore)

	st := ZrokEnvState{Scope: scope}
	st.System = inspectOneZrokRoot(systemDir)
	st.Project = inspectOneZrokRoot(projectDir)
	st.MustChoose = scope == "" && st.System.Enabled && st.Project.Enabled
	return st, nil
}

// inspectOneZrokRoot loads one directory and reports on it. Caller holds zrokRootMu.
func inspectOneZrokRoot(dir string) ZrokRootInfo {
	info := ZrokRootInfo{Path: dir}
	if _, err := os.Stat(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			info.Why = fmt.Sprintf("cannot read %s: %v", dir, err)
		}
		return info
	}
	info.Exists = true

	environment.SetRootDirName(dir)
	root, err := environment.LoadRoot()
	if err != nil {
		info.Why = fmt.Sprintf("the directory is there but zrok cannot read it: %v", err)
		return info
	}

	info.Enabled = root.IsEnabled()
	if ep, _ := root.ApiEndpoint(); ep != "" {
		info.APIEndpoint = ep
	}
	if ns, _ := root.DefaultNamespace(); ns != "" {
		info.Namespace = ns
	}
	if info.Exists && !info.Enabled {
		info.Why = "the directory exists but no account is enrolled in it"
	}
	if info.Enabled {
		info.Unsupported = zrokUnsupported(root, info.APIEndpoint)
	}
	return info
}

// zrokUnsupported reports why an enrolled environment still cannot be used, empty when it can.
//
// docpreview is built against the zrok **v2** SDK, and the two versions are not
// interchangeable — a v1 controller has no namespaces and no reserved names, which is the whole
// mechanism a preview's stable URL depends on. Enrolled against the wrong one, everything looks
// configured and the first publish fails with a 404 from a path that does not exist.
//
// Two checks, because there are two ways to be on the wrong version:
//
//   - The on-disk format. `environment.IsLatest` compares the root's recorded version against the
//     one this library writes. An older directory is a v1 enrolment, or a v2 one from before a
//     format change.
//   - The service. The hosted v1 API is `api.zrok.io` and the hosted v2 API is `api-v2.zrok.io`,
//     so a `zrok.io` endpoint with no v2 marker is the v1 service. Only checked for `zrok.io`
//     hosts: a self-hosted controller can be at any address and guessing at one would refuse a
//     working setup.
func zrokUnsupported(root env_core.Root, apiEndpoint string) string {
	if !environment.IsLatest(root) {
		v := "an older version"
		if m := root.Metadata(); m != nil && m.V != "" {
			v = "version " + m.V
		}
		return "this environment directory is " + v + ", which docpreview cannot use; " +
			"docpreview needs a zrok v2 environment"
	}
	if apiEndpoint == "" {
		return ""
	}
	u, err := url.Parse(apiEndpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "zrok.io" || strings.HasSuffix(host, ".zrok.io") {
		if !strings.Contains(host, "v2") && !strings.Contains(u.Path, "v2") {
			return "this is enrolled against the zrok v1 service (" + apiEndpoint + "). " +
				"docpreview needs v2, which has the namespaces and reserved names a stable " +
				"preview URL depends on — v1 has neither"
		}
	}
	return ""
}

// UseZrokRoot points this process at one of the two environments.
//
// Called once, before anything loads a zrok root — which in practice means before the exposer is
// constructed. A second call naming the same scope is accepted and does nothing, so wiring can be
// defensive; a second call naming a different one is an error, because the process would then hold
// a root loaded from one directory while writing to another.
//
// ZrokSystem is a no-op on the global, deliberately: leaving the library's own default in place is
// the same thing and it keeps `~/.zrok2` working for an operator who never chose anything.
func UseZrokRoot(scope ZrokScope, projectDir string) error {
	zrokRootMu.Lock()
	defer zrokRootMu.Unlock()

	var path string
	switch scope {
	case ZrokProject:
		if projectDir == "" {
			return errors.New("no project zrok directory given; this is derived from data_dir")
		}
		abs, err := filepath.Abs(projectDir)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", projectDir, err)
		}
		path = abs
	case ZrokSystem:
		path = ZrokRootDirName
	default:
		return fmt.Errorf("unknown zrok scope %q: use %q or %q", scope, ZrokSystem, ZrokProject)
	}

	if zrokRootChosen.set {
		if zrokRootChosen.scope == scope && zrokRootChosen.path == path {
			return nil
		}
		return fmt.Errorf("this process is already using the %s zrok environment (%s); "+
			"restart the daemon to change it",
			zrokRootChosen.scope, zrokRootChosen.path)
	}

	environment.SetRootDirName(path)
	zrokRootChosen.set = true
	zrokRootChosen.scope = scope
	zrokRootChosen.path = path
	return nil
}

// ZrokScopeInForce is the scope UseZrokRoot applied, empty if it has not run.
func ZrokScopeInForce() ZrokScope {
	zrokRootMu.Lock()
	defer zrokRootMu.Unlock()
	if !zrokRootChosen.set {
		return ""
	}
	return zrokRootChosen.scope
}

// DefaultZrokAPIEndpoint is the hosted service, used when neither root has a config to read one
// from — which is the case for a signup, since signing up is what happens before there is a root.
const DefaultZrokAPIEndpoint = "https://api-v2.zrok.io"

// zrokAccountClient builds a client for the account endpoints, which need no credential.
//
// Built from an endpoint string rather than from a root, because all three calls here happen
// before an environment exists. The endpoint is parsed rather than concatenated so that a
// mistyped one fails here with the value in the message.
func zrokAccountClient(apiEndpoint string) (account.ClientService, error) {
	if apiEndpoint == "" {
		apiEndpoint = DefaultZrokAPIEndpoint
	}
	u, err := url.Parse(apiEndpoint)
	if err != nil {
		return nil, fmt.Errorf("the zrok API endpoint %q is not a URL: %w", apiEndpoint, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("the zrok API endpoint %q has no host; "+
			"write it as https://api-v2.zrok.io", apiEndpoint)
	}
	basePath := u.Path
	if basePath == "" || basePath == "/" {
		basePath = "/api/v2"
	}
	transport := httptransport.New(u.Host, basePath, []string{u.Scheme})
	return account.New(transport, nil), nil
}

// ZrokInvite asks the zrok service to email a registration link.
//
// inviteToken is for services configured with `tokenStrategy: store`, where an invite is itself
// invitation-only. Empty is correct for the hosted service.
//
// The registration token is **not** returned: the service emails it, and that is the point of the
// step. So the operator's next action is to open their email — which the caller must say, because
// a form that submits and reports nothing looks broken.
func ZrokInvite(ctx context.Context, apiEndpoint, email, inviteToken string) error {
	client, err := zrokAccountClient(apiEndpoint)
	if err != nil {
		return err
	}
	params := account.NewInviteParamsWithContext(ctx)
	params.Body.Email = strings.TrimSpace(email)
	params.Body.InviteToken = strings.TrimSpace(inviteToken)

	if _, err := client.Invite(params); err != nil {
		// The service answers 400 for a duplicate email with a body saying so, which is the
		// single most likely outcome for somebody who has used zrok before — worth passing
		// through rather than flattening into "bad request".
		return fmt.Errorf("asking zrok at %s to invite %s: %w",
			orDefaultEndpoint(apiEndpoint), email, err)
	}
	return nil
}

// ZrokRegister exchanges a registration token and a chosen password for an account token.
//
// The token comes from the emailed link, whose last path segment it is. ZrokRegisterToken pulls it
// out, so a caller can accept whichever of the two the operator pastes.
//
// The account token is returned as a vault.Secret: it is the credential that can create and delete
// every share on the account, and it must not appear in a log, an error or a JSON response.
func ZrokRegister(ctx context.Context, apiEndpoint, registerToken, password string) (vault.Secret, error) {
	token := ZrokRegisterToken(registerToken)
	if token == "" {
		return vault.Secret{}, errors.New("no registration token; paste the link zrok emailed you, " +
			"or just the token at the end of it")
	}
	if password == "" {
		return vault.Secret{}, errors.New("no password; zrok needs one for the account it is creating")
	}

	client, err := zrokAccountClient(apiEndpoint)
	if err != nil {
		return vault.Secret{}, err
	}
	params := account.NewRegisterParamsWithContext(ctx)
	params.Body.RegisterToken = token
	params.Body.Password = password

	resp, err := client.Register(params)
	if err != nil {
		// 404 means the token is unknown, which after a successful registration is what a
		// second attempt with the same link looks like — the service deletes the request once
		// it is used. Named because "not found" for a token that is visibly right is otherwise
		// baffling.
		return vault.Secret{}, fmt.Errorf("registering with zrok at %s "+
			"(a token already used answers 404, and each link works once): %w",
			orDefaultEndpoint(apiEndpoint), err)
	}
	if resp.Payload == nil || resp.Payload.AccountToken == "" {
		return vault.Secret{}, errors.New("zrok accepted the registration but returned no account token")
	}
	return vault.NewSecretString(resp.Payload.AccountToken), nil
}

// ZrokRegisterToken extracts the registration token from whatever was pasted.
//
// The email carries `<registrationUrl>/<token>`, so the token is the last path segment. Accepting
// both the URL and the bare token is not politeness: the operator has a browser link in their
// hand, and asking them to edit it is asking them to guess which part matters.
func ZrokRegisterToken(pasted string) string {
	s := strings.TrimSpace(pasted)
	if s == "" {
		return ""
	}
	// Query and fragment first: some services carry the token as the last segment of a path
	// that is followed by neither, but a copied link can pick up both.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// ZrokEnable enrolls this host's environment with an account token.
//
// The equivalent of `zrok2 enable <token>`, and the order of the three writes is copied from it:
// call the service, save the environment, then save the ziti identity the service returned. Done
// in the other order, a failure leaves an environment record pointing at an identity that is not
// on disk — which reports as enabled and fails every share.
//
// It refuses an already-enabled root rather than enrolling a second time. Every enable consumes an
// environment against the account's quota and leaves the previous one behind, orphaned, still
// counted, and still holding whatever shares it had.
func ZrokEnable(ctx context.Context, accountToken vault.Secret, description string) (err error) {
	root, err := environment.LoadRoot()
	if err != nil {
		return fmt.Errorf("loading the zrok environment directory: %w", err)
	}
	if root.IsEnabled() {
		return errors.New("this zrok environment is already enabled; disable it first if you " +
			"mean to enrol against a different account")
	}
	if accountToken.IsZero() {
		return errors.New("no zrok account token")
	}
	if description == "" {
		description = "docpreview"
	}

	apiEndpoint, _ := root.ApiEndpoint()

	// SetEnvironment before EnableEnvironment, because the SDK reads the account token back out
	// of the root to authenticate the call. Written twice for that reason: once to carry the
	// token in, and again with the identity the service returns.
	if err := root.SetEnvironment(&env_core.Environment{
		AccountToken: accountToken.RevealString(),
		ApiEndpoint:  apiEndpoint,
	}); err != nil {
		return fmt.Errorf("saving the zrok environment: %w", err)
	}
	// Anything that fails from here leaves a half-written environment that reports enabled and
	// cannot publish, so the record goes away with the failure.
	defer func() {
		if err != nil {
			if delErr := root.DeleteEnvironment(); delErr != nil {
				err = errors.Join(err, fmt.Errorf("and the half-written environment "+
					"could not be removed: %w", delErr))
			}
		}
	}()

	env, err := sdk.EnableEnvironment(root, &sdk.EnableRequest{
		Description: description,
		Host:        description,
	})
	if err != nil {
		return fmt.Errorf("zrok at %s refused to enable this environment "+
			"(is the account token right?): %w", orDefaultEndpoint(apiEndpoint), err)
	}

	if err := root.SetEnvironment(&env_core.Environment{
		AccountToken: accountToken.RevealString(),
		ZitiIdentity: env.ZitiIdentity,
		ApiEndpoint:  apiEndpoint,
	}); err != nil {
		return fmt.Errorf("saving the enabled zrok environment: %w", err)
	}
	if err := root.SaveZitiIdentityNamed(root.EnvironmentIdentityName(), env.ZitiConfig); err != nil {
		return fmt.Errorf("writing the ziti identity zrok issued: %w", err)
	}
	return nil
}

// ZrokDisable removes this host's environment from the account and from disk.
//
// It is the escape from a wrong choice, and it is destructive in a way worth naming: the
// environment owns the shares published through it, so disabling takes every live preview URL
// with it. The names survive — they belong to the account — so republishing after a fresh enable
// restores the same URLs.
func ZrokDisable(ctx context.Context) error {
	root, err := environment.LoadRoot()
	if err != nil {
		return fmt.Errorf("loading the zrok environment directory: %w", err)
	}
	if !root.IsEnabled() {
		return errors.New("this zrok environment is not enabled; there is nothing to disable")
	}
	env := root.Environment()
	if err := sdk.DisableEnvironment(&sdk.Environment{ZitiIdentity: env.ZitiIdentity}, root); err != nil {
		return fmt.Errorf("zrok refused to disable this environment: %w", err)
	}
	if err := root.DeleteEnvironment(); err != nil {
		return fmt.Errorf("removing the local zrok environment: %w", err)
	}
	return nil
}

func orDefaultEndpoint(s string) string {
	if s == "" {
		return DefaultZrokAPIEndpoint
	}
	return s
}
