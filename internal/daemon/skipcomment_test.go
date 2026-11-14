package daemon

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// sourceRepo makes a git repository holding one documentation page and returns its path and
// the commit to build.
func sourceRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	src := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", src,
			"-c", "user.name=test", "-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	if err := os.MkdirAll(filepath.Join(src, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "docs", "intro.md"), []byte("# hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plumbing rather than `git commit`, so the test does not run whatever commit hooks the
	// machine it is on has configured.
	git("add", "docs/intro.md")
	sha := git("commit-tree", git("write-tree"), "-m", "one")
	git("update-ref", "refs/heads/main", sha)
	return src, sha
}

// A pull request that changes no documentation is skipped, and a skip posts no comment: a new
// comment notifies everybody on the pull request, and this one had nothing to tell them. Every
// report such a build sends may only edit a comment an earlier commit earned.
func TestASkippedPullRequestNeverPostsAComment(t *testing.T) {
	src, sha := sourceRepo(t)
	client := &fakeClient{cloneURL: src, noChanges: true}
	_, d, _ := testIngress(t, client)

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: sha,
	}
	if err := d.Handle(t.Context(), scm.Event{Kind: scm.EventBuild, PR: pr}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	d.build(ctx, pr)
	d.publisher.Close()

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reports) == 0 {
		t.Fatal("the skip reached the platform as nothing at all, so its check run was never written")
	}
	for _, r := range client.reports {
		if !r.UpdateOnly {
			t.Errorf("the %s report could post a new comment on a pull request that was skipped", r.State)
		}
	}
	if last := client.reports[len(client.reports)-1]; last.State != scm.StateSkipped {
		t.Errorf("the last report is %s, want %s", last.State, scm.StateSkipped)
	}
}

// The other half: once detection says there is documentation to build, the pull request gets
// its comment, and gets it then rather than when the build ends.
func TestABuildPostsItsCommentOnceDetectionPasses(t *testing.T) {
	src, sha := sourceRepo(t)

	// Cancelled as the announcement arrives, so the build that follows it stops instead of
	// running a real one. No debounce, so it arrives at once.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &fakeClient{cloneURL: src, onPublish: func(scm.Report) { cancel() }}
	_, d, _ := testIngress(t, client)
	d.publisher.debounce = 0

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: sha,
	}
	_, decision, _ := d.runPipeline(ctx, &build{}, client, pr, slog.New(slog.DiscardHandler))
	if !decision.Build {
		t.Fatalf("detection did not say build: %q", decision.Reason)
	}
	d.publisher.Close()

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, r := range client.reports {
		if r.State == scm.StateBuilding && !r.UpdateOnly {
			return
		}
	}
	t.Errorf("no building report able to post a comment arrived: %d report(s)", len(client.reports))
}

// A build that was superseded while it was cloning announces nothing. Detection by path reads no
// context, so such a build gets as far as the announcement, and without this it would write
// "building" for its old commit over the comment of the build that replaced it.
func TestASupersededBuildAnnouncesNothing(t *testing.T) {
	src, sha := sourceRepo(t)

	// Cancelled as the changed files are asked for, which is the step before detection.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &fakeClient{cloneURL: src, afterChanged: cancel}
	_, d, _ := testIngress(t, client)

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: sha,
	}
	d.runPipeline(ctx, &build{}, client, pr, slog.New(slog.DiscardHandler))
	d.publisher.Close()

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reports) != 0 {
		t.Errorf("a cancelled build sent %d report(s), the first %s", len(client.reports), client.reports[0].State)
	}
}

// And a build whose commit has already reported further is not announced as building again.
func TestABuildingAnnouncementDoesNotGoBackwards(t *testing.T) {
	src, sha := sourceRepo(t)
	client := &fakeClient{cloneURL: src}
	_, d, _ := testIngress(t, client)

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: sha,
	}
	d.report(t.Context(), scm.Report{PR: pr, PreviewID: pr.PreviewID(), State: scm.StateReady, Commit: sha})
	d.publisher.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client.onPublish = func(scm.Report) { cancel() }
	client.afterChanged = func() { time.AfterFunc(2*time.Second, cancel) }
	d.runPipeline(ctx, &build{}, client, pr, slog.New(slog.DiscardHandler))

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, r := range client.reports {
		if r.State == scm.StateBuilding {
			t.Error("a commit that had reported ready was announced as building again")
		}
	}
}
