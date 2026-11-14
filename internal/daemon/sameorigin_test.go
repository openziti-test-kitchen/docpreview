package daemon

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestCrossSiteWritesAreRefused: SameSite=Lax does not stop a same-site page, and every zrok
// share and every loopback port is same-site with the dashboard. The browser's own statement of
// where a request came from is what is checked.
func TestCrossSiteWritesAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		path    string
		host    string
		headers map[string]string
		refused bool
	}{
		{name: "the dashboard's own fetch", method: "POST", path: "/api/builds/x/rebuild",
			headers: map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:8471"}},
		{name: "a typed address", method: "POST", path: "/logout",
			headers: map[string]string{"Sec-Fetch-Site": "none"}},
		{name: "curl or the CLI, which send neither header", method: "POST", path: "/api/builds/x/rebuild"},
		{name: "another zrok share", method: "POST", path: "/api/builds/x/rebuild",
			headers: map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.shares.zrok.io"}, refused: true},
		{name: "another site", method: "POST", path: "/api/builds/x/rebuild",
			headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, refused: true},
		{name: "another site, DELETE", method: "DELETE", path: "/api/secrets/k",
			headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, refused: true},
		{name: "a sandboxed local preview", method: "POST", path: "/logout",
			headers: map[string]string{"Origin": "null"}, refused: true},
		{name: "plain http, same host", method: "POST", path: "/api/builds/x/rebuild", host: "10.0.0.5:8471",
			headers: map[string]string{"Origin": "http://10.0.0.5:8471"}},
		{name: "plain http, another port on the host", method: "POST", path: "/api/builds/x/rebuild", host: "10.0.0.5:8471",
			headers: map[string]string{"Origin": "http://10.0.0.5:9000"}, refused: true},
		{name: "through the public proxy", method: "POST", path: "/login", host: "127.0.0.1:8471",
			headers: map[string]string{"Origin": "https://dash.shares.zrok.io", "X-Forwarded-Host": "dash.shares.zrok.io"}},
		{name: "through the public proxy, from another share", method: "POST", path: "/login", host: "127.0.0.1:8471",
			headers: map[string]string{"Origin": "https://evil.shares.zrok.io", "X-Forwarded-Host": "dash.shares.zrok.io"}, refused: true},
		{name: "a cross-site GET is a navigation, and reads nothing", method: "GET", path: "/",
			headers: map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{name: "a webhook delivery is judged by its signature", method: "POST", path: "/webhook/github",
			headers: map[string]string{"Sec-Fetch-Site": "cross-site"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.host != "" {
				r.Host = tc.host
			}
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			reached := false
			h := sameOriginWrites(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if tc.refused && (reached || w.Code != http.StatusForbidden) {
				t.Errorf("reached the handler = %v, status %d; want a 403", reached, w.Code)
			}
			if !tc.refused && !reached {
				t.Errorf("refused with %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestTheWholeIngressIsBehindTheCrossSiteCheck: the check is only worth anything wrapped around
// every route, the login-exempt ones included.
func TestTheWholeIngressIsBehindTheCrossSiteCheck(t *testing.T) {
	ingress, _, _ := testIngress(t, &fakeClient{})
	h := ingress.Handler()
	for _, path := range []string{"/logout", "/login", "/api/builds/x/rebuild", "/api/secrets/unlock"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		r.Header.Set("Sec-Fetch-Site", "same-site")
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("same-site POST %s = %d, want 403", path, w.Code)
		}
	}
}

// TestOnlyDeclaredJSONIsDecoded: text/plain, a form and an untyped body all cross origins with no
// preflight, and a JSON decoder would read any of them.
func TestOnlyDeclaredJSONIsDecoded(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":                  true,
		"application/json; charset=utf-8":   true,
		"Application/JSON":                  true,
		"":                                  false,
		"text/plain":                        false,
		"text/plain; x=application/json":    false,
		"application/x-www-form-urlencoded": false,
		"multipart/form-data; boundary=x":   false,
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(`{"a":1}`))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		var into map[string]any
		err := decodeJSON(r, &into)
		if (err == nil) != want {
			t.Errorf("Content-Type %q: decodeJSON error = %v, want accepted = %v", ct, err, want)
		}
	}
}

// TestAForwardingHeaderCountsHoweverItIsSpelled: a request is private only when no proxy
// forwarded it, and a header whose first value is empty is still a header.
func TestAForwardingHeaderCountsHoweverItIsSpelled(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:40000"
	if forwarded(r) {
		t.Fatal("a request with no forwarding header reads as forwarded")
	}
	r.Header["X-Forwarded-For"] = []string{"", "203.0.113.9"}
	if !forwarded(r) {
		t.Error("an empty first X-Forwarded-For value hid the second")
	}
	r.Header = http.Header{"X-Forwarded-Host": []string{""}}
	if !forwarded(r) {
		t.Error("an empty X-Forwarded-Host reads as not forwarded")
	}
	if privatePath(r) {
		t.Error("a forwarded loopback request is treated as private")
	}
}

// TestPagesRefuseFramingAndTheLoginPageRunsOnlyItsOwnScript.
func TestPagesRefuseFramingAndTheLoginPageRunsOnlyItsOwnScript(t *testing.T) {
	i, _ := loginFixture(t)

	w := httptest.NewRecorder()
	i.dashboard(w, httptest.NewRequest(http.MethodGet, "/", nil))
	for name, page := range map[string]*httptest.ResponseRecorder{"dashboard": w, "login": func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		i.serveLogin(w, "/", "", http.StatusOK)
		return w
	}()} {
		h := page.Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'", "object-src 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: Content-Security-Policy %q lacks %q", name, csp, want)
			}
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", name, h.Get("X-Frame-Options"))
		}
		if h.Get("Cross-Origin-Opener-Policy") != "same-origin" {
			t.Errorf("%s: Cross-Origin-Opener-Policy = %q", name, h.Get("Cross-Origin-Opener-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", name, h.Get("X-Content-Type-Options"))
		}
	}

	// The login page's policy names one script by hash. Computed here from the page as it was
	// served, so an edit to the script that forgets the policy fails this rather than a browser.
	login := httptest.NewRecorder()
	i.serveLogin(login, "/", "", http.StatusOK)
	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(login.Body.String(), -1)
	if len(scripts) != 1 {
		t.Fatalf("the login page has %d scripts, want 1", len(scripts))
	}
	sum := sha256.Sum256([]byte(scripts[0][1]))
	want := "script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	csp := login.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, want) {
		t.Errorf("login Content-Security-Policy = %q, want it to contain %q", csp, want)
	}
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("login Content-Security-Policy is not closed by default: %q", csp)
	}
	// The dashboard the same way: its one script, by hash, and nothing inline besides.
	board := w.Body.String()
	scripts = regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(board, -1)
	if len(scripts) != 1 || strings.Count(board, "<script") != 1 {
		t.Fatalf("the dashboard has %d plain scripts and %d script tags, want 1 and 1",
			len(scripts), strings.Count(board, "<script"))
	}
	sum = sha256.Sum256([]byte(strings.ReplaceAll(scripts[0][1], "\r\n", "\n")))
	want = "script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "';"
	csp = w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, want) || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("dashboard Content-Security-Policy = %q, want it to contain %q and no 'unsafe-inline'", csp, want)
	}
	// A handler attribute or a javascript: link in the page's own markup would be blocked by
	// that policy, and the control it belonged to would silently stop working.
	if m := regexp.MustCompile(`<[a-zA-Z][^<>]*\son[a-z]+\s*=\s*["'$]|=\s*["']?javascript:`).FindString(board); m != "" {
		t.Errorf("the dashboard has inline script its policy blocks: %q", m)
	}
	if strings.Contains(login.Body.String(), " onclick=") || strings.Contains(login.Body.String(), " onload=") {
		t.Error("the login page has an inline handler, which its policy blocks")
	}
}
