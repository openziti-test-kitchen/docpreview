package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A fragment survives the login.
//
// Every pull request comment for a build with no URL links to `/#preview=<id>`, and the fragment
// is the only part of that link carrying which preview. A reader who is not signed in lands on the
// login form, and if the fragment is dropped there they arrive at the dashboard with no idea which
// of twenty previews they clicked.
//
// The fragment reaches the form through the browser rather than through this daemon — it is never
// sent to a server — so what is tested here is the half this code owns: that a `next` carrying one
// is honoured rather than reduced to "/".
func TestALoginKeepsTheFragmentItWasSentTo(t *testing.T) {
	i, _ := loginFixture(t)
	if err := SetConsolePassword(context.Background(), i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader("username=viewer&password=a-viewer-password&next=/%23preview=e13c067994e2"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	i.login(w, r)

	if got, want := w.Header().Get("Location"), "/#preview=e13c067994e2"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

// safeNext is the one decision about where a login may send somebody, so every shape it has to
// refuse is enumerated in one place rather than discovered at each call site.
func TestSafeNextRefusesEverythingThatIsNotAPathHere(t *testing.T) {
	for _, tc := range []struct {
		next string
		want string
	}{
		{"/", "/"},
		{"/projects", "/projects"},
		{"/#preview=e13c067994e2", "/#preview=e13c067994e2"},
		{"/?q=1#frag", "/?q=1#frag"},

		// An open redirect, in each of its disguises. The victim has just typed a password.
		{"https://evil.example/", "/"},
		{"//evil.example/", "/"},
		{"http://evil.example", "/"},
		{"evil.example", "/"},
		{"", "/"},

		// Response splitting. No browser produces these, so nothing is lost by refusing them.
		{"/ok\r\nLocation: https://evil.example", "/"},
		{"/ok\nSet-Cookie: x=1", "/"},
		{"/ok with a space", "/"},
	} {
		if got := safeNext(tc.next); got != tc.want {
			t.Errorf("safeNext(%q) = %q, want %q", tc.next, got, tc.want)
		}
	}
}

// The login page carries the script that puts the fragment back.
//
// Asserted on the page rather than only on safeNext, because the two halves are useless apart: a
// server that honours a fragment in `next` never sees one if the form does not put it there.
func TestTheLoginPageRestoresTheFragment(t *testing.T) {
	i, _ := loginFixture(t)
	if err := SetConsolePassword(context.Background(), i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	i.login(w, httptest.NewRequest(http.MethodGet, "/login?next=%2F", nil))
	body := w.Body.String()

	for _, want := range []string{"location.hash", `input[name="next"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("the login page does not mention %s, so a fragment is dropped at sign-in", want)
		}
	}
}
