package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// cacheManagers are the per-manager subdirectories under build.cache_dir.
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

// clearCache empties the package manager caches.
//
// The escape hatch for the one failure a shared cache introduces: a corrupt or
// half-written entry that makes every build of every project fail the same way,
// with an error about a tarball rather than about the cache. Without a button the
// fix is a path an operator has to be told about, at the moment they are least
// able to go looking.
//
// Gated exactly as a project write is — clearing forces every subsequent build to
// re-download, so a remote caller could hold the build host on the registry
// indefinitely.
func (a *ProjectsAdmin) clearCache(w http.ResponseWriter, r *http.Request) {
	root := a.cfg.Build.CacheDir
	if root == "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "no build.cache_dir is configured, so there is no cache to clear",
		})
		return
	}

	var cleared []string
	for _, m := range cacheManagers {
		dir := filepath.Join(root, m)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		// Remove and recreate rather than walking the contents: a package cache
		// holds tens of thousands of small files and RemoveAll is the only version
		// of this that finishes quickly on Windows.
		if err := os.RemoveAll(dir); err != nil {
			a.log.Warn("clearing a build cache", "dir", dir, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": fmt.Sprintf("could not clear %s: %v — a build may be running and holding "+
					"a file open in it; try again once the queue is idle", dir, err),
			})
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			a.log.Warn("recreating a build cache", "dir", dir, "error", err)
		}
		cleared = append(cleared, m)
	}

	a.log.Info("cleared the build caches", "managers", cleared)
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared})
}
