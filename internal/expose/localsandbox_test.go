package expose

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestLocalPreviewsAreSandboxedOffTheDashboardOrigin: a preview under /preview/ is served by the
// dashboard's own listener, so a pull request's script would otherwise run with the dashboard's
// origin and the visitor's session cookie. Every response under the prefix has to carry the
// sandbox, the 404s too, and it must never grant allow-same-origin.
func TestLocalPreviewsAreSandboxedOffTheDashboardOrigin(t *testing.T) {
	ex := localOnServer(t)

	site := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<script>fetch('/api/secrets')</script>")
	})
	pub, err := ex.Publish(context.Background(), Spec{PreviewID: "abc123", Name: "feature-x", BaseURL: "/"}, site)
	if err != nil {
		t.Fatal(err)
	}

	for _, url := range []string{
		pub.URL,
		pub.URL + "assets/app.js",
		ex.origin + MountPrefix + "never-published/",
		ex.origin + MountPrefix,
	} {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		resp.Body.Close()

		csp := resp.Header.Get("Content-Security-Policy")
		directives := strings.Fields(csp)
		if len(directives) == 0 || directives[0] != "sandbox" {
			t.Errorf("GET %s (%d): Content-Security-Policy = %q, want a sandbox", url, resp.StatusCode, csp)
			continue
		}
		for _, d := range directives[1:] {
			if d == "allow-same-origin" || d == "allow-top-navigation" ||
				d == "allow-top-navigation-by-user-activation" {
				t.Errorf("GET %s: the sandbox grants %s, which hands the preview the dashboard's origin", url, d)
			}
		}
		if !strings.Contains(csp, "allow-scripts") {
			t.Errorf("GET %s: the sandbox blocks scripts, so no documentation site would render: %q", url, csp)
		}
	}
}

// TestASandboxedPreviewCanReadItsOwnFiles: the sandbox makes the preview's own scripts, fonts and
// data another origin to it, and a browser asks for them with `Origin: null`. Without an answer
// that lets that origin read, a module script never runs and the page is blank.
func TestASandboxedPreviewCanReadItsOwnFiles(t *testing.T) {
	ex := localOnServer(t)
	site := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "export {}") })
	pub, err := ex.Publish(context.Background(), Spec{PreviewID: "abc123", Name: "feature-x", BaseURL: "/"}, site)
	if err != nil {
		t.Fatal(err)
	}

	for origin, want := range map[string]string{
		"null":                 "*",
		"":                     "",
		"https://elsewhere.io": "",
	} {
		req, err := http.NewRequest(http.MethodGet, pub.URL+"assets/app.js", nil)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("Origin %q: Access-Control-Allow-Origin = %q, want %q", origin, got, want)
		}
		if resp.Header.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("Origin %q: the preview allows credentialed reads", origin)
		}
	}
}

// TestPreviewsWithTheirOwnHostAreNotSandboxed: a Frontdoor preview is served from a port of its
// own behind a hostname of its own, and zrok and ziti serve the site handler directly. Sandboxing
// those would take their storage away for nothing.
func TestPreviewsWithTheirOwnHostAreNotSandboxed(t *testing.T) {
	fd, _ := newTestFrontdoor(t, &fakeFrontdoor{})

	share, err := fd.ports.serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLocal(share) })

	resp, err := http.Get("http://" + share.listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
		t.Errorf("a preview on its own port was sent %q", csp)
	}
}
