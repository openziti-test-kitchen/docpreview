package pipeline

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
