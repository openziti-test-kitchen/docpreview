package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/pipeline"
)

// cacheManagers are the per-manager subdirectories under a repository's cache.
//
// Named here rather than discovered by listing the directory, because clearing is
// destructive and cache_dir may be somewhere the operator chose — a typo in the
// config should not turn "clear the caches" into "delete that directory". Only
// paths this program creates are ones it will remove.
//
// Kept in step with cacheMounts in internal/pipeline/dockermount.go by hand. The
// consequence of drift is a cache the button does not clear, which is a stale
// download rather than a lost file.
var cacheManagers = []string{"npm", "yarn", "pnpm"}

// clearCache empties one repository's package manager caches.
//
// The escape hatch for the one failure a cache introduces: a corrupt or
// half-written entry that makes every build of a project fail the same way, with an
// error about a tarball rather than about the cache. Without a button the fix is a
// path an operator has to be told about, at the moment they are least able to go
// looking.
//
// Per repository, because the caches are — see pipeline.CacheKey. Clearing one
// project's cache costs that project its next download and leaves everything else
// warm, which is the whole reason the caches are not shared.
//
// Gated exactly as a project write is: clearing forces a refetch, so a remote
// caller could otherwise hold the build host on the registry indefinitely.
func (a *ProjectsAdmin) clearCache(w http.ResponseWriter, r *http.Request) {
	root := a.cfg.Build.CacheDir
	if root == "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "no build.cache_dir is configured, so there is no cache to clear",
		})
		return
	}

	key := pipeline.CacheKey(
		model.Platform(r.PathValue("platform")),
		r.PathValue("owner"),
		r.PathValue("repo"),
	)
	dir := filepath.Join(root, key)

	cleared, err := clearManagerDirs(dir)
	if err != nil {
		a.log.Warn("clearing a build cache", "dir", dir, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	a.log.Info("cleared a project's build cache", "project", key, "managers", cleared)
	writeJSON(w, http.StatusOK, map[string]any{"project": key, "cleared": cleared})
}

// clearManagerDirs removes the manager subdirectories under one repository's cache
// and recreates them.
//
// Recreated rather than left absent so the next build's bind mount finds a
// directory that exists on the host. Docker creates a missing bind source itself,
// as root — which on a Linux host leaves a cache the operator cannot clear, and
// makes this button stop working the first time it is used.
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
