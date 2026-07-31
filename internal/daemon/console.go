package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/netfoundry/docpreview/internal/store"
)

// A login on the whole dashboard, with two roles.
//
// # What this replaces
//
// Until now the only gate was where a request came from: loopback, no forwarding header, or a
// named overlay identity. That is a boundary rather than authentication — it answers "did this
// originate on this machine", not "who is this" — so administering docpreview from anywhere
// else was unsupported, and *reading* it required publishing the dashboard to everyone who
// found the URL.
//
// Two roles, because those are the two audiences:
//
//   - **viewer** sees the dashboard, the previews list, build logs and the activity feed, and
//     can change nothing. This is the colleague who wants to look at a preview.
//   - **admin** additionally gets the projects and credential surfaces, which decide what
//     command runs on the build host and with which secrets.
//
// One password each rather than accounts. There is one operator and a handful of colleagues;
// per-user accounts would need a user store, a reset flow and a password policy, to distinguish
// people who are all doing the same thing. When the answer needs to be per-person, the answer
// is the identity provider — see the note on delegating below.
//
// # Where the hashes live, and why not the vault
//
// The `settings` table, in plaintext, because a password *hash* is not a secret and because the
// vault cannot be used here: it may be locked, and the page that unlocks it is behind this
// gate. A password kept in the vault could not be checked in order to reach the form that opens
// the vault.
//
// Reusing the vault's master key would be worse than a separate password rather than better. It
// decrypts every credential docpreview holds, it is frequently a file or an `exec:` command
// rather than anything typeable, and putting it into a web form widens the exact thing it
// exists to protect.
//
// # Why a hash and not the password encrypted
//
// The obvious idea is to age-encrypt the password and compare ciphertexts. That cannot work:
// age uses a fresh ephemeral key per encryption, so the same password encrypts to different
// bytes every time. Verification is either decrypt-then-compare, which means holding the
// plaintext, or a hash — the standard answer, and the one taken here. argon2id, memory-hard,
// with its parameters recorded alongside each hash so they can be raised later without
// invalidating what is stored.
//
// # Delegating to an identity provider instead
//
// For "colleagues sign in with Google and must have an @example.com address", the cheapest
// correct answer is not implemented here at all — the thing publishing the dashboard already
// does it:
//
//   - **zrok** gates a share on an OAuth provider and a list of email patterns at its frontend
//     (`oauth_provider`, `oauth_email_domains`), so an unauthenticated request never reaches
//     this process.
//   - **Frontdoor** has the same idea per share (`authProviderId`, `oauthEmailDomains`).
//   - **OpenZiti** has no such notion, so the equivalent is two services: one bound for
//     viewers and one for admins, with `admin_identities` naming who may write on the second.
//     A reviewer holding the viewer service's grant cannot reach the admin one at all.
//
// The passwords here are what covers everything else: a bare TCP listener, a tunnel with no
// OAuth configured, and the first login on a fresh host.

// Role is what a session is allowed to do.
type Role string

const (
	// RoleNone is an unauthenticated request.
	RoleNone Role = ""

	// RoleViewer reads. Every GET the dashboard needs, and nothing that changes state.
	RoleViewer Role = "viewer"

	// RoleAdmin reads and writes, including the credential and project surfaces.
	RoleAdmin Role = "admin"
)

// Settings keys holding the encoded argon2id hashes.
const (
	SettingAdminPassword  = "console.admin_password"
	SettingViewerPassword = "console.viewer_password"
)

// settingFor maps a role to where its hash is kept.
func settingFor(role Role) (string, error) {
	switch role {
	case RoleAdmin:
		return SettingAdminPassword, nil
	case RoleViewer:
		return SettingViewerPassword, nil
	default:
		return "", fmt.Errorf("unknown role %q: use admin or viewer", role)
	}
}

// Argon2 parameters. Deliberately modest: this verifies an interactive login on a machine that
// is also running documentation builds, and a login that takes a second on an idle box takes
// several under load. One second of a build's CPU is a worse trade than the marginal strength.
//
// Recorded in the encoded hash, so raising them later leaves existing passwords verifiable.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// minPasswordLen is the floor.
//
// Twelve rather than eight because nothing rate-limits this surface: there is no lockout and no
// attempt counter, so the only cost per guess is one argon2 derivation.
const minPasswordLen = 12

// sessionTTL is how long a login lasts.
//
// Long enough not to be a nuisance while working, short enough that a browser left open on an
// unattended machine is not a standing grant. There is no refresh: the cookie carries its own
// expiry and logging in again is one field.
const sessionTTL = 12 * time.Hour

// HashPassword returns an encoded argon2id hash, salt included.
//
// The format is the PHC string used everywhere else — `$argon2id$v=19$m=…,t=…,p=…$salt$hash` —
// so the parameters travel with the value and a future change to them does not invalidate what
// is already stored.
func HashPassword(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", errors.New("the password is empty")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating a salt: %w", err)
	}
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64(salt), b64(sum)), nil
}

// VerifyPassword checks a password against an encoded hash.
//
// The comparison is constant-time. A byte-by-byte one leaks the length of the matching prefix
// through timing, which over enough attempts recovers the hash — and the hash is what an
// attacker needs to mount an offline attack.
//
// A malformed stored hash is a verification failure, not an error worth distinguishing: either
// way nobody gets in, and telling a caller "your stored hash is corrupt" tells an attacker that
// the password they sent was not the problem.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=…,t=…,p=…", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	// Both halves must be non-empty, and leaving this out is a crash rather than a wrong
	// answer: `argon2.IDKey` with a zero key length dereferences a nil digest inside blake2b
	// and panics. A stored hash ending in `$$` is enough to do it, and that is reachable from
	// a login attempt — so a corrupt settings row would be a way to take the daemon down
	// rather than merely a password nobody can match. Zero cost parameters are the same class
	// of problem arriving through the same untrusted string.
	if len(salt) == 0 || len(want) == 0 || memory == 0 || timeCost == 0 || threads == 0 {
		return false
	}

	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// SetConsolePassword stores the password for one role against a store the caller owns.
//
// Exported for `docpreview console password`, which runs in a separate process from the daemon.
// The rules live here rather than in the command so the two cannot disagree about what an
// acceptable password is — the CLI is how the *first* one is set, so it is the path that most
// needs to enforce them.
func SetConsolePassword(ctx context.Context, st *store.Store, role Role, password string) error {
	key, err := settingFor(role)
	if err != nil {
		return err
	}
	if len([]rune(password)) < minPasswordLen {
		return fmt.Errorf("use at least %d characters: this surface can be reached remotely "+
			"and there is no attempt limit", minPasswordLen)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, key, hash)
}

// ClearConsolePassword removes one role's password.
//
// Clearing the admin password returns writes to loopback-only. Clearing the viewer password
// makes the dashboard readable by anyone who can reach it — which is the behaviour every
// installation had before this existed, and is why it has to be possible to say.
func ClearConsolePassword(ctx context.Context, st *store.Store, role Role) error {
	key, err := settingFor(role)
	if err != nil {
		return err
	}
	return st.ClearSetting(ctx, key)
}

// ConsolePasswordSet reports whether a role's password is set, revealing nothing about it.
func ConsolePasswordSet(ctx context.Context, st *store.Store, role Role) (bool, error) {
	key, err := settingFor(role)
	if err != nil {
		return false, err
	}
	v, _, err := st.Setting(ctx, key)
	if err != nil {
		return false, err
	}
	return v != "", nil
}

// console is the login gate.
//
// The signing key is per process and never stored. Every restart invalidates every session,
// which for a daemon restarted by hand is one login, and it means there is no long-lived
// session-signing secret on disk to steal.
//
// **The hashes are deliberately not cached.** Every check reads the settings row.
//
// The first version held them in memory, loaded at startup, which was wrong the moment
// `docpreview console password` existed: setting a password from the CLI on a running daemon
// would have left the gate off until somebody restarted it. A security control that silently
// takes effect later is worse than one that is off, because the operator believes it is on.
//
// The cost is one indexed lookup on a local sqlite file per request. Against a dashboard whose
// traffic is a poll every five seconds per open tab, that is nothing.
type console struct {
	store *store.Store

	mu     sync.RWMutex
	signer []byte
}

func newConsole(st *store.Store) (*console, error) {
	signer := make([]byte, 32)
	if _, err := rand.Read(signer); err != nil {
		return nil, fmt.Errorf("generating a session key: %w", err)
	}
	return &console{store: st, signer: signer}, nil
}

// hashFor reads one role's stored hash, empty when none is set.
//
// A read failure is reported as "no password", which fails *open* on a database error. The
// alternative fails closed and locks an operator out of their own daemon over a transient sqlite
// error, with no way back in — and what sits behind this is still protected by the locality and
// identity gates, which do not touch the database.
func (c *console) hashFor(ctx context.Context, role Role) string {
	key, err := settingFor(role)
	if err != nil {
		return ""
	}
	v, _, err := c.store.Setting(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

// loginRequired reports whether an unauthenticated request should be sent to the login page.
//
// False when no viewer *or* admin password is set, which is every installation that has not
// opted in: the dashboard stays open, exactly as before. Setting only an admin password
// protects the write surfaces and leaves reading open, which is a reasonable arrangement and
// the reason this is not simply "is any password set".
func (c *console) loginRequired(ctx context.Context) bool {
	return c.hashFor(ctx, RoleViewer) != ""
}

// anyPasswordSet reports whether logging in is possible at all, which is what decides whether
// the login page is worth serving.
func (c *console) anyPasswordSet(ctx context.Context) bool {
	return c.hashFor(ctx, RoleViewer) != "" || c.hashFor(ctx, RoleAdmin) != ""
}

// RoleFromUsername maps what somebody typed into the username field.
//
// The form asks for a username as well as a password, and the username *is* the role. That is
// partly a password-manager problem — a password-only form gives a manager nothing to label the
// entry with, so an operator's admin and viewer passwords collapse into one indistinguishable
// item, which was the reported complaint — and partly a clarity one: the role is now what the
// person asked for rather than something inferred from which of two hashes happened to match.
//
// Case and surrounding space are forgiven. "Admin " typed into a form is not a different user.
func RoleFromUsername(s string) Role {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleAdmin:
		return RoleAdmin
	case RoleViewer:
		return RoleViewer
	default:
		return RoleNone
	}
}

// Verify checks a password against one role's stored hash.
//
// One role, named by the caller, rather than "try both and return whichever matched". The
// earlier version had to check admin first so that identical passwords granted admin rather than
// viewer, and had to check both every time so the timing did not reveal which one matched. With
// the role stated, neither problem exists.
//
// An unknown role, or one with no password set, is refused — and refused *after* the same work
// as a wrong password, because an early return would let somebody time the difference between
// "no viewer password is configured" and "your viewer password is wrong". The first is a fact
// about the installation and worth not leaking on a page reachable from the internet.
func (c *console) Verify(ctx context.Context, role Role, password string) bool {
	hash := ""
	if role == RoleAdmin || role == RoleViewer {
		hash = c.hashFor(ctx, role)
	}
	if hash == "" {
		// A decoy of the same shape, so an unset role costs the same argon2 derivation as a
		// set one. The value is fixed and matches nothing.
		VerifyPassword(decoyHash, password)
		return false
	}
	return VerifyPassword(hash, password)
}

// decoyHash is a valid argon2id hash of a value nobody has.
//
// Its only purpose is to make the "no password set for this role" path cost the same as a real
// verification. Generated once and pasted here rather than derived at startup: a hash computed
// per process would be indistinguishable in cost, but a constant makes it obvious to a reader
// that nothing is being checked against it.
const decoyHash = "$argon2id$v=19$m=65536,t=2,p=4$" +
	"ZGVjb3lzYWx0ZGVjb3lzYQ$3RaGmp5xrtUdWjSPWMkbHUxa9U8UYbCLg8O5wJHOYQA"

// consoleCookie is the session cookie's name.
const consoleCookie = "docpreview_session"

// issue mints a session token: the role, an expiry, and an HMAC over both.
//
// Stateless on purpose. A server-side session table would have to be reaped, would not survive
// the restart that the signing key does not survive either, and buys nothing here — there are
// two principals and "log out everywhere" is a restart.
//
// The role is inside the signed body, which is the whole point: a viewer editing the cookie to
// say `admin` invalidates the signature.
func (c *console) issue(role Role, now time.Time) string {
	body := fmt.Sprintf("%d:%s", now.Add(sessionTTL).Unix(), role)
	c.mu.RLock()
	signer := c.signer
	c.mu.RUnlock()
	mac := hmac.New(sha256.New, signer)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// roleOf returns the role a token carries, or RoleNone if it is not one this process issued,
// has expired, or names a role that no longer means anything.
func (c *console) roleOf(token string, now time.Time) Role {
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return RoleNone
	}
	c.mu.RLock()
	signer := c.signer
	c.mu.RUnlock()
	mac := hmac.New(sha256.New, signer)
	mac.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	// Constant-time, for the same reason the password comparison is: a byte-at-a-time
	// comparison against a forged signature is a forgeable signature given enough attempts.
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return RoleNone
	}

	expStr, roleStr, ok := strings.Cut(body, ":")
	if !ok {
		return RoleNone
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || !now.Before(time.Unix(exp, 0)) {
		return RoleNone
	}
	switch Role(roleStr) {
	case RoleAdmin:
		return RoleAdmin
	case RoleViewer:
		return RoleViewer
	default:
		return RoleNone
	}
}

// roleOfRequest is the role a request's cookie carries.
func (c *console) roleOfRequest(r *http.Request) Role {
	ck, err := r.Cookie(consoleCookie)
	if err != nil {
		return RoleNone
	}
	return c.roleOf(ck.Value, time.Now())
}

// setCookie writes the session cookie.
//
// HttpOnly so a script cannot read it, SameSite=Lax so a cross-site form post cannot use it
// while an ordinary link into the dashboard still works. Not marked Secure: the daemon serves
// plain HTTP and TLS terminates at whatever fronts it, so Secure would make the cookie unusable
// in the arrangement this exists for.
func setConsoleCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     consoleCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func clearConsoleCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     consoleCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
