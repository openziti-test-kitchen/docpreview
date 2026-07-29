package daemon

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/pipeline"
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

// seedCache fills one repository's cache the way a build would.
func seedCache(t *testing.T, root, owner, repo string) string {
	t.Helper()
	dir := filepath.Join(root, pipeline.CacheKey(model.PlatformGitHub, owner, repo))
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

func clearCacheRequest(t *testing.T, h http.Handler, owner, repo, remote string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/projects/github/"+owner+"/"+repo+"/cache", nil)
	r.RemoteAddr = remote
	h.ServeHTTP(rec, r)
	return rec
}

// TestClearCacheEmptiesOnlyThatProject is the property the per-repository layout
// exists for. Clearing one project must leave every other project warm, or the
// button is a build-everything-again button.
func TestClearCacheEmptiesOnlyThatProject(t *testing.T) {
	cache := t.TempDir()
	mine := seedCache(t, cache, "acme", "docs")
	theirs := seedCache(t, cache, "other", "docs")

	h := testProjectsAdmin(t, cache)
	rec := clearCacheRequest(t, h, "acme", "docs", "127.0.0.1:54321")
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
			t.Errorf("another project's %s cache was cleared too: %v", m, err)
		}
	}
}

// TestClearCacheRefusedRemotely — clearing forces a refetch, so a remote caller
// could otherwise hold the build host on the registry.
func TestClearCacheRefusedRemotely(t *testing.T) {
	cache := t.TempDir()
	seedCache(t, cache, "acme", "docs")
	h := testProjectsAdmin(t, cache)

	for _, remote := range []string{"192.0.2.1:1234", "10.0.0.5:443"} {
		if rec := clearCacheRequest(t, h, "acme", "docs", remote); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", remote, rec.Code)
		}
	}

	// And the tunnel case: loopback, but forwarded.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/projects/github/acme/docs/cache", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("forwarded: status = %d, want 403", rec.Code)
	}
}

// TestClearCacheCannotEscapeTheCacheRoot — the owner and repository come from the
// URL, so ".." in one must not reach a directory outside the cache.
//
// Two spellings, because only one of them reaches the handler. ServeMux cleans the
// path before routing, so a literal ".." segment is answered with a redirect and
// never arrives; a percent-encoded one is decoded into the path value afterwards
// and does. The second is the case CacheKey has to survive, and testing only the
// first would have proved nothing about it.
func TestClearCacheCannotEscapeTheCacheRoot(t *testing.T) {
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

	for _, path := range []string{
		"/api/projects/github/../not-the-cache/cache",
		"/api/projects/github/%2e%2e/not-the-cache/cache",
		"/api/projects/github/%2e%2e%2f%2e%2e/not-the-cache/cache",
	} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("DELETE", path, nil)
		r.RemoteAddr = "127.0.0.1:54321"
		h.ServeHTTP(rec, r)
		t.Logf("%s → %d", path, rec.Code)

		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("%s cleared a directory outside the cache root: %v", path, err)
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
	seedCache(t, cache, "acme", "docs")

	h := testProjectsAdmin(t, cache)
	if rec := clearCacheRequest(t, h, "acme", "docs", "127.0.0.1:54321"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("a file the cache does not own was deleted: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("the cache root itself was deleted: %v", err)
	}
}
