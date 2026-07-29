package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

// cacheManagers are the per-manager subdirectories under a preview's cache.
//
// Named here rather than discovered by listing the directory, because clearing is
// destructive and cache_dir may be somewhere the operator chose — a typo in the
// config should not turn "clear the cache" into "delete that directory". Only paths
// this program creates are ones it will remove.
//
// Kept in step with cacheMounts in internal/pipeline/dockermount.go by hand. The
// consequence of drift is a cache the button does not clear, which is a stale
// download rather than a lost file.
var cacheManagers = []string{"npm", "yarn", "pnpm"}

// previewIDPattern is what model.PullRequest.PreviewID produces: twelve hex
// characters from a sha256.
//
// Checked rather than trusted, because this value comes from a URL and is about to
// be joined onto a filesystem path. A digest needs no escaping, which is most of the
// reason the cache is keyed on one — but that is only true of a value that really is
// a digest.
var previewIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// clearCache empties one preview's package manager caches.
//
// The escape hatch for the one failure a cache introduces: a corrupt or
// half-written entry that makes every build of a pull request fail the same way,
// with an error about a tarball rather than about the cache. Teardown removes the
// cache with the rest of the preview, so this is for the pull request that is still
// open and whose next build has to work.
//
// Gated exactly as a project write is: clearing forces a refetch, so a remote
// caller could otherwise hold the build host on the registry indefinitely.
func (a *ProjectsAdmin) clearCache(w http.ResponseWriter, r *http.Request) {
	if a.cfg.CacheRoot() == "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "no build.cache_dir is configured, so there is no cache to clear",
		})
		return
	}

	preview := r.PathValue("preview")
	if !previewIDPattern.MatchString(preview) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "not a preview id: expected twelve hex characters, got " + preview,
		})
		return
	}

	dir := a.cfg.PreviewCacheDir(preview)
	cleared, err := clearManagerDirs(dir)
	if err != nil {
		a.log.Warn("clearing a build cache", "dir", dir, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	a.log.Info("cleared a preview's build cache", "preview", preview, "managers", cleared)
	writeJSON(w, http.StatusOK, map[string]any{"preview": preview, "cleared": cleared})
}

// clearManagerDirs removes the manager subdirectories under one cache and recreates
// them.
//
// Recreated rather than left absent so the next build's bind mount finds a directory
// that exists on the host. Docker creates a missing bind source itself, as root —
// which on a Linux host leaves a cache the operator cannot clear, and makes this
// button stop working the first time it is used.
func clearManagerDirs(dir string) ([]string, error) {
	var cleared []string
	for _, m := range cacheManagers {
		sub := filepath.Join(dir, m)
		if _, err := os.Stat(sub); err != nil {
			continue
		}
		// RemoveAll rather than walking the contents: a package cache holds tens of
		// thousands of small files and this is the only version of it that finishes
		// quickly on Windows.
		if err := os.RemoveAll(sub); err != nil {
			return cleared, fmt.Errorf("could not clear %s: %w — a build may be running and "+
				"holding a file open in it; try again once the queue is idle", sub, err)
		}
		if err := os.MkdirAll(sub, 0o755); err != nil {
			return cleared, fmt.Errorf("cleared %s but could not recreate it: %w", sub, err)
		}
		cleared = append(cleared, m)
	}
	return cleared, nil
}
