package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/store"
)

// loginFixture is an ingress whose only route echoes, wrapped in the login gate.
//
// The handler is a stub because what is under test is the middleware: which requests reach a
// handler at all, and what role they carry when they do.
func loginFixture(t *testing.T) (*Ingress, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	i := &Ingress{log: slog.New(slog.DiscardHandler)}
	if _, err := i.WithLogin(st); err != nil {
		t.Fatal(err)
	}
	reached := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Role", string(roleOfContext(r.Context())))
		w.WriteHeader(http.StatusOK)
	})
	return i, i.requireLogin(reached)
}

// loginGet issues a request from off-machine. The address is set explicitly rather than left at
// httptest's default because where a request comes from must make no difference here, and a test
// that only ever asks from one place cannot show that.
func loginGet(h http.Handler, method, path string, cookie string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "203.0.113.9:44444"
	r.Header.Set("Accept", "text/html")
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: consoleCookie, Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// With no viewer password nothing is gated, which is what every installation had before this
// existed. A feature that locks people out on upgrade is not shippable.
func TestWithNoViewerPasswordEverythingIsOpen(t *testing.T) {
	_, h := loginFixture(t)
	for _, p := range []string{"/", "/status", "/logs/abc", "/pr"} {
		if got := loginGet(h, http.MethodGet, p, "").Code; got != http.StatusOK {
			t.Errorf("GET %s = %d with no password set, want 200", p, got)
		}
	}
}

// With one set, the dashboard asks for a login — and the four exemptions do not.
func TestAViewerPasswordGatesTheDashboardButNotTheExemptions(t *testing.T) {
	i, h := loginFixture(t)
	ctx := context.Background()
	if err := SetConsolePassword(ctx, i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}

	// Gated: a browser is redirected to the form rather than shown the page.
	for _, p := range []string{"/", "/status", "/events", "/logs/abc", "/pr"} {
		w := loginGet(h, http.MethodGet, p, "")
		if w.Code != http.StatusSeeOther {
			t.Errorf("GET %s = %d, want a redirect to the login page", p, w.Code)
		}
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Errorf("GET %s redirected to %q", p, loc)
		}
	}

	// Not gated, and each for a different reason — see openPath. A webhook is HMAC-verified
	// and its caller has no cookie; /healthz is polled by a supervisor that would otherwise
	// restart the daemon in a loop; a preview URL is pasted into a pull request for a reviewer
	// who has no login; and the login page cannot be behind the login page.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/webhook/github"},
		{http.MethodPost, "/webhook/bitbucket"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/preview/docs-main/"},
		{http.MethodGet, "/login"},
	} {
		if got := loginGet(h, tc.method, tc.path, "").Code; got != http.StatusOK {
			t.Errorf("%s %s = %d, want it served without a login", tc.method, tc.path, got)
		}
	}
}

// A request from the machine itself must not skip the login.
//
// It used to, as a bootstrap convenience, and the reasoning was that anyone who can reach
// 127.0.0.1 can already run the binary. The hole is that "from the machine itself" is not
// something the daemon can establish: a tunnel in proxy mode connects from a local process, so
// RemoteAddr is loopback for a request that arrived from the internet. Anything that proxies
// without setting a forwarding header therefore hands every visitor an admin session.
//
// Both forms are asserted, because the earlier version admitted the plain loopback request and
// only the forwarding header stopped it — so testing the header alone would have passed against
// the code this test exists to keep deleted.
func TestLoopbackIsNotExemptFromTheLogin(t *testing.T) {
	i, h := loginFixture(t)
	if err := SetConsolePassword(context.Background(), i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{name: "plain loopback"},
		{name: "loopback behind a proxy", headers: map[string]string{"X-Forwarded-For": "203.0.113.9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/status", nil)
			r.RemoteAddr = "127.0.0.1:51000"
			r.Header.Set("Accept", "text/html")
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != http.StatusSeeOther {
				t.Fatalf("GET /status from loopback = %d, want a redirect to the login page", w.Code)
			}
			if role := w.Header().Get("X-Role"); role != "" {
				t.Errorf("the request reached the handler carrying role %q", role)
			}
		})
	}
}

// The bootstrap the loopback exemption was there for still works: until a password exists, the
// local operator reaches everything. That is what makes removing the exemption safe rather than
// a lockout on upgrade.
func TestLoopbackIsOpenUntilAPasswordExists(t *testing.T) {
	_, h := loginFixture(t)

	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.RemoteAddr = "127.0.0.1:51000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /status from loopback with no password set = %d, want 200", w.Code)
	}
}

// An API caller gets a 401 and a sentence rather than a redirect to a form.
//
// A fetch that receives an HTML login page has no way to report what went wrong: it either
// parses the page as JSON and fails with a syntax error, or renders it into the dashboard.
func TestAnAPICallerGetsA401RatherThanAForm(t *testing.T) {
	i, h := loginFixture(t)
	if err := SetConsolePassword(context.Background(), i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	r.RemoteAddr = "203.0.113.9:44444"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/projects = %d, want 401", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("content type is %q, want JSON", ct)
	}
}

// A session reaches the handler, carrying its role.
func TestASessionReachesTheHandlerWithItsRole(t *testing.T) {
	i, h := loginFixture(t)
	ctx := context.Background()
	if err := SetConsolePassword(ctx, i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}

	for _, role := range []Role{RoleViewer, RoleAdmin} {
		w := loginGet(h, http.MethodGet, "/status", i.console.issue(role, time.Now()))
		if w.Code != http.StatusOK {
			t.Errorf("a %s session got %d", role, w.Code)
		}
		if got := w.Header().Get("X-Role"); got != string(role) {
			t.Errorf("the handler saw role %q, want %q", got, role)
		}
	}
}

// Logging in with the viewer password must not produce an admin session.
//
// The whole role split rests on this, and the failure would be silent: a viewer would simply
// find every button working.
func TestLoginIssuesTheRoleThePasswordEarns(t *testing.T) {
	i, _ := loginFixture(t)
	ctx := context.Background()
	if err := SetConsolePassword(ctx, i.console.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}
	if err := SetConsolePassword(ctx, i.console.store, RoleAdmin, "an-admin-password"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		password string
		want     Role
	}{
		{"a-viewer-password", RoleViewer},
		{"an-admin-password", RoleAdmin},
	} {
		r := httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader("username="+string(tc.want)+"&password="+tc.password))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "203.0.113.9:44444"
		w := httptest.NewRecorder()
		i.login(w, r)

		if w.Code != http.StatusSeeOther {
			t.Fatalf("login with the %s password = %d, want a redirect", tc.want, w.Code)
		}
		var token string
		for _, ck := range w.Result().Cookies() {
			if ck.Name == consoleCookie {
				token = ck.Value
			}
		}
		if token == "" {
			t.Fatalf("no session cookie was set for the %s password", tc.want)
		}
		if got := i.console.roleOf(token, time.Now()); got != tc.want {
			t.Errorf("the %s password produced a %q session", tc.want, got)
		}
	}
}

// A wrong password says one thing, whichever password it was closest to.
func TestAWrongPasswordIsNotAnOracle(t *testing.T) {
	i, _ := loginFixture(t)
	if err := SetConsolePassword(context.Background(), i.console.store, RoleAdmin, "an-admin-password"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=admin&password=wrong"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	i.login(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password = %d, want 401", w.Code)
	}
	body := w.Body.String()
	for _, leak := range []string{"admin", "viewer"} {
		if strings.Contains(strings.ToLower(body), leak+" password was") {
			t.Errorf("the page says which password was wrong: %q", leak)
		}
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("a failed login set a cookie")
	}
}

// `next` is attacker-supplied, so it must never send a browser off-site.
//
// An open redirect on a login page is the classic phishing primitive, and worse here because the
// victim has just typed a password.
func TestLoginRefusesAnOffsiteNext(t *testing.T) {
	i, _ := loginFixture(t)
	ctx := context.Background()
	if err := SetConsolePassword(ctx, i.console.store, RoleAdmin, "an-admin-password"); err != nil {
		t.Fatal(err)
	}

	for _, next := range []string{
		"https://evil.example/", "//evil.example/", "http://evil.example",
	} {
		r := httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader("username=admin&password=an-admin-password&next="+next))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		i.login(w, r)

		if got := w.Header().Get("Location"); got != "/" {
			t.Errorf("next=%q redirected to %q, want /", next, got)
		}
	}

	// A path on this daemon is honoured, which is the point of carrying it at all.
	r := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader("username=admin&password=an-admin-password&next=/projects"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	i.login(w, r)
	if got := w.Header().Get("Location"); got != "/projects" {
		t.Errorf("an on-site next redirected to %q", got)
	}
}

// A viewer session is refused at the write gate, with a reason that says what to do.
//
// It must not fall through to the loopback checks: a viewer is a positive statement that this
// caller is not an admin, so treating one on the loopback interface as an admin would make
// signing in as a viewer grant *more* than not signing in at all.
func TestAViewerSessionCannotWrite(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/api/secrets/github.app_id", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	ctx := context.WithValue(r.Context(), roleKey{}, RoleViewer)

	ok, why := isLocalRequest(r.WithContext(ctx))
	if ok {
		t.Fatal("a viewer session was allowed to write")
	}
	if !strings.Contains(why, "admin password") {
		t.Errorf("the refusal does not say how to fix it: %q", why)
	}
}

// An admin session may write from anywhere, which is the entire point of having a password.
func TestAnAdminSessionMayWriteFromAnywhere(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/api/secrets/github.app_id", nil)
	r.RemoteAddr = "203.0.113.9:44444"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	ctx := context.WithValue(r.Context(), roleKey{}, RoleAdmin)

	if ok, why := isLocalRequest(r.WithContext(ctx)); !ok {
		t.Errorf("an admin session was refused: %s", why)
	}
}
