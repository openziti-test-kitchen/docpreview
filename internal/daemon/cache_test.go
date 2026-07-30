package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/store"
)

func testProjectsAdmin(t *testing.T, cacheDir string) http.Handler {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.Build.CacheDir = cacheDir
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}

	// No real docker. The cache controls delete volumes, and the first version of that
	// ran against the machine's own daemon — a `go test` run deleted the cache volumes of
	// every live preview on the host. Stubbed to nothing: what these tests are about is
	// the directory half.
	return NewProjectsAdmin(st, cfg, slog.New(slog.DiscardHandler)).
		WithVolumeOps(
			func(context.Context) ([]string, error) { return nil, nil },
			func(context.Context, string) error { return nil },
		).Handler()
}

// seedCache fills one preview's cache the way a build would.
func seedCache(t *testing.T, root, previewID string) string {
	t.Helper()
	dir := filepath.Join(root, previewID)
	for _, m := range cacheManagers {
		if err := os.MkdirAll(filepath.Join(dir, m, "deep"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, m, "deep", "tarball"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func clearCacheRequest(t *testing.T, h http.Handler, previewID, remote string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/cache/"+previewID, nil)
	r.RemoteAddr = remote
	h.ServeHTTP(rec, r)
	return rec
}

// TestClearCacheEmptiesOnlyThatPreview is the property the per-preview layout exists
// for. Clearing one pull request's cache must leave every other one warm, or the
// button is a rebuild-everything button.
func TestClearCacheEmptiesOnlyThatPreview(t *testing.T) {
	cache := t.TempDir()
	mine := seedCache(t, cache, "19344c5ee369")
	theirs := seedCache(t, cache, "7ac8b8042f54")

	h := testProjectsAdmin(t, cache)
	rec := clearCacheRequest(t, h, "19344c5ee369", "127.0.0.1:54321")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	for _, m := range cacheManagers {
		if _, err := os.Stat(filepath.Join(mine, m, "deep")); !os.IsNotExist(err) {
			t.Errorf("%s cache contents survived the clear: %v", m, err)
		}
		// Recreated, so the next build's mount finds a directory that exists on the
		// host rather than one docker creates as root.
		if _, err := os.Stat(filepath.Join(mine, m)); err != nil {
			t.Errorf("%s cache directory was not recreated: %v", m, err)
		}
		if _, err := os.Stat(filepath.Join(theirs, m, "deep", "tarball")); err != nil {
			t.Errorf("another preview's %s cache was cleared too: %v", m, err)
		}
	}
}

// TestClearCacheRefusedRemotely — clearing forces a refetch, so a remote caller
// could otherwise hold the build host on the registry.
func TestClearCacheRefusedRemotely(t *testing.T) {
	cache := t.TempDir()
	seedCache(t, cache, "19344c5ee369")
	h := testProjectsAdmin(t, cache)

	for _, remote := range []string{"192.0.2.1:1234", "10.0.0.5:443"} {
		if rec := clearCacheRequest(t, h, "19344c5ee369", remote); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", remote, rec.Code)
		}
	}

	// And the tunnel case: loopback, but forwarded.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/cache/19344c5ee369", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("forwarded: status = %d, want 403", rec.Code)
	}
}

// TestClearCacheRejectsAnythingButAPreviewID — the value arrives in a URL and is
// joined onto a filesystem path. A digest needs no escaping, but only a value that
// really is a digest.
//
// Both spellings of an escape, because only one reaches the handler: ServeMux cleans
// a literal ".." out of the path before routing, and a percent-encoded one is
// decoded into the path value afterwards.
func TestClearCacheRejectsAnythingButAPreviewID(t *testing.T) {
	cache := t.TempDir()
	outside := filepath.Join(cache, "..", "not-the-cache")
	if err := os.MkdirAll(filepath.Join(outside, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "npm", "keep.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := testProjectsAdmin(t, cache)

	for _, id := range []string{
		"..",
		"%2e%2e",
		"%2e%2e%2fnot-the-cache",
		"19344c5ee369x",  // too long
		"19344c5ee36",    // too short
		"19344C5EE369",   // uppercase is not what PreviewID produces
		"../not-the-cache",
	} {
		rec := clearCacheRequest(t, h, id, "127.0.0.1:54321")
		if rec.Code == http.StatusOK {
			t.Errorf("%q was accepted as a preview id", id)
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("%q reached outside the cache root: %v", id, err)
		}
	}
}

// TestClearCacheDoesNotTouchTheCacheRoot — cache_dir may be a path the operator
// chose, and "clear the cache" must not become "delete that directory".
func TestClearCacheDoesNotTouchTheCacheRoot(t *testing.T) {
	cache := t.TempDir()
	bystander := filepath.Join(cache, "something-else.txt")
	if err := os.WriteFile(bystander, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedCache(t, cache, "19344c5ee369")

	h := testProjectsAdmin(t, cache)
	if rec := clearCacheRequest(t, h, "19344c5ee369", "127.0.0.1:54321"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("a file the cache does not own was deleted: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("the cache root itself was deleted: %v", err)
	}
}
