// Package vault stores docpreview's credentials encrypted at rest.
//
// The threat model is deliberately modest and deliberately explicit. docpreview
// runs on a laptop or a small VM, not in a cloud KMS. The property we want is:
// somebody who walks off with the data directory — a stolen laptop, a leaked
// backup, a snapshot of a VM disk — gets nothing. Somebody who is already
// running code as the docpreview user has won, and no file format fixes that.
//
// The whole vault is one age-encrypted blob.
//
// age (https://age-encryption.org, filippo.io/age) is a file encryption format
// with a deliberately tiny API: generate an identity, encrypt to a recipient,
// decrypt with an identity. No cipher selection, no key size, no mode, no
// keyring, no config. It is used here rather than a hand-rolled AEAD because
// the two decisions a hand-rolled version has to make — key derivation and
// nonce handling — are exactly the two that quietly ruin this kind of file.
// Reuse a nonce and the ciphertexts XOR into plaintext; pick a fast hash where
// scrypt was needed and the passphrase falls to a wordlist. Both failures still
// encrypt, still decrypt, and still pass every test you would think to write.
//
// A recipient is either an X25519 public key or a scrypt-stretched passphrase;
// both are supported, see ResolveKey. The key never lands on disk: it comes
// from the environment or an interactive prompt and lives only in memory.
package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"filippo.io/age"
	"golang.org/x/term"
)

// MasterKeyEnv is the environment variable consulted for the vault key. Its
// value is either an age X25519 identity ("AGE-SECRET-KEY-1...") or, failing
// that, treated as a passphrase.
//
// Supported, not recommended. It is consulted only when vault.key_source is
// unset, and OWASP is blunt about why: environment variables "are generally
// accessible to all processes and may be included in logs or system dumps",
// and using them is "not recommended unless the other methods are not
// possible". Prefer a KeySource.
const MasterKeyEnv = "DOCPREVIEW_MASTER_KEY"

// Well-known vault keys. Using constants rather than bare strings means a typo
// is a compile error instead of a silently missing credential at 3am.
const (
	KeyGitHubPrivateKey = "github.private_key"
	KeyGitHubWebhookSec = "github.webhook_secret"
	// KeyBitbucketAccessToken is the recommended Bitbucket credential: a
	// repository, project or workspace access token. Scoped to one resource,
	// revocable on its own, and attributed to a synthetic bot address rather
	// than to a person.
	//
	// The two keys below name the fallback mode, which is wider in scope: an operator who
	// stores those instead gets a credential covering more than one resource by default.
	KeyBitbucketAccessToken = "bitbucket.access_token"

	// KeyBitbucketEmail and KeyBitbucketAPIToken are the api_token fallback. The
	// email is not itself a secret but is half an Authorization header and half a
	// clone URL, so it lives here with its partner.
	KeyBitbucketEmail    = "bitbucket.email"
	KeyBitbucketAPIToken = "bitbucket.api_token"

	KeyBitbucketHookSec = "bitbucket.webhook_secret"
	KeyFrontdoorToken   = "frontdoor.api_token"

	// KeyZrokAccountToken is the zrok account token, which creates and deletes every share on
	// the account.
	//
	// Here even though zrok also writes it to its own `environment.json` in plaintext, which
	// docpreview does not control. Two reasons: enrolling a *second* host on the same account
	// otherwise needs the registration email again, which has by then been used; and the
	// registration response is the only place the token ever appears, so a signup done from the
	// dashboard would produce a credential that exists in no readable place at all.
	//
	// Not read by the exposer — that authenticates from zrok's environment directory, as the
	// zrok CLI does. This copy exists for re-enrolment.
	KeyZrokAccountToken = "zrok.account_token"

	// The Google OAuth application, for signing in to the dashboard.
	//
	// The id is not a secret — it appears in the URL a browser is sent to — but it lives here
	// with its partner because the pair is useless separated, and because an operator looking
	// for "where do I put the Google credentials" should find one answer.
	//
	// A locked vault therefore means no Google sign-in, and the login page says so rather than
	// showing a button that cannot work. Password login is unaffected, which is what keeps the
	// unlock page reachable — the same ordering problem that decides where the password hashes
	// live, arriving from the other direction.
	KeyGoogleClientID     = "google.oauth_client_id"
	KeyGoogleClientSecret = "google.oauth_client_secret"
)

// ProjectPrefix is the namespace every project-scoped secret lives under.
//
// One vault, one flat map, and a naming convention rather than a second store: the
// key is the only structure there is, and adding a nested format would change the
// on-disk shape of every existing vault to express something a prefix already can.
//
// The slash is deliberate and is what keeps the two scopes from ever colliding. A
// global key is validated as letters, digits, dot, dash and underscore — no slash —
// so `/api/secrets/{key}` cannot reach into this namespace even if somebody asks it
// to, and a project secret cannot shadow `github.private_key`.
const ProjectPrefix = "project/"

// ProjectSecretPrefix is where one project's secrets live.
func ProjectSecretPrefix(platform, owner, repo string) string {
	return ProjectPrefix + platform + "/" + owner + "/" + repo + "/"
}

// Per-project source-control credentials, for a platform that cannot issue one token
// covering several repositories.
//
// Bitbucket is the reason these exist. An access token there is scoped to a repository, a
// project or a workspace, and an administrator can refuse the wider two — at which point
// one `bitbucket.access_token` in the global namespace cannot reach a second repository,
// and the only place a per-repository credential can live is beside the project row.
//
// Deliberately **dotted**, which is what keeps them out of a build. They share the project
// prefix with that project's environment variables, and the resolver that builds a build's
// environment takes only shell-shaped names — the same rule that keeps
// `github.private_key` out of every build, applied one scope down. A credential that can
// clone and comment on a repository is not something a pull request's own build script
// should be handed.
const (
	SCMAccessToken = "scm.access_token"
	SCMEmail       = "scm.email"
	SCMAPIToken    = "scm.api_token"
)

// ProjectSCMKey is where one project's own source-control credential lives.
func ProjectSCMKey(platform, owner, repo, name string) string {
	return ProjectSecretPrefix(platform, owner, repo) + name
}

// IsProjectSCMKey reports whether a name under a project prefix is one of the
// source-control credentials rather than a build variable.
//
// A closed list rather than "anything dotted", so a typo does not silently become a
// credential the daemon looks for and nothing sets.
func IsProjectSCMKey(name string) bool {
	switch name {
	case SCMAccessToken, SCMEmail, SCMAPIToken:
		return true
	}
	return false
}

// IsBuildEnvKey reports whether a vault key is a build environment variable rather than
// an infrastructure credential.
//
// The rule is the key's *shape*: upper-case letters, digits and underscore, not starting
// with a digit — the form a shell can read. That is what separates BB_REPO_TOKEN_ONPREM,
// which a build script looks for by name, from github.private_key, which the daemon uses
// itself and no build should ever see. The dotted keys are all infrastructure and the
// naming convention already carried that distinction; this makes it load-bearing.
//
// A global secret stored under a shell-shaped name reaches every build on this daemon, and
// a project's own entry of the same name overrides it. A build can read its own environment,
// so store what a build needs and nothing else.
func IsBuildEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ProjectSecretKey names one environment variable's value for one project.
//
// The environment variable name is the last segment, so listing a project's secrets
// is a prefix scan of Keys() and needs no parsing — which matters because an owner
// or a repository may contain a dot or a dash and splitting on those would be
// ambiguous.
func ProjectSecretKey(platform, owner, repo, env string) string {
	return ProjectSecretPrefix(platform, owner, repo) + env
}

// ErrLocked is returned when the vault file exists but no key was available to
// open it.
var ErrLocked = errors.New("vault is locked: no master key available")

// ErrNotFound is returned by Get for an absent key.
var ErrNotFound = errors.New("no such secret")

// Vault is a set of named secrets backed by an encrypted file. It is safe for
// concurrent use.
type Vault struct {
	path string

	mu      sync.RWMutex
	secrets map[string]Secret

	// identity and recipient are the age key material for this vault. Both are
	// derived from the same master key.
	identity  age.Identity
	recipient age.Recipient
}

// Open loads the vault at path, decrypting it with the master key resolved by
// ResolveKey. If the file does not exist, an empty vault is returned and the
// file is created on the first Save.
func Open(path string) (*Vault, error) {
	return OpenFrom(path, KeySource{})
}

// OpenFrom loads the vault at path using the configured key source. See
// ResolveKeyFrom for the resolution order.
func OpenFrom(path string, src KeySource) (*Vault, error) {
	id, rcp, err := ResolveKeyFrom(src)
	if err != nil {
		return nil, err
	}
	return openWith(path, id, rcp)
}

// OpenWithKey opens the vault with a key supplied by the caller rather than
// from the environment or a terminal prompt.
//
// It exists for the setup UI, where the key arrives in a form post: a browser
// has no stdin and the server process cannot be handed a new environment
// variable after it has started. The key is used and discarded; it is never
// written anywhere, which is the same guarantee ResolveKey gives.
//
// An empty key is refused rather than treated as a passphrase, because an
// empty scrypt passphrase is a valid one and would produce a vault anybody
// could open.
func OpenWithKey(path, key string) (*Vault, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("%w: empty key", ErrLocked)
	}
	id, rcp, err := keyFromString(key)
	if err != nil {
		return nil, err
	}
	return openWith(path, id, rcp)
}

func openWith(path string, id age.Identity, rcp age.Recipient) (*Vault, error) {
	v := &Vault{
		path:      path,
		secrets:   map[string]Secret{},
		identity:  id,
		recipient: rcp,
	}

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading vault: %w", err)
	}

	r, err := age.Decrypt(bytes.NewReader(raw), v.identity)
	if err != nil {
		// age does not distinguish "wrong key" from "corrupt file" in a way
		// worth surfacing separately; both mean the operator has to intervene.
		return nil, fmt.Errorf("decrypting vault (wrong master key?): %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading decrypted vault: %w", err)
	}

	var onDisk map[string]string
	if err := json.Unmarshal(plain, &onDisk); err != nil {
		return nil, fmt.Errorf("parsing decrypted vault: %w", err)
	}
	for k, val := range onDisk {
		v.secrets[k] = NewSecretString(val)
	}
	// The plaintext copy has served its purpose.
	zero(plain)

	return v, nil
}

// Get returns the named secret.
func (v *Vault) Get(key string) (Secret, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.secrets[key]
	if !ok {
		return Secret{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return s, nil
}

// MustGet returns the named secret or an error naming the vault command that
// would fix the omission. Startup validation uses this so that a fresh install
// tells the operator what to do instead of failing with a nil dereference three
// layers down.
func (v *Vault) MustGet(key string) (Secret, error) {
	s, err := v.Get(key)
	if err != nil {
		return Secret{}, fmt.Errorf("%w (set it with: docpreview vault set %s)", err, key)
	}
	if s.IsZero() {
		return Secret{}, fmt.Errorf("secret %s is empty (set it with: docpreview vault set %s)", key, key)
	}
	return s, nil
}

// Set stores a secret and persists the vault.
func (v *Vault) Set(key string, s Secret) error {
	v.mu.Lock()
	v.secrets[key] = s
	v.mu.Unlock()
	return v.Save()
}

// Delete removes a secret and persists the vault.
func (v *Vault) Delete(key string) error {
	v.mu.Lock()
	delete(v.secrets, key)
	v.mu.Unlock()
	return v.Save()
}

// Keys lists the stored secret names, sorted. Names only — never values.
func (v *Vault) Keys() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]string, 0, len(v.secrets))
	for k := range v.secrets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// KeysWithPrefix lists the stored names under a prefix, sorted. Names only.
func (v *Vault) KeysWithPrefix(prefix string) []string {
	var out []string
	for _, k := range v.Keys() {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

// Reveal returns every value under a prefix, keyed by the segment after it.
//
// This is the one bulk read of values in the package, and it exists because the
// alternative is the caller doing Keys-then-Get in a loop and getting the prefix
// arithmetic subtly wrong. It is named Reveal for the same reason Secret.Reveal is:
// a reviewer grepping for where plaintext credentials come from has to find this.
//
// The returned map holds bare strings, so the caller owns the consequences. Its only
// intended use is building a process environment for a build, where the value has to
// be a string eventually — and where the redactor is compiled from these same values
// so they cannot reach a log.
func (v *Vault) RevealPrefix(prefix string) map[string]string {
	out := map[string]string{}
	v.mu.RLock()
	defer v.mu.RUnlock()
	for k, s := range v.secrets {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		name := strings.TrimPrefix(k, prefix)
		// Nothing deeper than one segment is a project secret. A key with another
		// slash in it came from somewhere this does not know about, and guessing
		// would inject an environment variable with a slash in its name.
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		// Only shell-shaped names, the same rule that keeps `github.private_key` out of
		// every build, applied to the project scope. A project's source-control credential
		// lives under this same prefix as `scm.access_token` — dotted, deliberately — and
		// without this filter it would be injected into every build of the repository it
		// can clone and comment on. See TestProjectSCMCredentialsNeverReachABuild.
		if !IsBuildEnvKey(name) {
			continue
		}
		out[name] = s.RevealString()
	}
	return out
}

// Save encrypts and writes the vault. The write goes to a temporary file in the
// same directory and is then renamed over the target, so a crash mid-write
// cannot leave a truncated — and therefore unrecoverable — vault behind.
func (v *Vault) Save() error {
	v.mu.RLock()
	onDisk := make(map[string]string, len(v.secrets))
	for k, s := range v.secrets {
		onDisk[k] = s.RevealString()
	}
	v.mu.RUnlock()

	plain, err := json.Marshal(onDisk)
	if err != nil {
		return fmt.Errorf("serializing vault: %w", err)
	}
	defer zero(plain)

	if err := os.MkdirAll(filepath.Dir(v.path), 0o700); err != nil {
		return fmt.Errorf("creating vault directory: %w", err)
	}

	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, v.recipient)
	if err != nil {
		return fmt.Errorf("starting vault encryption: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return fmt.Errorf("encrypting vault: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finalizing vault encryption: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(v.path), ".vault-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp vault: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil && !isWindows() {
		tmp.Close()
		return fmt.Errorf("securing temp vault: %w", err)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp vault: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp vault: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp vault: %w", err)
	}
	if err := os.Rename(tmpName, v.path); err != nil {
		return fmt.Errorf("installing vault: %w", err)
	}
	return nil
}

// ResolveKey produces the age identity and recipient for the vault, with no
// configured key source. See ResolveKeyFrom.
func ResolveKey() (age.Identity, age.Recipient, error) {
	return ResolveKeyFrom(KeySource{})
}

// ResolveKeyFrom produces the age identity and recipient for the vault.
//
// Resolution order:
//
//  1. The configured KeySource — a file, or a command that prints the key. This
//     is the intended production path. See the KeySource doc for why.
//  2. $DOCPREVIEW_MASTER_KEY. Still supported, no longer recommended: an
//     environment variable is readable by every process under the same user and
//     lands in service definitions, process listings and crash dumps.
//  3. An interactive prompt on a TTY.
//
// Whatever the source, the material is either an age X25519 secret key or a
// passphrase. A passphrase is stretched with scrypt by age itself, which is why
// the passphrase path is noticeably slower to open. That is the point.
//
// With no source, no environment variable and no terminal, this returns
// ErrLocked — which is a state the daemon serves in, not a startup failure. The
// dashboard unlocks it.
func ResolveKeyFrom(src KeySource) (age.Identity, age.Recipient, error) {
	if !src.IsZero() {
		raw, err := src.Read()
		if err != nil {
			return nil, nil, err
		}
		return keyFromString(raw)
	}

	if raw := os.Getenv(MasterKeyEnv); raw != "" {
		return keyFromString(strings.TrimSpace(raw))
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, nil, fmt.Errorf("%w: set vault.key_source in the config, "+
			"unlock it from the dashboard, or run on a terminal", ErrLocked)
	}

	fmt.Fprint(os.Stderr, "docpreview vault passphrase: ")
	pass, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, nil, fmt.Errorf("reading passphrase: %w", err)
	}
	if len(pass) == 0 {
		return nil, nil, fmt.Errorf("%w: empty passphrase", ErrLocked)
	}
	return keyFromString(string(pass))
}

func keyFromString(raw string) (age.Identity, age.Recipient, error) {
	if strings.HasPrefix(strings.ToUpper(raw), "AGE-SECRET-KEY-1") {
		id, err := age.ParseX25519Identity(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing %s as an age identity: %w", MasterKeyEnv, err)
		}
		return id, id.Recipient(), nil
	}

	id, err := age.NewScryptIdentity(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("deriving vault key from passphrase: %w", err)
	}
	rcp, err := age.NewScryptRecipient(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("deriving vault recipient from passphrase: %w", err)
	}
	return id, rcp, nil
}

// GenerateIdentity mints a fresh age X25519 identity for use as a vault master
// key. The caller is expected to show it to the operator exactly once.
func GenerateIdentity() (string, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", fmt.Errorf("generating identity: %w", err)
	}
	return id.String(), nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }
