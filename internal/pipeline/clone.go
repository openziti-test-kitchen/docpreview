// Package pipeline turns a pull request into a directory of static files: it
// clones the branch, decides whether the change is documentation, and runs the
// site build.
//
// Everything here treats the repository contents as hostile input. A pull
// request author controls the working tree, which means they control
// package.json, .docpreview.yml, and any script the build invokes. The
// mitigations are spelled out where they apply; the summary is that the local
// build driver is for repositories you already trust, and the docker driver is
// for the ones you do not.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
)

// cloneTimeout bounds a clone. A documentation repository that cannot be
// fetched in five minutes has a problem no retry will fix.
const cloneTimeout = 5 * time.Minute

// Cloner checks pull request branches out into workspaces.
type Cloner struct {
	root string
	log  *slog.Logger
}

// NewCloner returns a Cloner that creates workspaces under root.
func NewCloner(root string, log *slog.Logger) *Cloner {
	return &Cloner{root: root, log: log.With("component", "clone")}
}

// Workspace is a checked-out pull request on disk.
type Workspace struct {
	// Dir is the repository root.
	Dir string

	// PR is what was checked out. Carried so the builder can tell the build
	// what it is building — a site has no other way to know its own commit, and
	// without it a preview cannot show which push produced it.
	PR model.PullRequest
}

// Remove deletes the workspace.
func (w *Workspace) Remove() error {
	if w == nil || w.Dir == "" {
		return nil
	}
	return os.RemoveAll(w.Dir)
}

// Clone checks out pr at cloneURL into a fresh workspace.
//
// cloneURL carries an access token, so it is never logged and never placed
// anywhere an error message could reach. Git is invoked with the URL as an
// argument rather than through a credential helper because the token is
// single-use and short-lived; the tradeoff is that it is briefly visible in the
// process table, which is why the local build driver is documented as
// trusted-repositories-only.
func (c *Cloner) Clone(ctx context.Context, pr model.PullRequest, cloneURL string) (*Workspace, error) {
	dir := filepath.Join(c.root, pr.PreviewID())

	// A leftover workspace from a previous build of this pull request would
	// leave stale files in the tree — deleted pages would still be there, and
	// the preview would show documentation that no longer exists.
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clearing workspace: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating workspace: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()

	ws := &Workspace{Dir: dir, PR: pr}

	// Fetch by SHA rather than by branch name. Between the webhook firing and
	// this clone running, the branch can move — a reviewer pushing twice in
	// quick succession is ordinary — and building a different commit than the
	// one we are about to report on produces a preview that silently disagrees
	// with its own comment.
	steps := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", cloneURL},
		{"fetch", "--quiet", "--depth", "1", "origin", pr.HeadSHA},
		{"checkout", "--quiet", "FETCH_HEAD"},
	}

	for _, args := range steps {
		if err := c.git(ctx, dir, args...); err != nil {
			ws.Remove()
			return nil, err
		}
	}

	c.log.Info("cloned", "pr", pr.String(), "sha", pr.HeadSHA, "dir", dir)
	return ws, nil
}

// git runs one git command in dir, scrubbing credentials from any error.
func (c *Cloner) git(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Git must never stop to ask for credentials: an interactive prompt in a
	// daemon is a hang, not a question.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"GCM_INTERACTIVE=never",
	)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("git %s timed out after %s", args[0], cloneTimeout)
	}
	return fmt.Errorf("git %s failed: %w: %s", args[0], err, scrub(string(out)))
}

// scrub removes anything that looks like a credential from git output.
//
// Git echoes the remote URL in several of its error messages, and that URL
// contains an installation token. Without this, a clone failure would write a
// live GitHub credential into the log and, because build output is attached to
// the pull request comment, potentially into the pull request itself.
func scrub(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, scrubLine(line))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// scrubLine redacts the userinfo of every URL on one line.
//
// The rule is RFC 3986's: after "://", the authority runs to the first "/", "?"
// or "#", and userinfo is everything before the **last** "@" inside it.
//
// The last, not the first. This was originally the first, which leaked the
// secret it existed to hide the moment a username contained an unescaped "@" —
// a Bitbucket credential is an email address, so
//
//	https://someone@example.com:TOKEN@bitbucket.org/ws/repo.git
//
// redacted "someone", emitted ":TOKEN@bitbucket.org/..." verbatim, and wrote a
// live token into the build log and from there into the pull request comment.
// An unescaped "@" in userinfo is not legal, but git accepts it and people write
// it, so the scrubber has to survive it.
func scrubLine(line string) string {
	var b strings.Builder
	for {
		i := strings.Index(line, "://")
		if i < 0 {
			b.WriteString(line)
			return b.String()
		}

		b.WriteString(line[:i+3])
		rest := line[i+3:]

		// The authority ends at the first path, query or fragment delimiter, or
		// at whitespace, since these lines are prose with URLs embedded in them.
		end := strings.IndexAny(rest, "/?# \t")
		if end < 0 {
			end = len(rest)
		}

		at := strings.LastIndexByte(rest[:end], '@')
		if at < 0 {
			// No userinfo. Emit the authority and carry on: a later URL on the
			// same line still has to be scrubbed.
			b.WriteString(rest[:end])
			line = rest[end:]
			continue
		}

		b.WriteString("***REDACTED***@")
		line = rest[at+1:]
	}
}
