package preview

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// realSiteDir locates the built www/ output, if the docs site has been built.
//
// This test is the closest thing to the real thing that can run without a
// network: it serves the actual Docusaurus output through the actual preview
// handler and checks that a browser would get working pages. It skips rather
// than fails when the site has not been built, so `go test ./...` on a fresh
// clone stays green.
func realSiteDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "www", "build"))
	if err != nil {
		t.Skip("cannot resolve www/build")
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		t.Skip("www/build not present; run `npm run build` in www/ to enable this test")
	}
	return dir
}

// detectedBaseURL reads the base URL out of the built site the same way the
// builder does, so this test works whichever way www/ was last built.
func detectedBaseURL(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "href=/zrok/") || strings.Contains(string(raw), `href="/zrok/`) {
		return "/zrok/"
	}
	return "/"
}

func TestServeRealDocusaurusBuild(t *testing.T) {
	dir := realSiteDir(t)
	baseURL := detectedBaseURL(t, dir)

	normalized, err := config.NormalizeBaseURL(baseURL)
	if err != nil {
		t.Fatal(err)
	}

	site, err := New(dir, normalized)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(site)
	defer srv.Close()

	// Fetch the homepage, then fetch every asset it references and confirm
	// none of them 404. This is exactly the failure the base URL check exists
	// to prevent, verified end to end rather than by inspection.
	resp, err := http.Get(srv.URL + normalized)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", normalized, resp.StatusCode)
	}

	refs := extractLocalRefs(string(body))
	if len(refs) < 3 {
		t.Fatalf("found only %d local references in the built homepage; the site did not render", len(refs))
	}

	for _, ref := range refs {
		r, err := http.Get(srv.URL + ref)
		if err != nil {
			t.Errorf("GET %s: %v", ref, err)
			continue
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 — an asset the homepage references is not served", ref, r.StatusCode)
		}
	}
}

func TestRealBuildBareRootIsUsable(t *testing.T) {
	dir := realSiteDir(t)
	baseURL := detectedBaseURL(t, dir)
	if baseURL == "/" {
		t.Skip("site was built at the root; nothing to redirect")
	}

	site, err := New(dir, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(site)
	defer srv.Close()

	// The default client follows redirects, so this exercises the whole path a
	// reviewer takes when they click the bare hostname.
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d after redirects, want 200", resp.StatusCode)
	}
}

// extractLocalRefs pulls same-origin href and src values out of HTML, handling
// the unquoted attributes Docusaurus emits.
func extractLocalRefs(html string) []string {
	var out []string
	seen := map[string]bool{}

	for _, attr := range []string{"href=", "src="} {
		rest := html
		for {
			i := strings.Index(rest, attr)
			if i < 0 {
				break
			}
			rest = rest[i+len(attr):]

			var value string
			switch {
			case strings.HasPrefix(rest, `"`):
				value, _, _ = strings.Cut(rest[1:], `"`)
			case strings.HasPrefix(rest, `'`):
				value, _, _ = strings.Cut(rest[1:], `'`)
			default:
				value = rest
				if j := strings.IndexAny(value, " \t\n>"); j >= 0 {
					value = value[:j]
				}
			}

			// Same-origin paths only, and skip anchors and query strings.
			if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") &&
				!strings.ContainsAny(value, "#?") && !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
	}
	return out
}
