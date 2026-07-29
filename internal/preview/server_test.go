package preview

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func siteDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	must := func(rel, body string) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	must("index.html", "<html>home</html>")
	must("404.html", "<html>not found here</html>")
	must("assets/css/styles.css", "body{}")
	must("docs/intro/index.html", "<html>intro</html>")
	return dir
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestServeAtRoot(t *testing.T) {
	site, err := New(siteDir(t), "/")
	if err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]string{
		"/":                      "home",
		"/assets/css/styles.css": "body{}",
		"/docs/intro/":           "intro",
	} {
		rec := get(t, site, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s body = %q, want it to contain %q", path, rec.Body.String(), want)
		}
	}
}

func TestServeAtPrefix(t *testing.T) {
	// A site built with baseUrl "/zrok/" must be reachable under that prefix
	// and nowhere else, because that is where its own asset URLs point.
	site, err := New(siteDir(t), "/zrok/")
	if err != nil {
		t.Fatal(err)
	}

	rec := get(t, site, "/zrok/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "home") {
		t.Errorf("GET /zrok/ = %d %q", rec.Code, rec.Body.String())
	}

	rec = get(t, site, "/zrok/assets/css/styles.css")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /zrok/assets/css/styles.css = %d, want 200", rec.Code)
	}
}

func TestBareRootRedirectsToTheMountPoint(t *testing.T) {
	// A reviewer who clicks the origin without the path should land on the
	// site, not on a 404 that looks like a broken preview.
	site, err := New(siteDir(t), "/zrok/")
	if err != nil {
		t.Fatal(err)
	}

	rec := get(t, site, "/")
	if rec.Code != http.StatusFound {
		t.Fatalf("GET / = %d, want a redirect", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/zrok/" {
		t.Errorf("Location = %q, want %q", got, "/zrok/")
	}
}

func TestMissingPageServesTheSites404(t *testing.T) {
	site, err := New(siteDir(t), "/")
	if err != nil {
		t.Fatal(err)
	}

	rec := get(t, site, "/no/such/page")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not found here") {
		t.Errorf("body = %q, want the site's own 404 page", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "404 page not found") {
		t.Error("the Go file server's plain-text 404 leaked through")
	}
}

func TestPreviewHeaders(t *testing.T) {
	site, err := New(siteDir(t), "/")
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, site, "/")

	// Caching a preview means a reviewer refreshes and sees the version they
	// already rejected. Indexing one means unreleased docs in search results.
	for header, want := range map[string]string{
		"Cache-Control":          "no-store",
		"X-Robots-Tag":           "noindex",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
}

func TestNonReadMethodsRejected(t *testing.T) {
	site, err := New(siteDir(t), "/")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", rec.Code)
	}
}

func TestPathTraversalIsRefused(t *testing.T) {
	dir := siteDir(t)
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not serve me"), 0o600); err != nil {
		t.Fatal(err)
	}

	site, err := New(dir, "/")
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/../secret.txt",
		"/..%2Fsecret.txt",
		"/docs/../../secret.txt",
	} {
		rec := get(t, site, path)
		if strings.Contains(rec.Body.String(), "do not serve me") {
			t.Fatalf("GET %s escaped the site directory", path)
		}
	}
}

func TestNewRejectsAMissingDirectory(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "nope"), "/"); err == nil {
		t.Fatal("New accepted a directory that does not exist")
	}
}
