package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func clearAllRequest(t *testing.T, h http.Handler, remote string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/cache", nil)
	r.RemoteAddr = remote
	h.ServeHTTP(rec, r)
	return rec
}

// TestClearAllCachesEmptiesEveryPreview is what the dashboard's one cache control
// calls. Per preview is the right shape for the code, and the wrong shape for a
// button: an operator reaching for this has a build failing on a package download
// and does not yet know which preview holds the bad entry.
func TestClearAllCachesEmptiesEveryPreview(t *testing.T) {
	cache := t.TempDir()
	first := seedCache(t, cache, "19344c5ee369")
	second := seedCache(t, cache, "7ac8b8042f54")

	h := testProjectsAdmin(t, cache)
	rec := clearAllRequest(t, h, "127.0.0.1:54321")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got struct{ Cleared, Failed []string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cleared) != 2 {
		t.Errorf("cleared = %v, want both previews", got.Cleared)
	}
	if len(got.Failed) != 0 {
		t.Errorf("failed = %v, want none", got.Failed)
	}

	for _, dir := range []string{first, second} {
		for _, m := range cacheManagers {
			if _, err := os.Stat(filepath.Join(dir, m, "deep")); !os.IsNotExist(err) {
				t.Errorf("%s/%s survived: %v", filepath.Base(dir), m, err)
			}
			if _, err := os.Stat(filepath.Join(dir, m)); err != nil {
				t.Errorf("%s/%s was not recreated: %v", filepath.Base(dir), m, err)
			}
		}
	}
}

// TestClearAllCachesIgnoresDirectoriesItDoesNotOwn — cache_dir may be a path the
// operator chose. Anything under it that is not a preview cache is not this button's
// business, and a clear-everything button that means it literally is how somebody
// loses a directory.
func TestClearAllCachesIgnoresDirectoriesItDoesNotOwn(t *testing.T) {
	cache := t.TempDir()
	seedCache(t, cache, "19344c5ee369")

	// A directory that is not a preview id, holding something that looks exactly
	// like a cache — so only the name distinguishes it.
	stranger := filepath.Join(cache, "someone-elses-stuff")
	if err := os.MkdirAll(filepath.Join(stranger, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(stranger, "npm", "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(cache, "notes.txt")
	if err := os.WriteFile(loose, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := testProjectsAdmin(t, cache)
	if rec := clearAllRequest(t, h, "127.0.0.1:54321"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a directory that is not a preview cache was cleared: %v", err)
	}
	if _, err := os.Stat(loose); err != nil {
		t.Errorf("a loose file under the cache root was deleted: %v", err)
	}
}

// TestClearAllCachesRefusedRemotely — it forces a refetch for every open pull
// request, which is a way to hold the build host on the registry.
func TestClearAllCachesRefusedRemotely(t *testing.T) {
	cache := t.TempDir()
	seedCache(t, cache, "19344c5ee369")
	h := testProjectsAdmin(t, cache)

	if rec := clearAllRequest(t, h, "192.0.2.1:1234"); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/cache", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("forwarded: status = %d, want 403", rec.Code)
	}
}

// TestClearAllCachesWithNothingBuiltYet — the cache root does not exist until the
// first docker build, and asking to clear it then is not an error.
func TestClearAllCachesWithNothingBuiltYet(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "never-created")
	h := testProjectsAdmin(t, cache)

	rec := clearAllRequest(t, h, "127.0.0.1:54321")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
