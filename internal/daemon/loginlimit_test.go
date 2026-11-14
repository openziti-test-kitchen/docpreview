package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postLogin posts a login form the way a browser on this machine would.
func postLogin(i *Ingress, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	i.login(w, r)
	return w
}

// TestLoginRefusesBeyondItsVerificationSlots: every attempt is a 64 MiB argon2 derivation on a
// page that needs no session, so unbounded concurrency is unbounded memory. With every slot
// taken, the next attempt is answered 429 and no password is checked, the right one included.
func TestLoginRefusesBeyondItsVerificationSlots(t *testing.T) {
	i, _ := loginFixture(t)
	if err := SetConsolePassword(context.Background(), i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}
	const form = "username=viewer&password=a-viewer-password&next=/"

	var releases []func()
	for {
		release, ok := takeLoginSlot(true)
		if !ok {
			break
		}
		releases = append(releases, release)
	}
	if len(releases) == 0 || len(releases) > 8 {
		t.Fatalf("%d verification slots; want a small number", len(releases))
	}

	w := postLogin(i, form)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("with every slot taken, POST /login = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("the 429 carries no Retry-After")
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("a refused login set a cookie")
	}

	for _, release := range releases {
		release()
	}

	// The slot is given back after a verification, or three logins would be the last three.
	for n := 0; n < len(releases)+2; n++ {
		if w := postLogin(i, form); w.Code != http.StatusSeeOther {
			t.Fatalf("login %d with the slots free = %d, want 303", n+1, w.Code)
		}
	}
}

// TestAFloodThroughTheProxyDoesNotLockOutTheHost: the public slots can be held by anybody who
// keeps posting wrong passwords, so one verification is kept for a request on a private path.
func TestAFloodThroughTheProxyDoesNotLockOutTheHost(t *testing.T) {
	i, _ := loginFixture(t)
	if err := SetConsolePassword(context.Background(), i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}
	const form = "username=viewer&password=a-viewer-password&next=/"

	for {
		release, ok := takeLoginSlot(false)
		if !ok {
			break
		}
		defer release()
	}

	proxied := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	proxied.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	proxied.Header.Set("X-Forwarded-For", "203.0.113.9")
	proxied.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()
	i.login(w, proxied)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("a proxied login with the public slots taken = %d, want 429", w.Code)
	}

	for n := 0; n < 2; n++ {
		direct := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
		direct.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		direct.RemoteAddr = "127.0.0.1:50000"
		w := httptest.NewRecorder()
		i.login(w, direct)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("login %d from the host with the public slots taken = %d, want 303", n+1, w.Code)
		}
	}
}

// TestLoginFormIsCapped: the form is three short fields, and reading an unbounded body on an
// open page is a second way to spend the daemon's memory.
func TestLoginFormIsCapped(t *testing.T) {
	i, _ := loginFixture(t)
	if err := SetConsolePassword(context.Background(), i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}

	big := "username=viewer&password=" + strings.Repeat("x", loginFormLimit+1)
	if w := postLogin(i, big); w.Code != http.StatusBadRequest {
		t.Errorf("an oversized login form = %d, want 400", w.Code)
	}
}
