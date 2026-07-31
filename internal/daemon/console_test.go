package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/store"
)

func testConsole(t *testing.T) *console {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c, err := newConsole(st)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The hash is salted, so the same password stored twice does not produce the same value.
//
// This is the property that makes the obvious design — age-encrypt the password and compare
// ciphertexts — unnecessary *and* the reason it could never have worked: the comparison has to
// re-derive from the stored salt, not compare stored bytes.
func TestHashPasswordIsSaltedAndVerifiable(t *testing.T) {
	const pw = "correct horse battery staple"

	a, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("the same password hashed to the same value twice, so the salt is not random")
	}
	for _, h := range []string{a, b} {
		if !VerifyPassword(h, pw) {
			t.Error("the correct password did not verify")
		}
		if VerifyPassword(h, pw+"!") {
			t.Error("a wrong password verified")
		}
	}

	// The parameters travel with the hash, so raising them later leaves stored passwords
	// verifiable rather than locking everybody out.
	if !strings.HasPrefix(a, "$argon2id$v=19$m=") {
		t.Errorf("hash is not in the PHC form: %q", a)
	}
	// And the plaintext is not in it, which is the whole point.
	if strings.Contains(a, "horse") {
		t.Error("the encoded hash contains the password")
	}
}

// Anything that is not a hash this code wrote is a failure — not a panic, and not a pass.
//
// A corrupt or truncated stored value must refuse everybody rather than accidentally
// accepting, and an empty stored hash accepting an empty password would be the worst version
// of that bug.
//
// The `$$` case found a genuine crash rather than a wrong answer: an empty salt and hash gave
// `argon2.IDKey` a zero key length, which dereferences a nil digest inside blake2b and panics.
// That was reachable from a login attempt, so a corrupt settings row was a way to take the
// daemon down. Every entry below stays in this list for that reason.
func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"", "notahash", "$argon2id$",
		"$argon2id$v=19$m=65536,t=2,p=4$$",
		"$argon2id$v=19$m=65536,t=2,p=4$c2FsdA$",
		"$argon2id$v=19$m=65536,t=2,p=4$$aGFzaA",
		"$argon2id$v=19$m=0,t=0,p=0$c2FsdA$aGFzaA",
		"$bcrypt$v=19$m=65536,t=2,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=bad,t=2,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=2,p=4$!!!notbase64$aGFzaA",
	} {
		if VerifyPassword(bad, "") {
			t.Errorf("VerifyPassword(%q, \"\") accepted an empty password", bad)
		}
		if VerifyPassword(bad, "anything") {
			t.Errorf("VerifyPassword(%q, …) accepted a password", bad)
		}
	}
}

// No password set means the gate is off, which is what every existing installation gets.
//
// Requiring one that nobody has set would lock an operator out of the surface that sets it —
// the same ordering trap that decides where the hashes are stored.
func TestNoPasswordMeansTheGateIsOff(t *testing.T) {
	c := testConsole(t)
	ctx := context.Background()

	if c.loginRequired(ctx) {
		t.Error("a fresh installation demands a login")
	}
	if c.anyPasswordSet(ctx) {
		t.Error("a fresh installation reports a password as set")
	}
	for _, role := range []Role{RoleAdmin, RoleViewer} {
		if c.Verify(ctx, role, "") || c.Verify(ctx, role, "anything") {
			t.Errorf("verification as %q succeeded with nothing stored", role)
		}
	}
}

// Each role's password opens that role and not the other.
//
// The username names the role, so this is not "which hash matched" — it is "does the password
// they gave match the role they asked for". A viewer password submitted as admin must fail even
// though it is a perfectly good password.
func TestEachRoleGetsItsOwnPassword(t *testing.T) {
	c := testConsole(t)
	ctx := context.Background()

	if err := SetConsolePassword(ctx, c.store, RoleAdmin, "the-admin-password"); err != nil {
		t.Fatal(err)
	}
	if err := SetConsolePassword(ctx, c.store, RoleViewer, "the-viewer-password"); err != nil {
		t.Fatal(err)
	}

	if !c.Verify(ctx, RoleAdmin, "the-admin-password") {
		t.Error("the admin password did not open admin")
	}
	if !c.Verify(ctx, RoleViewer, "the-viewer-password") {
		t.Error("the viewer password did not open viewer")
	}

	// The crossings, which are the point of having two.
	if c.Verify(ctx, RoleAdmin, "the-viewer-password") {
		t.Error("the viewer password opened admin")
	}
	if c.Verify(ctx, RoleViewer, "the-admin-password") {
		t.Error("the admin password opened viewer")
	}
	if c.Verify(ctx, RoleAdmin, "neither-of-those") {
		t.Error("an unknown password opened admin")
	}
}

// A username that is not a role is refused, whatever password comes with it.
func TestAnUnknownUsernameIsRefused(t *testing.T) {
	c := testConsole(t)
	ctx := context.Background()
	if err := SetConsolePassword(ctx, c.store, RoleAdmin, "the-admin-password"); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"", "root", "administrator", "Admin ", "viewer"} {
		role := RoleFromUsername(name)
		if role == RoleAdmin && name != "Admin " {
			t.Errorf("%q mapped to admin", name)
		}
		// "Admin " is admin: case and surrounding space are forgiven, because a name typed
		// into a form with a trailing space is not a different user.
		if c.Verify(ctx, role, "the-admin-password") && RoleFromUsername(name) != RoleAdmin {
			t.Errorf("%q was accepted with the admin password", name)
		}
	}
	if RoleFromUsername("Admin ") != RoleAdmin {
		t.Error("a capitalised username with a trailing space was not recognised")
	}
	if RoleFromUsername("VIEWER") != RoleViewer {
		t.Error("an upper-case username was not recognised")
	}
}

// An admin password alone protects writes and leaves reading open.
//
// That is a reasonable arrangement — a dashboard anyone can read on a network you trust, with
// the write surfaces locked — and it is why the login page is gated on the *viewer* password
// rather than on "any password".
func TestAnAdminPasswordAloneDoesNotDemandALogin(t *testing.T) {
	c := testConsole(t)
	ctx := context.Background()

	if err := SetConsolePassword(ctx, c.store, RoleAdmin, "the-admin-password"); err != nil {
		t.Fatal(err)
	}
	if c.loginRequired(ctx) {
		t.Error("an admin password alone forces every reader to log in")
	}
	if !c.anyPasswordSet(ctx) {
		t.Error("logging in is impossible, so the login page would be unreachable")
	}
}

func TestSetPasswordRoundTripsAndClears(t *testing.T) {
	c := testConsole(t)
	ctx := context.Background()

	if err := SetConsolePassword(ctx, c.store, RoleViewer, "a-long-enough-password"); err != nil {
		t.Fatal(err)
	}
	if !c.loginRequired(ctx) {
		t.Fatal("the viewer password did not take effect")
	}

	// It is read from the store on every check rather than cached, so a second process — the
	// CLI setting the first password on a running daemon — takes effect at once.
	fresh, err := newConsole(c.store)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Verify(ctx, RoleViewer, "a-long-enough-password") {
		t.Error("another process could not verify the password this one set")
	}

	if err := ClearConsolePassword(ctx, c.store, RoleViewer); err != nil {
		t.Fatal(err)
	}
	if c.loginRequired(ctx) {
		t.Error("clearing left the password set")
	}
}

// Short passwords are refused, because nothing rate-limits this surface.
//
// There is no lockout and no attempt counter, so the only cost per guess is one argon2
// derivation. A five-character password behind that is a formality.
func TestSetPasswordRefusesSomethingTooShort(t *testing.T) {
	c := testConsole(t)
	err := SetConsolePassword(context.Background(), c.store, RoleAdmin, "short")
	if err == nil {
		t.Fatal("a five-character password was accepted")
	}
	if !strings.Contains(err.Error(), "12") {
		t.Errorf("the refusal does not say how long it must be: %v", err)
	}
}

func TestSetConsolePasswordRejectsAnUnknownRole(t *testing.T) {
	c := testConsole(t)
	if err := SetConsolePassword(context.Background(), c.store, Role("root"), "a-long-password"); err == nil {
		t.Error("an unknown role was accepted")
	}
}

// A session is valid only if this process issued it, it has not expired, and its role survives.
func TestSessionTokens(t *testing.T) {
	c := testConsole(t)
	now := time.Now()

	for _, role := range []Role{RoleAdmin, RoleViewer} {
		tok := c.issue(role, now)
		if got := c.roleOf(tok, now); got != role {
			t.Errorf("a freshly issued %q token read back as %q", role, got)
		}
		if got := c.roleOf(tok, now.Add(sessionTTL+time.Minute)); got != RoleNone {
			t.Errorf("an expired token still granted %q", got)
		}
	}

	tok := c.issue(RoleViewer, now)
	body, sig, _ := strings.Cut(tok, ".")

	// The role is inside the signed body, which is the point: a viewer editing the cookie to
	// say admin breaks the signature.
	forged := strings.Replace(body, string(RoleViewer), string(RoleAdmin), 1) + "." + sig
	if got := c.roleOf(forged, now); got == RoleAdmin {
		t.Error("a viewer promoted themselves to admin by editing the cookie")
	}

	if got := c.roleOf(body+".x"+sig, now); got != RoleNone {
		t.Errorf("a forged signature granted %q", got)
	}
	// The expiry is signed too, so it cannot be extended.
	if got := c.roleOf("99999999999:viewer."+sig, now); got != RoleNone {
		t.Errorf("a rewritten expiry granted %q", got)
	}
	for _, bad := range []string{"", "garbage", "nodot", ":.", "123:root." + sig} {
		if got := c.roleOf(bad, now); got != RoleNone {
			t.Errorf("roleOf(%q) = %q", bad, got)
		}
	}

	// A second process cannot validate the first one's tokens. That is the trade for keeping no
	// signing key on disk: a restart is a logout.
	other := testConsole(t)
	if got := other.roleOf(tok, now); got != RoleNone {
		t.Errorf("another process accepted the token as %q", got)
	}
}

func TestRoleOfRequestReadsTheCookie(t *testing.T) {
	c := testConsole(t)
	tok := c.issue(RoleAdmin, time.Now())

	r := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	if got := c.roleOfRequest(r); got != RoleNone {
		t.Errorf("a request with no cookie was %q", got)
	}

	r.AddCookie(&http.Cookie{Name: consoleCookie, Value: tok})
	if got := c.roleOfRequest(r); got != RoleAdmin {
		t.Errorf("a request carrying an admin session was %q", got)
	}

	bad := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	bad.AddCookie(&http.Cookie{Name: consoleCookie, Value: tok + "x"})
	if got := c.roleOfRequest(bad); got != RoleNone {
		t.Errorf("a tampered session was %q", got)
	}
}

// The cookie must not be readable by a script, and must not ride along on a cross-site POST.
//
// The dashboard is a page full of buttons that change what runs on the build host; a
// script-readable session cookie turns any content injection into a full compromise of it.
func TestTheSessionCookieIsHardened(t *testing.T) {
	w := httptest.NewRecorder()
	setConsoleCookie(w, "token")

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies set, want 1", len(cookies))
	}
	ck := cookies[0]
	if !ck.HttpOnly {
		t.Error("the session cookie is readable by scripts")
	}
	if ck.SameSite != http.SameSiteLaxMode && ck.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie has no SameSite protection")
	}
	if ck.Path != "/" {
		t.Errorf("cookie path is %q", ck.Path)
	}
}
