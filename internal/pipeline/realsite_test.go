package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyBaseURLOnTheRealLandingPage runs the check against this repository's
// own built site.
//
// It exists because the landing page is what broke the check. The page links into
// /docs/ thirteen times across six routes, which made "docs" the dominant first
// path segment — so a site correctly built for "/" was reported as built for
// "/docs/" and the build was refused, with an error quoting /img/favicon.ico as
// evidence for a prefix it is not under.
//
// Skipped when www/build is absent, which it is on a clean checkout: this asserts
// against a real artifact rather than a fixture, and a fixture is exactly what
// failed to catch this.
func TestVerifyBaseURLOnTheRealLandingPage(t *testing.T) {
	dir := filepath.Join("..", "..", "www", "build")
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		t.Skip("www/build is not present; run `npm run build` in www/ to exercise this")
	}

	if err := verifyBaseURL(dir, "/"); err != nil {
		t.Errorf("the real landing page was rejected at its own base URL: %v", err)
	}
}
