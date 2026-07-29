package daemon

import (
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

	return NewProjectsAdmin(st, cfg, slog.New(slog.DiscardHandler)).Handler()
}

func TestClearCacheEmptiesEachManagerDirectory(t *testing.T) {
	cache := t.TempDir()
	for _, m := range cacheManagers {
		if err := os.MkdirAll(filepath.Join(cache, m, "deep"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, m, "deep", "tarball"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	h := testProjectsAdmin(t, cache)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/projects/cache", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for _, m := range cacheManagers {
		if _, err := os.Stat(filepath.Join(cache, m, "deep")); !os.IsNotExist(err) {
			t.Errorf("%s cache contents survived: %v", m, err)
		}
		// Recreated, so the next build's mount is of a directory that exists on
		// the host rather than one docker creates as root.
		if _, err := os.Stat(filepath.Join(cache, m)); err != nil {
			t.Errorf("%s cache directory was not recreated: %v", m, err)
		}
	}
}

// TestClearCacheRefusedRemotely — clearing costs every project its next build's
// downloads, so a remote caller could hold the build host on the registry.
func TestClearCacheRefusedRemotely(t *testing.T) {
	cache := t.TempDir()
	h := testProjectsAdmin(t, cache)

	for _, remote := range []string{"192.0.2.1:1234", "10.0.0.5:443"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("DELETE", "/api/projects/cache", nil)
		r.RemoteAddr = remote
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", remote, rec.Code)
		}
	}

	// And the tunnel case: loopback but forwarded.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/projects/cache", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("forwarded: status = %d, want 403", rec.Code)
	}
}

// TestClearCacheDoesNotTouchTheCacheRoot — cache_dir may be a path the operator
// chose, and "clear the caches" must not become "delete that directory".
func TestClearCacheDoesNotTouchTheCacheRoot(t *testing.T) {
	cache := t.TempDir()
	bystander := filepath.Join(cache, "something-else.txt")
	if err := os.WriteFile(bystander, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := testProjectsAdmin(t, cache)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/projects/cache", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("a file the cache does not own was deleted: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("the cache root itself was deleted: %v", err)
	}
}
