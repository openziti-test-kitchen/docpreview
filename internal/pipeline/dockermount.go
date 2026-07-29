package pipeline

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/netfoundry/docpreview/internal/model"
)

// hostMountPath spells a host directory the way the docker daemon can see it.
//
// On Linux and macOS that is the path itself. On Windows it is not: the daemon is
// Linux, so it has no concept of a drive letter, and every spelling of one is
// rejected — --volume splits on the colon and reports "invalid mode: /workspace",
// --mount reports the path is not absolute. The daemon does see the drive, at
// /mnt/<letter>/…, which is where a dockerd inside WSL2 finds the Windows disks.
//
// Docker Desktop's own engine uses /run/desktop/mnt/host/<letter> instead, and a
// daemon on another machine cannot see the host's disk at all. Neither is handled:
// this returns an error rather than mounting something wrong, because an empty
// mount fails later as a missing package.json and sends whoever is debugging it
// looking at the repository instead of at the mount.
func hostMountPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving the workspace path: %w", err)
	}
	p := filepath.ToSlash(abs)

	if runtime.GOOS != "windows" {
		return p, nil
	}
	if len(p) > 2 && p[1] == ':' && p[2] == '/' {
		return "/mnt/" + strings.ToLower(p[:1]) + p[2:], nil
	}
	return "", fmt.Errorf("cannot express %s as a path the docker daemon can mount; "+
		"the docker driver needs the workspace on a lettered drive", abs)
}

// cacheMounts are the package manager caches for one repository, and the
// environment that points each manager at its own.
//
// Without these every build downloads the whole dependency tree again: a workspace
// is created per commit and pruned with its siblings, so nothing inside it
// survives a push. Measured on this project's own www/: two minutes for `npm ci`
// cold, against a few seconds warm.
//
// # One cache per repository
//
// Keyed by platform/owner/repo, not shared. A shared cache would be faster on a
// repository's first build and is defensible on integrity grounds — entries are
// content-addressed and checked against the lockfile's hashes — but it makes every
// repository's builds depend on a directory every other repository writes to. One
// corrupt entry then breaks every project at once, and the blast radius of the
// clear-cache button is everything rather than the thing that is broken.
//
// The cost is bounded: the miss is per repository and happens once, and the same
// tarball existing twice on disk is cheap next to a build that has to refetch it.
//
// What is deliberately *not* cached is node_modules. `npm ci` deletes it before
// installing, which a bind mount cannot survive, and an installed tree is the thing
// that would be unsafe to reuse anyway.
//
// Directories are created on the host first. A bind mount of a path the daemon
// cannot find creates it as root-owned, which on a Linux host leaves a cache
// directory the operator cannot clear.
func (b *Builder) cacheMounts(pr model.PullRequest) ([]string, error) {
	root := b.defaults.CacheDir
	if root == "" {
		return nil, nil
	}
	root = filepath.Join(root, CacheKey(pr.Repo.Platform, pr.Repo.Owner, pr.Repo.Name))

	// One directory per manager. They have no common format and pnpm in
	// particular hard-links out of its store, which requires the store to be on
	// its own path rather than inside another manager's tree.
	managers := []struct{ dir, env, target string }{
		{"npm", "npm_config_cache", "/cache/npm"},
		{"yarn", "YARN_CACHE_FOLDER", "/cache/yarn"},
		{"pnpm", "npm_config_store_dir", "/cache/pnpm"},
	}

	var args []string
	for _, m := range managers {
		host := filepath.Join(root, m.dir)
		if err := os.MkdirAll(host, 0o755); err != nil {
			return nil, fmt.Errorf("creating the %s cache directory: %w", m.dir, err)
		}
		source, err := hostMountPath(host)
		if err != nil {
			return nil, err
		}
		args = append(args,
			"--mount", "type=bind,source="+source+",target="+m.target,
			"--env", m.env+"="+m.target,
		)
	}
	return args, nil
}

// CacheKey is a repository's directory name under the cache root.
//
// One flat component rather than nested directories, so the path stays short —
// node_modules paths are already the deepest thing this program creates and Windows
// has a limit.
//
// Every character outside a small set becomes an underscore. The owner and
// repository names arrive from a webhook, and the failure being prevented is not
// cosmetic: an owner of ".." would place a cache directory outside the cache root,
// and a clear would then delete something else. Exported because the daemon builds
// the same path to clear it, and two spellings of one path is how they drift.
func CacheKey(platform model.Platform, owner, repo string) string {
	return sanitizeCacheComponent(string(platform)) + "-" +
		sanitizeCacheComponent(owner) + "-" +
		sanitizeCacheComponent(repo)
}

func sanitizeCacheComponent(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// rejectSymlinks fails a build whose output directory contains a symlink.
//
// This is the one guarantee a bind mount gives away. The preview server hands the
// output directory to http.Dir, which refuses paths that escape the root but
// happily follows a symlink that leaves it — so a build under the docker driver,
// which by definition ran code from a repository nobody vetted, could publish
// /etc/passwd by creating build/leak -> /etc/passwd on its way out. The container
// cannot read the host, but the server that serves its output can.
//
// Failing rather than quietly unlinking: a static site has no use for a symlink, so
// one appearing is either an attempt or a bug, and both are worth a message. Only
// the docker driver calls this; the local driver already runs with this process's
// privileges, where a symlink is the least of it.
func rejectSymlinks(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				rel = path
			}
			return fmt.Errorf("the build output contains a symlink (%s), which cannot be published", rel)
		}
		return nil
	})
}

// runContainer starts a container, streams its output, and reports its exit code.
//
// Three commands rather than `docker start -a`, because attach delivers nothing
// over a TCP endpoint. Measured, not assumed: against this daemon `start -a`
// returned zero bytes for a container that had written to both stdout and stderr,
// while `docker logs` returned all of it. A build whose output never arrives is
// worse than no docker driver — the dashboard tails an empty pane and a failure
// comment has nothing to point at. See TestDockerStdoutReachesTheLog.
//
// `logs -f` follows until the container stops, so output still arrives as it
// happens rather than in one block at the end.
//
// The exit code comes from `docker wait`, because `logs` succeeds whether the
// build passed or failed — it is reporting output, not outcome.
func (b *Builder) runContainer(ctx context.Context, container string, out io.Writer) error {
	if err := exec.CommandContext(ctx, "docker", "start", container).Run(); err != nil {
		return fmt.Errorf("starting the build container: %w", err)
	}

	// Not fatal on its own. A dropped log stream loses the output but the build is
	// still running, and its exit code is still the answer that matters.
	follow := exec.CommandContext(ctx, "docker", "logs", "-f", container)
	follow.Stdout = out
	follow.Stderr = out
	if err := follow.Run(); err != nil && ctx.Err() == nil {
		b.log.Warn("following the build container's output", "container", container, "error", err)
	}

	code, err := exec.CommandContext(ctx, "docker", "wait", container).Output()
	if err != nil {
		return fmt.Errorf("waiting for the build container: %w", err)
	}
	status := strings.TrimSpace(string(code))
	if status != "0" {
		return fmt.Errorf("docker build failed: the build exited %s", status)
	}
	return nil
}
