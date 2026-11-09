package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/netfoundry/docpreview/internal/config"
)

// skipExitCode is the exit status a detect script uses to say "not a
// documentation change".
//
// 78 is EX_CONFIG from sysexits.h. Any nonzero code could have meant this, but
// then a script with a typo in it — which exits 127 or 2 — would be
// indistinguishable from a deliberate skip, and every broken script would
// silently disable previews for that repository. Reserving one specific code
// means a script that crashes reports a build failure, which is what the author
// needs to see.
const skipExitCode = 78

// detectTimeout bounds a repository-supplied detect script.
const detectTimeout = 60 * time.Second

// Decision is the outcome of documentation-change detection.
type Decision struct {
	// Build reports whether a preview should be built.
	Build bool

	// Reason is a one-line human explanation, shown in the pull request
	// comment when a change is skipped.
	Reason string
}

// Detector decides whether a change is documentation-related.
type Detector struct {
	log *slog.Logger

	// container, when set, runs the detect script in a container instead of on the host.
	container *DetectContainer
}

/*
DetectContainer is where the detect script runs under the docker driver.

	The script arrives in the pull request, so on the host it is anyone-who-can-open-one running code
	on the machine that holds the vault and the GitHub App key. The build already runs in a container
	for that reason, and the detect script, which runs first, ran outside it.

	Its container is stricter than the build's: no network, the checkout mounted read-only, and none
	of the build's environment. A detect script reads a list of paths and answers yes or no.
*/
type DetectContainer struct {
	Image  string
	CPUs   float64
	Memory string
}

// InContainer returns a copy of the detector that runs scripts in c.
func (d *Detector) InContainer(c DetectContainer) *Detector {
	copied := *d
	copied.container = &c
	return &copied
}

// NewDetector returns a Detector.
func NewDetector(log *slog.Logger) *Detector {
	return &Detector{log: log.With("component", "detect")}
}

// Detect classifies a change.
//
// A repository-supplied script, if configured, wins outright: the repository
// knows its own layout better than any default glob list. Otherwise the changed
// paths are matched against the configured globs.
func (d *Detector) Detect(ctx context.Context, ws *Workspace, cfg config.RepoConfig, changed []string) (Decision, error) {
	if len(changed) == 0 {
		// No files means the platform reported an empty diff, which happens on
		// a reopened pull request whose branch never moved. There is nothing
		// new to preview, but there may well be an existing preview that
		// should stay up, so this is a skip rather than a failure.
		return Decision{Build: false, Reason: "No files changed in this pull request."}, nil
	}

	if cfg.Detect.Script != "" {
		decision, err := d.runScript(ctx, ws, cfg.Detect.Script, changed)
		if !errors.Is(err, errNoDetectScript) {
			return decision, err
		}
		if cfg.Detect.Implicit {
			d.log.Info("no detect script in this commit; the path globs decide", "script", cfg.Detect.Script)
			return d.matchGlobs(cfg.Detect.Paths, changed), nil
		}

		/* The script is configured and this commit does not contain it, so the globs decide.

		   A project row names a path, and the file lives in the branch, so the two disagree on every
		   branch that predates the file or removed it. Failing the build there is the wrong answer
		   twice over: nothing is wrong with the branch, and the pull request gets a red comment
		   about a path its author has never heard of.

		   Live on 2026-08-27: `netfoundry/docusaurus-shared` had `.nfpreview/detect` set on the
		   project before the file was committed, and every open pull request failed with
		   `lstat .../.nfpreview: no such file or directory`.

		   Falling back rather than building unconditionally, because the globs are what this
		   repository used before the script was configured, and they are the documented default.
		   The reason says the script was missing, so the pull request comment carries it and the
		   operator can see the row and the branch disagree. */
		d.log.Warn("the configured detect script is not in this commit; using the path globs instead",
			"script", cfg.Detect.Script,
			"fix", "commit it to this branch, or clear the field on the project")

		fallback := d.matchGlobs(cfg.Detect.Paths, changed)
		fallback.Reason = fmt.Sprintf("`%s` is not in this commit, so paths were matched instead. %s",
			cfg.Detect.Script, fallback.Reason)
		return fallback, nil
	}
	return d.matchGlobs(cfg.Detect.Paths, changed), nil
}

/*
errNoDetectScript means the configured script is not in the commit being built.

	A sentinel rather than an os.IsNotExist test at the call site, because only one of the several
	things runScript does can produce a missing file that is safe to ignore. A symlink that resolves
	outside the repository, a script that is not executable and a script that exits 3 are all
	different failures, and none of them should quietly become "match the globs".
*/
var errNoDetectScript = errors.New("the configured detect script is not in this commit")

// matchGlobs tests the changed paths against the configured globs.
func (d *Detector) matchGlobs(patterns, changed []string) Decision {
	if len(patterns) == 0 {
		// An explicitly empty pattern list is a configuration mistake, not an
		// instruction to never build. Defaulting to "build" keeps a
		// misconfigured repository noisy instead of silently dead.
		return Decision{Build: true, Reason: "No detect patterns configured; building everything."}
	}

	for _, file := range changed {
		norm := filepath.ToSlash(file)
		for _, pattern := range patterns {
			ok, err := doublestar.Match(pattern, norm)
			if err != nil {
				// A malformed glob is the repository's problem, but failing the
				// whole build over it would be disproportionate.
				d.log.Warn("ignoring malformed detect glob", "pattern", pattern, "error", err)
				continue
			}
			if ok {
				d.log.Debug("documentation change detected", "file", norm, "pattern", pattern)
				return Decision{Build: true, Reason: fmt.Sprintf("`%s` matched `%s`.", norm, pattern)}
			}
		}
	}

	return Decision{
		Build:  false,
		Reason: fmt.Sprintf("None of the %d changed files matched a documentation path.", len(changed)),
	}
}

// runScript executes the repository's detect script.
//
// The script receives the changed paths on stdin, one per line, so it does not
// have to reconstruct them from git. It runs with the workspace as its working
// directory and a five-line environment, because a detect script has no
// business reading the host's environment — and the host's environment on a
// build server contains things a pull request author should not see.
func (d *Detector) runScript(ctx context.Context, ws *Workspace, script string, changed []string) (Decision, error) {
	abs := filepath.Join(ws.Dir, filepath.FromSlash(script))

	// config validated that the path does not escape the repository, but
	// re-check against the resolved path: a symlink inside the working tree
	// could point anywhere, and the working tree is attacker-controlled.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Absent is a different answer from unreadable, and only absent is safe to fall back from.
		// See errNoDetectScript: the row names a path and the file lives in the branch, so the two
		// disagree on any branch that predates it.
		if os.IsNotExist(err) {
			return Decision{}, fmt.Errorf("%w: %s", errNoDetectScript, script)
		}
		return Decision{}, fmt.Errorf("detect script %q: %w", script, err)
	}
	rootResolved, err := filepath.EvalSymlinks(ws.Dir)
	if err != nil {
		return Decision{}, fmt.Errorf("resolving workspace: %w", err)
	}
	if !strings.HasPrefix(resolved, rootResolved+string(os.PathSeparator)) {
		return Decision{}, fmt.Errorf("detect script %q resolves outside the repository", script)
	}

	stdin := strings.Join(changed, "\n") + "\n"

	// Pulled before the timeout starts, so a first build on a new image is not a timed-out
	// detect script.
	if d.container != nil {
		if err := d.ensureImage(ctx); err != nil {
			return Decision{}, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()

	var cmd *exec.Cmd
	if d.container != nil {
		rel, err := filepath.Rel(rootResolved, resolved)
		if err != nil {
			return Decision{}, fmt.Errorf("locating detect script %q: %w", script, err)
		}
		var cleanup func()
		if cmd, cleanup, err = d.containerCommand(ctx, ws, filepath.ToSlash(rel)); err != nil {
			return Decision{}, err
		}
		defer cleanup()
	} else {
		cmd = exec.CommandContext(ctx, resolved)
		cmd.Dir = ws.Dir
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + ws.Dir,
			"NFPREVIEW=1",
		}
	}
	cmd.Stdin = strings.NewReader(stdin)

	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))

	if err == nil {
		reason := "Detect script requested a build."
		if trimmed != "" {
			reason = trimmed
		}
		return Decision{Build: true, Reason: reason}, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == skipExitCode {
		reason := "Detect script reported no documentation changes."
		if trimmed != "" {
			reason = trimmed
		}
		return Decision{Build: false, Reason: reason}, nil
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Decision{}, fmt.Errorf("detect script %q timed out after %s", script, detectTimeout)
	}
	return Decision{}, fmt.Errorf("detect script %q failed: %w: %s", script, err, trimmed)
}

// ensureImage pulls the detect container's image when it is not already local.
func (d *Detector) ensureImage(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "image", "inspect", d.container.Image).Run() == nil {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "docker", "pull", d.container.Image).CombinedOutput(); err != nil {
		return fmt.Errorf("pulling %s for the detect script: %w: %s",
			d.container.Image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

/*
containerCommand is `docker run` for the detect script at rel, and how to remove its container.

	The entrypoint is the script itself, so an image's own entrypoint (node's wraps its arguments in
	`node`) has no say in what runs, and the shebang decides as it does on the host. Named, so a
	cancelled run that kills the docker client does not leave the container running behind it.
*/
func (d *Detector) containerCommand(ctx context.Context, ws *Workspace, rel string) (*exec.Cmd, func(), error) {
	source, err := hostMountPath(ws.Dir)
	if err != nil {
		return nil, nil, err
	}
	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, nil, err
	}
	name := "nfpreview-detect-" + hex.EncodeToString(id[:])

	cmd := exec.CommandContext(ctx, "docker", detectRunArgs(*d.container, name, source, rel)...)
	cleanup := func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		// Usually already gone through --rm, and then this fails, which is the answer.
		_ = exec.CommandContext(rmCtx, "docker", "rm", "-f", name).Run()
	}
	return cmd, cleanup, nil
}

// detectRunArgs is the `docker run` command line, split out so it can be pinned without docker.
func detectRunArgs(c DetectContainer, name, source, rel string) []string {
	args := []string{
		"run", "--rm", "-i", "--name", name,
		"--pull", "never",
		"--network", "none",
		"--mount", "type=bind,source=" + source + ",target=/workspace,readonly",
		"--workdir", "/workspace",
		"--env", "NFPREVIEW=1",
		"--env", "HOME=/tmp",
		"--entrypoint", "/workspace/" + rel,
	}
	if c.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(c.CPUs, 'f', -1, 64))
	}
	if c.Memory != "" {
		args = append(args, "--memory", c.Memory)
	}
	return append(args, c.Image)
}
