package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
)

// ProbeDocker reports whether a docker daemon is reachable, and what to say when it
// is not.
//
// It exists because the driver decision is a security decision. The local driver runs
// the repository's own build on this host — `npm run build` executes whatever the
// branch's package.json says, and `npm install` runs every dependency's postinstall
// script — so the container is not an optimisation, it is the only thing standing
// between a pull request and the machine holding the GitHub App private key. Choosing
// that default silently, and then falling back to the unsandboxed driver just as
// silently, is how an operator ends up believing they have isolation they do not have.
//
// `docker version` rather than `docker info`: it is the cheapest call that reaches the
// *daemon* rather than only the client. A CLI present with no daemon behind it is the
// common Windows case — Docker Desktop not started — and `docker --version` answers
// happily in exactly that state, which is the wrong answer here.
//
// The detail is quoted from docker's own stderr, trimmed, because the useful part is
// always docker's message rather than anything this could invent about it.
func ProbeDocker(ctx context.Context) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.Is(err, exec.ErrNotFound):
			return false, "the docker command is not on PATH"
		case ctx.Err() != nil:
			return false, "docker did not answer within 10s"
		case errors.As(err, &exitErr):
			detail := strings.TrimSpace(string(exitErr.Stderr))
			// Multi-line on Windows, and the first line is the one that says what
			// is wrong rather than what to try.
			if i := strings.IndexAny(detail, "\r\n"); i > 0 {
				detail = detail[:i]
			}
			if detail == "" {
				detail = "docker version exited " + fmt.Sprint(exitErr.ExitCode())
			}
			return false, detail
		default:
			return false, err.Error()
		}
	}

	version := strings.TrimSpace(string(out))
	if version == "" {
		// A zero exit with no server version means the client answered for itself.
		return false, "docker reported no server version; is the daemon running?"
	}
	return true, version
}

// ImageStatus is what InspectImage found.
type ImageStatus struct {
	// Found is whether a build using this image would be able to start.
	Found bool

	// Local is true when the image is already pulled, so the first build does not
	// wait for a download. Worth reporting separately: "found, but it is a 400 MB
	// pull" and "found, already here" are different answers to "will this work".
	Local bool

	// Detail is docker's own message when Found is false, or the resolved digest or
	// image id when it is true.
	Detail string
}

// imageRef is what a reference may contain: a registry host with an optional port, a
// path, and a tag or digest.
//
// Validated before being handed to docker, for two reasons. A reference beginning with
// a dash would be parsed by docker as a flag rather than as an image, and this is
// reachable from an HTTP handler — argv means there is no shell to inject into, but
// there is still an argument parser. And a rejection here is a better error than
// whatever docker says about a string nobody meant to type.
var imageRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,255}$`)

// InspectImage reports whether a container image can be resolved.
//
// It exists so that a typo in the image field is caught while somebody is looking at the
// form, rather than surfacing as a failed build minutes later with docker's own
// "manifest unknown" in the middle of a log. That is the entire justification: the
// operator's attention is on this decision now and will not be later.
//
// Local first, then the registry. `docker image inspect` is instant and offline and
// answers the common case — the default image, already pulled. `docker manifest
// inspect` is a network round trip to the registry and is what catches a tag that does
// not exist anywhere, but it also fails for a private registry the daemon has no
// credentials for, which is why a local hit short-circuits it rather than the other way
// round.
func InspectImage(ctx context.Context, ref string) (ImageStatus, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ImageStatus{}, errors.New("no image given")
	}
	if !imageRef.MatchString(ref) {
		return ImageStatus{}, fmt.Errorf("%q is not a container image reference", ref)
	}

	if out, err := dockerOut(ctx, 10*time.Second,
		"image", "inspect", "--format", "{{.Id}}", ref); err == nil {
		return ImageStatus{Found: true, Local: true, Detail: firstLine(out)}, nil
	}

	// Not here. Ask the registry, which is slower and can fail for reasons that have
	// nothing to do with the reference being wrong.
	out, err := dockerOut(ctx, 30*time.Second, "manifest", "inspect", ref)
	if err == nil {
		_ = out
		return ImageStatus{Found: true, Detail: "found in the registry, and not yet pulled"}, nil
	}
	return ImageStatus{Detail: err.Error()}, nil
}

// dockerOut runs docker and returns stdout, or an error carrying docker's own first
// line of stderr — which is the part worth showing an operator.
func dockerOut(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if detail := firstLine(string(exitErr.Stderr)); detail != "" {
				return "", errors.New(detail)
			}
		}
		if ctx.Err() != nil {
			return "", errors.New("docker did not answer in time")
		}
		return "", err
	}
	return string(out), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i > 0 {
		s = s[:i]
	}
	return s
}

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
// survives a push.
//
// **It is not the speed win, and this comment used to claim it was.** Measured on this
// project's own www/: 4m28s cold against 4m21s warm — inside the noise. The whole
// saving was node_modules, which moved off the bind mount onto a volume and took the
// install from 5m46s to 14s (see the mount comments in build.go). What the cache buys
// is network: the same tarballs are not fetched again on the second push to a branch,
// which matters on a metered or slow link and not at all to the clock here. Keep it for
// that reason, and do not credit it with the other one.
//
// # One cache per pull request
//
// Keyed by preview, which is the same lifetime as everything else a pull request
// owns — its workspace, its artifacts, its logs. The cache is therefore deleted by
// the same teardown that removes those, which is the property a shared cache cannot
// have: a directory outliving every branch that wrote to it has no moment at which
// anyone knows it is safe to remove.
//
// The cost is the first build of each pull request, which is cold. That is accepted
// deliberately. A cache shared more widely is warmer, and it makes every build
// depend on a directory every other build writes to — one corrupt entry then fails
// everything at once, and clearing it costs everything too. Within a pull request,
// which is where the pushes actually repeat, this is warm from the second build on.
//
// Not keyed on the branch name: PreviewID excludes it, so a force-push or a rename
// keeps the cache the pull request already filled.
//
// What is deliberately *not* cached is node_modules. `npm ci` deletes it before
// installing, which a bind mount cannot survive, and an installed tree is the thing
// that would be unsafe to reuse anyway.
//
// Directories are created on the host first. A bind mount of a path the daemon
// cannot find creates it as root-owned, which on a Linux host leaves a cache
// directory the operator cannot clear.
func (b *Builder) cacheMounts(pr model.PullRequest) ([]string, error) {
	// One volume per manager. They have no common format and pnpm in particular
	// hard-links out of its store, which requires the store to be on its own path
	// rather than inside another manager's tree.
	managers := []struct{ name, env, target string }{
		{"npm", "npm_config_cache", "/cache/npm"},
		{"yarn", "YARN_CACHE_FOLDER", "/cache/yarn"},
		{"pnpm", "npm_config_store_dir", "/cache/pnpm"},
	}

	var args []string
	for _, m := range managers {
		args = append(args,
			"--mount", "type=volume,source="+CacheVolume(pr.PreviewID(), m.name)+
				",target="+m.target,
			"--env", m.env+"="+m.target,
		)
	}
	return args, nil
}

// CacheVolume names the docker volume holding one package manager's cache for one
// preview.
//
// # Why a volume and not a directory on the host
//
// It was a bind mount, and on Windows that made the cache the slowest part of a build
// rather than the thing that speeds one up. Measured while watching a real yarn install
// fetch packages into it: **0.4 MB/s**, against the 100+ MB/s the same disk does natively.
// A package cache is tens of thousands of small files, and every one of them was crossing
// the WSL to NTFS boundary — the identical penalty that made `npm ci` take 5m46s writing
// node_modules through a mount and 14s writing it to a volume.
//
// A named volume lives in the docker VM's own filesystem, so the writes are native. It is
// still per preview, so it still has the lifetime of everything else a pull request owns,
// and it still survives between pushes to the same branch. What it gives up is being
// visible on the host — which is why teardown and the cache controls delete volumes rather
// than directories.
//
// `build.cache_dir` therefore applies to the local driver only, and says so in the config.
//
// The name is the preview id and the manager, both already safe for docker's
// `[a-zA-Z0-9][a-zA-Z0-9_.-]` rule: a preview id is hex and a manager name is a word.
func CacheVolume(previewID, manager string) string {
	return "docpreview-cache-" + previewID + "-" + manager
}

// CacheVolumesFor lists every cache volume belonging to a preview, for a caller that has
// to remove them.
func CacheVolumesFor(previewID string) []string {
	return []string{
		CacheVolume(previewID, "npm"),
		CacheVolume(previewID, "yarn"),
		CacheVolume(previewID, "pnpm"),
	}
}

// RemoveCacheVolumes deletes a preview's cache volumes.
//
// Best effort, and quiet about a volume that does not exist: a preview built under the
// local driver never had any, and a preview torn down twice would otherwise log a failure
// for work already done. `docker volume rm -f` is itself idempotent, so the only errors
// worth reporting are docker being unreachable, which the caller logs.
func RemoveCacheVolumes(ctx context.Context, previewID string) error {
	args := append([]string{"volume", "rm", "-f"}, CacheVolumesFor(previewID)...)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).Run()
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
