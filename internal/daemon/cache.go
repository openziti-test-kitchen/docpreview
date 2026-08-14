package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/pipeline"
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

// clearCache empties the package manager caches a preview's builds use.
//
// The escape hatch for the one failure a cache introduces: a corrupt or
// half-written entry that makes every build fail the same way, with an error about a
// tarball rather than about the cache.
//
// **The cache belongs to the repository, not to this preview.** Addressed by preview because that
// is what an operator has in front of them — a pull request whose build is failing — but clearing
// it costs every pull request against that repository one cold install. The response names the
// scope that was cleared so the page can say so.
//
// Gated exactly as a project write is: clearing forces a refetch, so a remote
// caller could otherwise hold the build host on the registry indefinitely.
func (a *ProjectsAdmin) clearCache(w http.ResponseWriter, r *http.Request) {
	preview := r.PathValue("preview")
	if !previewIDPattern.MatchString(preview) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "not a preview id: expected twelve hex characters, got " + preview,
		})
		return
	}

	// The repository behind the preview, because that is what the volumes are named for.
	//
	// A preview nothing recorded yields no scope, and no volume is removed for it. Not an error:
	// the local driver's cache is a directory named by the preview and is cleared below regardless,
	// and refusing the whole request would make an unknown preview unclearable under either driver.
	// The response carries the scope, empty when there was none, so the page can tell the two
	// outcomes apart.
	scope, scopeErr := a.cacheScopeOf(r.Context(), preview)
	if scopeErr != nil {
		a.log.Warn("no repository for this preview, so no cache volumes were removed",
			"preview", preview, "error", scopeErr)
	}

	// The docker driver's caches are volumes. Removed rather than emptied: a volume is
	// recreated by the next `docker create` that names it, so deleting it *is* clearing
	// it, and there is no way to walk its contents from here anyway.
	if a.removeVolumes != nil && scope != "" {
		if err := a.removeVolumes(r.Context(), scope); err != nil {
			a.log.Warn("removing cache volumes", "preview", preview, "scope", scope, "error", err)
		}
	}

	// And the local driver's, which is still a directory. No cache_dir configured is not
	// an error any more: the docker path above needs none.
	var cleared []string
	if dir := a.cfg.PreviewCacheDir(preview); dir != "" {
		dirErr := error(nil)
		cleared, dirErr = clearManagerDirs(dir)
		if dirErr != nil {
			a.log.Warn("clearing a build cache", "dir", dir, "error", dirErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": dirErr.Error()})
			return
		}
	}

	a.log.Info("cleared the build cache for a repository",
		"preview", preview, "scope", scope, "managers", cleared)
	writeJSON(w, http.StatusOK, map[string]any{
		"preview": preview, "scope": scope, "cleared": cleared,
	})
}

// cacheScopeOf resolves a preview to the cache scope its builds mount.
//
// Through the store rather than through the live set: the control exists for a preview whose build
// is failing, and a preview with no live publication is exactly the case where somebody reaches
// for it.
func (a *ProjectsAdmin) cacheScopeOf(ctx context.Context, previewID string) (string, error) {
	previews, err := a.store.ListPreviews(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range previews {
		if p.PreviewID == previewID {
			return pipeline.CacheScope(p.PR), nil
		}
	}
	return "", fmt.Errorf("no preview %s", previewID)
}

// clearAllCaches empties every preview's package manager caches.
//
// What the dashboard's one cache control calls. Per preview is the right shape for
// the *code* — the cache's lifetime is the preview's — but it is the wrong shape for
// a button, because an operator reaching for this has a build failing on a package
// download and does not yet know which preview owns the bad entry.
//
// Only directories that look like a preview ID are touched, and inside each only the
// three this program creates. cache_dir may be a path the operator chose, and
// anything else living under it is not this button's business.
func (a *ProjectsAdmin) clearAllCaches(w http.ResponseWriter, r *http.Request) {
	var cleared []string
	var failed []string

	// The docker driver's caches, which are volumes named `docpreview-cache-<preview>-…`.
	// Enumerated from docker rather than from the database, because the caches that most
	// need clearing belong to previews that have been torn down — the row is gone and the
	// volume is not.
	var vols []string
	if a.listVolumes != nil {
		var err error
		vols, err = a.listVolumes(r.Context())
		if err != nil {
			a.log.Warn("listing cache volumes", "error", err)
		}
	}
	// Grouped back into previews, so the three volumes of one pull request are removed
	// together and the result reads in the same units as the per-preview control.
	for _, id := range previewIDsFromVolumes(vols) {
		if err := a.removeVolumes(r.Context(), id); err != nil {
			// One volume held open by a running build must not stop the rest: the
			// operator asked for this because something is broken, and a partial clear is
			// more use than none.
			a.log.Warn("removing a preview's cache volumes", "preview", id, "error", err)
			failed = append(failed, id)
			continue
		}
		cleared = append(cleared, id)
	}

	// And the local driver's, if one is configured. Absent is no longer an error: with
	// the docker driver there is no directory to configure.
	if root := a.cfg.CacheRoot(); root != "" {
		entries, err := os.ReadDir(root)
		if err != nil && !os.IsNotExist(err) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": fmt.Sprintf("could not read %s: %v", root, err),
			})
			return
		}
		for _, e := range entries {
			if !e.IsDir() || !previewIDPattern.MatchString(e.Name()) {
				continue
			}
			if _, err := clearManagerDirs(filepath.Join(root, e.Name())); err != nil {
				a.log.Warn("clearing a build cache", "preview", e.Name(), "error", err)
				failed = append(failed, e.Name())
				continue
			}
			cleared = append(cleared, e.Name())
		}
	}

	if cleared == nil {
		cleared = []string{}
	}
	a.log.Info("cleared the build caches", "cleared", len(cleared), "failed", len(failed))
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared, "failed": failed})
}

// listCacheVolumes asks docker for the volumes this program created.
//
// Filtered by name prefix on docker's side, so a host with hundreds of unrelated volumes
// does not have them all read into memory — and so nothing this did not create can be
// returned to a function whose only purpose is to delete things.
func listCacheVolumes(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "volume", "ls",
		"--filter", "name=docpreview-cache-", "--format", "{{.Name}}").Output()
	if err != nil {
		return nil, err
	}

	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		// Re-checked here: `--filter name=` is a substring match on docker's side, and a
		// volume that merely contains the prefix is not one of ours.
		if strings.HasPrefix(name, "docpreview-cache-") {
			names = append(names, name)
		}
	}
	return names, nil
}

// previewIDsFromVolumes reduces `docpreview-cache-<preview>-<manager>` names to the
// distinct preview ids in them, in a stable order.
//
// Parsed by pattern rather than by splitting on the last dash: a manager name is a word
// and a preview id is twelve hex characters, so the id is what sits between the prefix and
// the final segment — and a volume that does not match that shape is not one of ours and
// is dropped rather than guessed at, because the next thing that happens to it is
// deletion.
func previewIDsFromVolumes(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		rest, ok := strings.CutPrefix(n, "docpreview-cache-")
		if !ok {
			continue
		}
		id, _, ok := strings.Cut(rest, "-")
		if !ok || !previewIDPattern.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
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
