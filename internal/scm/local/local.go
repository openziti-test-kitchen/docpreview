// Package local implements scm.Client against a git repository on this
// machine, standing in for a hosted one.
//
// It exists so the whole flow can be felt without a GitHub App: make a change,
// commit, push, watch a build start, watch a comment appear, push again, watch
// the same comment update. Everything a hosted platform does that docpreview
// depends on is small enough to emulate — a webhook, a clone URL, a changed-file
// list, and a comment that can be edited.
//
// This is not a mock. It runs the same daemon, the same queue, the same clone,
// the same build, the same exposer and the same comment renderer as a real
// pull request does. The only substitutions are that the "remote" is a bare
// repository in a directory, and the comment goes to a page docpreview serves
// instead of to github.com.
package local

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// CommentSink is where comments go. The store implements it.
type CommentSink interface {
	UpsertComment(ctx context.Context, c store.Comment) error
	DeleteComment(ctx context.Context, previewID string) error
}

// Client speaks to local bare git repositories.
type Client struct {
	cfg   config.LocalSCMConfig
	log   *slog.Logger
	sink  CommentSink
	owner string
}

// New builds a local SCM client.
func New(cfg config.LocalSCMConfig, sink CommentSink, log *slog.Logger) (*Client, error) {
	if cfg.ReposDir == "" {
		return nil, fmt.Errorf("local.repos_dir must be set")
	}
	if err := os.MkdirAll(cfg.ReposDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", cfg.ReposDir, err)
	}
	return &Client{
		cfg:   cfg,
		log:   log.With("scm", "local"),
		sink:  sink,
		owner: "local",
	}, nil
}

func (c *Client) Platform() model.Platform { return model.PlatformLocal }

// Validate confirms git is available and the repository directory is usable.
func (c *Client) Validate(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "git", "--version")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git is not usable (%s): %w", strings.TrimSpace(string(out)), err)
	}
	repos, err := c.Repos()
	if err != nil {
		return err
	}
	c.log.Info("local scm ready", "repos_dir", c.cfg.ReposDir, "repos", len(repos))
	return nil
}

// Repos lists the bare repositories docpreview will accept pushes for.
func (c *Client) Repos() ([]string, error) {
	entries, err := os.ReadDir(c.cfg.ReposDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", c.cfg.ReposDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".git") {
			out = append(out, strings.TrimSuffix(e.Name(), ".git"))
		}
	}
	return out, nil
}

// repoPath resolves a repository name to its bare directory.
//
// The name arrives from a webhook, so it is checked rather than trusted: it
// selects a directory to run git against, and a name containing a separator
// would run it somewhere else entirely.
func (c *Client) repoPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("no repository name given")
	}
	if strings.ContainsAny(name, `/\:`) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid repository name %q", name)
	}
	path := filepath.Join(c.cfg.ReposDir, name+".git")
	if stat, err := os.Stat(path); err != nil || !stat.IsDir() {
		return "", fmt.Errorf("no repository %q in %s "+
			"(create one with: docpreview sim init %s)", name, c.cfg.ReposDir, name)
	}
	return path, nil
}

// Event is the webhook payload. It is deliberately the smallest thing that
// carries the same information docpreview reads out of a GitHub delivery.
type Event struct {
	// Action is opened, synchronize, reopened, or closed — the same vocabulary
	// GitHub uses, so the mental model transfers.
	Action string `json:"action"`

	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	Base   string `json:"base"`
}

// VerifyWebhook authenticates a delivery and normalizes it.
//
// The signature is optional here and mandatory on GitHub, which is the one
// place this deliberately diverges: a loopback endpoint you curl by hand should
// not require an HMAC to try. Set local.webhook_secret and it behaves exactly
// like the GitHub path, same header name and same constant-time comparison, so
// the signing flow can be exercised too.
func (c *Client) VerifyWebhook(_ context.Context, headers map[string][]string, body []byte) ([]scm.Event, error) {
	if c.cfg.WebhookSecret != "" {
		sig := http.Header(headers).Get("X-Hub-Signature-256")
		if !verifySignature([]byte(c.cfg.WebhookSecret), body, sig) {
			// The shared sentinel, so the ingress answers 401 rather than 400.
			// A bare error here would make a bad signature on this platform look
			// like a malformed body, telling a caller guessing at the secret
			// that its guess was at least well-formed.
			return nil, scm.ErrBadSignature
		}
	}

	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("parsing the event: %w", err)
	}
	if ev.Repo == "" {
		return nil, fmt.Errorf(`the event needs a "repo"`)
	}
	if ev.Number == 0 {
		return nil, fmt.Errorf(`the event needs a "number"`)
	}

	repoPath, err := c.repoPath(ev.Repo)
	if err != nil {
		return nil, err
	}

	if ev.Base == "" {
		ev.Base = c.cfg.DefaultBase
	}
	if ev.Branch == "" {
		return nil, fmt.Errorf(`the event needs a "branch"`)
	}

	// Resolving the SHA here rather than trusting the payload mirrors what a
	// hosted platform does, and it means a hand-written curl can leave it out.
	if ev.SHA == "" {
		ev.SHA, err = gitOutput(repoPath, "rev-parse", ev.Branch)
		if err != nil {
			return nil, fmt.Errorf("resolving branch %q in %s: %w", ev.Branch, ev.Repo, err)
		}
	}

	pr := model.PullRequest{
		Repo: model.Repo{
			Platform: model.PlatformLocal,
			Owner:    c.owner,
			Name:     ev.Repo,
			CloneURL: repoPath,
		},
		Number:     ev.Number,
		Branch:     ev.Branch,
		HeadSHA:    ev.SHA,
		BaseBranch: ev.Base,
	}

	switch ev.Action {
	case "", "opened", "synchronize", "reopened", "ready_for_review":
		return []scm.Event{{Kind: scm.EventBuild, PR: pr, Delivery: ev.SHA}}, nil
	case "closed":
		return []scm.Event{{Kind: scm.EventTeardown, PR: pr, Delivery: ev.SHA}}, nil
	default:
		c.log.Debug("ignoring action", "action", ev.Action)
		return nil, nil
	}
}

func verifySignature(secret, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// CloneURL returns the bare repository's path.
//
// git clones happily from a local path, so the pipeline needs no special case:
// the same `git init` / `git remote add` / `git fetch <sha>` sequence runs
// against a directory instead of an HTTPS endpoint. And there is no credential
// in it, which is the one way this is simpler than the hosted clients.
func (c *Client) CloneURL(_ context.Context, pr model.PullRequest) (string, error) {
	return c.repoPath(pr.Repo.Name)
}

// ChangedFiles lists what the branch touches relative to its merge base.
//
// The hosted clients ask the platform because computing this locally would
// require history a depth-1 clone does not have. Here the bare repository *is*
// the platform and has all of it, so the answer comes from git directly.
func (c *Client) ChangedFiles(ctx context.Context, pr model.PullRequest) ([]string, error) {
	repoPath, err := c.repoPath(pr.Repo.Name)
	if err != nil {
		return nil, err
	}

	head := pr.HeadSHA
	if head == "" {
		head = pr.Branch
	}

	// Three dots: changes on the branch since it diverged, not changes since
	// the tip of base. Otherwise every commit landing on main would show up as
	// a change to every open branch.
	out, err := gitOutput(repoPath, "diff", "--name-only", pr.BaseBranch+"..."+head)
	if err != nil {
		// A brand-new repository has no base branch yet, and a first push has
		// nothing to diff against. Treat everything in the tree as changed
		// rather than failing, which matches what "opened" means.
		c.log.Debug("no merge base, treating the whole tree as changed",
			"repo", pr.Repo.Name, "base", pr.BaseBranch, "error", err)
		out, err = gitOutput(repoPath, "ls-tree", "-r", "--name-only", head)
		if err != nil {
			return nil, fmt.Errorf("listing files in %s at %s: %w", pr.Repo.Name, head, err)
		}
	}

	var files []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// Publish writes the comment.
//
// Same renderer as GitHub uses, same marker, same upsert semantics — the only
// difference is where it lands. That matters: the thing being demonstrated is
// that one comment gets edited rather than five being posted, and using a
// different renderer here would demonstrate nothing.
func (c *Client) Publish(ctx context.Context, r scm.Report) error {
	body := scm.RenderComment(r)

	if err := c.sink.UpsertComment(ctx, store.Comment{
		PreviewID: r.PreviewID,
		PR:        r.PR,
		Body:      body,
	}); err != nil {
		return err
	}

	c.log.Info("comment updated",
		"pr", r.PR.String(), "state", r.State, "url", r.URL,
		"view", "http://<listen>/pr/"+r.PR.Repo.Name+"/"+itoa(r.PR.Number))
	return nil
}

// Retract removes the comment.
func (c *Client) Retract(ctx context.Context, pr model.PullRequest) error {
	return c.sink.DeleteComment(ctx, pr.PreviewID())
}

// gitOutput runs a git command in a repository and returns trimmed stdout.
//
// stdout and stderr are captured separately, and the error carries stderr.
// Using cmd.Output() and reporting only the exit status would throw away the
// one sentence that says what went wrong, leaving "git diff: exit status 128"
// as the entire diagnosis.
//
// The error text is scrubbed on the way out. Nothing in a *local* repository
// path is secret, so this is belt and braces here; it matters because the
// hosted clients build URLs with an access token in them and git echoes the
// remote it failed on. Doing it uniformly means the next command added to this
// file inherits the containment rather than having to remember it. The hosted
// path has its own scrubber in internal/pipeline/clone.go.
func gitOutput(repoPath string, args ...string) (string, error) {
	full := append([]string{"-C", repoPath}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, scrubGitOutput(detail))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// scrubGitOutput removes anything shaped like a credential from git's output.
//
// Deliberately duplicated in spirit rather than shared with
// internal/pipeline/clone.go: importing that package here for one function
// would invert the dependency between the SCM layer and the pipeline. The rule
// is the same — userinfo in a URL is a secret — and it is three lines.
func scrubGitOutput(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}

		b.WriteString(s[:i+3])
		rest := s[i+3:]

		at := strings.IndexByte(rest, '@')
		if at < 0 || strings.ContainsAny(rest[:at], " \t/") {
			// The "@" belongs to something past the host, not to userinfo.
			// Consume one byte so the scan advances and cannot loop.
			b.WriteString(rest[:min(len(rest), 1)])
			s = rest[min(len(rest), 1):]
			continue
		}

		b.WriteString("***REDACTED***@")
		s = rest[at+1:]
	}
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// Ensure the contract is satisfied at compile time rather than at the first
// webhook.
var _ scm.Client = (*Client)(nil)

var _ = time.Now
