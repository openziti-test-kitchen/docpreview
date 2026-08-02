package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyBaseURLOnTheRealLandingPage runs the check against this repository's
// own built site.
//
// The landing page is the shape that can break the check: it links into /docs/
// thirteen times across six routes, which could make "docs" the dominant first
// path segment and report a site correctly built for "/" as built for "/docs/" —
// with an error quoting /img/favicon.ico as evidence for a prefix it is not under.
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
