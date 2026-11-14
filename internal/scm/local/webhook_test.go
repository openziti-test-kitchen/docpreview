package local

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// testRepo makes a bare repository named docs with one commit on main, and returns a client
// for it and that commit.
func testRepo(t *testing.T) (*Client, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repos := t.TempDir()
	work := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir,
			"-c", "user.name=test", "-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(work, "init", "--quiet", "--initial-branch", "main")
	if err := os.WriteFile(filepath.Join(work, "page.md"), []byte("# hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plumbing rather than `git commit`, so the test does not run whatever commit hooks the
	// machine it is on has configured.
	run(work, "add", "page.md")
	sha := run(work, "commit-tree", run(work, "write-tree"), "-m", "one")
	run(work, "update-ref", "refs/heads/main", sha)
	run(repos, "clone", "--quiet", "--bare", work, filepath.Join(repos, "docs.git"))

	c, err := New(config.LocalSCMConfig{ReposDir: repos, DefaultBase: "main"}, nil,
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return c, sha
}

func deliver(t *testing.T, c *Client, ev Event) ([]scm.Event, error) {
	t.Helper()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return c.VerifyWebhook(context.Background(), nil, body)
}

// The delivery chooses the commit, and with no webhook_secret nobody signed it. The commit
// goes on to name a directory that is deleted and to be an argument to git.
func TestALocalDeliveryMustCarryARealCommit(t *testing.T) {
	c, sha := testRepo(t)

	for _, bad := range []string{
		"../../../../",
		"--upload-pack=touch pwned",
		"-q",
		"abc123",
		strings.ToUpper(sha),
		sha + "\n",
	} {
		events, err := deliver(t, c, Event{Repo: "docs", Number: 1, Branch: "main", SHA: bad})
		if err == nil {
			t.Errorf("sha %q produced %d events, want a refusal", bad, len(events))
		}
	}

	// A real one is accepted, and so is leaving it out: the branch is resolved instead.
	for _, good := range []string{sha, ""} {
		events, err := deliver(t, c, Event{Repo: "docs", Number: 1, Branch: "main", SHA: good})
		if err != nil {
			t.Fatalf("sha %q: %v", good, err)
		}
		if len(events) != 1 || events[0].PR.HeadSHA != sha {
			t.Errorf("sha %q: got %+v, want one build of %s", good, events, sha)
		}
		if !model.IsCommitSHA(events[0].PR.HeadSHA) {
			t.Errorf("the event carries %q, which is not a commit", events[0].PR.HeadSHA)
		}
	}
}

// The branch and the base are arguments to git too.
func TestALocalDeliveryCannotPassGitAnOption(t *testing.T) {
	c, sha := testRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")

	for _, ev := range []Event{
		{Repo: "docs", Number: 1, Branch: "--upload-pack=touch " + marker},
		{Repo: "docs", Number: 1, Branch: "--output=" + marker},
		{Repo: "docs", Number: 1, Branch: "-h"},
		{Repo: "docs", Number: 1, Branch: "main", SHA: sha, Base: "--output=" + marker},
		{Repo: "docs", Number: 1, Branch: "ma\nin", SHA: sha},
	} {
		if events, err := deliver(t, c, ev); err == nil {
			t.Errorf("branch %q base %q produced %d events, want a refusal", ev.Branch, ev.Base, len(events))
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("git was given an option that wrote a file")
	}

	// ChangedFiles is reached by a rebuild or a scan without passing through the webhook.
	pr := model.PullRequest{
		Repo:       model.Repo{Platform: model.PlatformLocal, Owner: "local", Name: "docs"},
		Number:     1,
		Branch:     "main",
		HeadSHA:    sha,
		BaseBranch: "--output=" + marker,
	}
	if files, err := c.ChangedFiles(context.Background(), pr); err == nil {
		t.Errorf("a base that is an option was diffed: %v", files)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("git diff was given --output and wrote a file")
	}
}

// WebhookUnsigned is what tells the ingress to hold deliveries to this machine.
func TestWebhookUnsignedFollowsTheSecret(t *testing.T) {
	open := &Client{cfg: config.LocalSCMConfig{}}
	if !open.WebhookUnsigned() {
		t.Error("a client with no secret says its deliveries are signed")
	}
	signed := &Client{cfg: config.LocalSCMConfig{WebhookSecret: "s3cret"}}
	if signed.WebhookUnsigned() {
		t.Error("a client with a secret says its deliveries are unsigned")
	}
}
