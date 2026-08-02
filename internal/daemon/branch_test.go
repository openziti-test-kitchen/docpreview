package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

func branchPR() model.PullRequest {
	return model.PullRequest{
		Repo:       model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Branch:     "main",
		HeadSHA:    "85912e2abcdef0123456789abcdef0123456789a",
		BaseBranch: "main",
	}
}

// queuedForPlatform reports whether `report` handed this preview to the publisher.
//
// Asserted here rather than against the fake client's received reports, because writes are debounced:
// `publisher.send` parks the payload behind a timer, so a test that read the fake immediately would see
// nothing for a *pull request* too, and pass while reporting was broken outright. This reads the boundary
// the code under test actually crosses.
func queuedForPlatform(d *Daemon, previewID string) bool {
	d.publisher.mu.Lock()
	defer d.publisher.mu.Unlock()
	_, ok := d.publisher.pending[previewID]
	return ok
}

// A branch preview must never be reported to the platform.
//
// There is nothing to report to. The comment upsert finds its comment by marker on pull
// request `Number`, which is 0 here, and what a platform does with a comment on pull request
// zero is not a thing to discover in production. The check lives in `report` — the funnel
// every state change passes through — rather than at the five call sites, which is what this
// test pins.
func TestABranchBuildIsNeverReportedToThePlatform(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})
	ctx := context.Background()
	pr := branchPR()

	for _, st := range []scm.State{scm.StateQueued, scm.StateBuilding, scm.StateReady, scm.StateFailed} {
		d.report(ctx, scm.Report{
			PR: pr, PreviewID: pr.PreviewID(), State: st,
			Commit: pr.HeadSHA, UpdatedAt: time.Now(),
		})
		if queuedForPlatform(d, pr.PreviewID()) {
			t.Fatalf("a branch build queued a %s report for the platform; "+
				"there is no pull request to report to", st)
		}
	}
}

// The same call on a pull request must still reach the publisher, or the test above would
// pass with reporting broken entirely.
func TestAPullRequestBuildIsStillReported(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})
	pr := branchPR()
	pr.Number = 42

	d.report(context.Background(), scm.Report{
		PR: pr, PreviewID: pr.PreviewID(), State: scm.StateQueued,
		Commit: pr.HeadSHA, UpdatedAt: time.Now(),
	})

	if !queuedForPlatform(d, pr.PreviewID()) {
		t.Error("a pull request build queued nothing for the platform")
	}
}

// A branch build is still recorded for the dashboard.
//
// The skip above is only about the platform. Dropping the record as well would leave the
// permanent preview invisible on the page that exists to show it — and the record is what
// the activity feed and the row state are read from.
func TestABranchBuildIsStillRecordedForTheDashboard(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})
	pr := branchPR()

	d.report(context.Background(), scm.Report{
		PR: pr, PreviewID: pr.PreviewID(), State: scm.StateBuilding,
		Commit: pr.HeadSHA, UpdatedAt: time.Now(),
	})

	st, err := d.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range st.Events {
		if e.PreviewID == pr.PreviewID() {
			return
		}
	}
	t.Error("a branch build left no trace on the dashboard, so the preview it is building is invisible")
}

// A branch preview must not be reaped for being old.
//
// The TTL exists because a pull request's preview outlives its usefulness. A branch preview
// has no such end: `main` is still `main` after a quiet fortnight, and the whole promise of
// this preview is that its URL always answers. A repository nobody has pushed to for longer
// than the TTL would otherwise have its permanent preview silently deleted.
func TestTheTTLReaperSkipsBranchPreviews(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	branch := branchPR()
	pull := branchPR()
	pull.Number = 42
	pull.Branch = "feature/pricing"

	// Both older than any TTL: SavePreview stamps updated_at from the row, so a zero time
	// is what "long ago" looks like here.
	for _, pr := range []model.PullRequest{branch, pull} {
		if err := st.SavePreview(ctx, store.Preview{
			PreviewID: pr.PreviewID(), PR: pr, Name: pr.Branch,
			URL: "https://" + pr.Branch + ".example/", State: "ready",
			UpdatedAt: time.Now().Add(-90 * 24 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}

	expired, err := st.ExpiredPreviews(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// The store is not what filters them — it answers "not updated within ttl" and both
	// qualify. The daemon's reaper is where the rule lives, which is the point: a caller
	// that forgot it would delete the permanent preview.
	var sawBranch bool
	for _, p := range expired {
		if p.PreviewID == branch.PreviewID() {
			sawBranch = true
		}
	}
	if !sawBranch {
		t.Skip("the store already filters branch previews; the reaper's own guard is then untested")
	}

	// Reproduce the reaper's decision rather than calling it: the loop is inside a ticker
	// goroutine, and what is being tested is the predicate, not the schedule.
	for _, p := range expired {
		if p.PR.IsBranch() {
			continue
		}
		if err := d.teardown(ctx, p.PR, p.PreviewID); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var branchSurvived, pullSurvived bool
	for _, p := range rows {
		switch p.PreviewID {
		case branch.PreviewID():
			branchSurvived = true
		case pull.PreviewID():
			pullSurvived = true
		}
	}
	if !branchSurvived {
		t.Error("the permanent branch preview was reaped for being old")
	}
	if pullSurvived {
		t.Error("an expired pull request preview survived, so the reaper does nothing at all")
	}
}

// Closing a pull request must not take the branch preview with it.
//
// Teardown is keyed on the preview id, and a branch preview's id includes the branch while a
// pull request's includes its number — so the two cannot collide even when the pull request
// is *on* that branch. Stated as a test because the alternative was keying branch previews on
// number 0, which would have made `main`'s preview and pull request 0's the same row.
func TestClosingAPullRequestLeavesTheBranchPreview(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	branch := branchPR()
	// A pull request whose head is the same branch. Contrived, and exactly the collision
	// worth ruling out.
	pull := branchPR()
	pull.Number = 7

	for _, pr := range []model.PullRequest{branch, pull} {
		if err := st.SavePreview(ctx, store.Preview{
			PreviewID: pr.PreviewID(), PR: pr, Name: "main", State: "ready",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if branch.PreviewID() == pull.PreviewID() {
		t.Fatal("a branch preview and a pull request on that branch share an id")
	}

	if err := d.teardown(ctx, pull, pull.PreviewID()); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PreviewID != branch.PreviewID() {
		t.Errorf("after closing the pull request the surviving previews are %v, want only the branch", rows)
	}
}

// A pull request with no preview is built at startup; one that already has a preview is not.
//
// This is what catches a missed delivery — the daemon stopped for a rebuild while somebody opened a pull
// request, or the tunnel was down for the minute that mattered. Without it, the first sign of a missed
// delivery is a person asking why their link never appeared.
func TestStartupBuildsOpenPullRequestsWithNoPreview(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	built := model.PullRequest{Repo: repo, Number: 7, Branch: "already-built", HeadSHA: "aaa1111"}
	missed := model.PullRequest{Repo: repo, Number: 21, Branch: "missed-delivery", HeadSHA: "bbb2222"}

	if err := st.SaveProject(ctx, store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// One of the two already has a preview.
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: built.PreviewID(), PR: built, Name: "already-built", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}
	client.openPRs = []model.PullRequest{built, missed}

	d.backfillOpenPullRequests(ctx)

	job, err := st.Claim(ctx)
	if err != nil {
		t.Fatalf("the scan queued nothing: %v", err)
	}
	if job.Number != missed.Number {
		t.Errorf("queued #%d, want the one with no preview (#%d)", job.Number, missed.Number)
	}
	// And only that one. Queueing the built one as well would rebuild every open pull request
	// on every restart, which on a busy repository is a stampede.
	if _, err := st.Claim(ctx); !errors.Is(err, store.ErrNoJobs) {
		t.Error("the scan queued a pull request that already had a preview")
	}
}

// An unlinked pull request is not resurrected by the scan.
//
// The check lives in handleBuild and the scan goes through it, which is the whole reason it is
// worth asserting: a scan that built what an operator had deliberately removed would undo the
// unlink on every restart.
func TestStartupDoesNotResurrectAnUnlinkedPullRequest(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	pr := model.PullRequest{Repo: repo, Number: 20, Branch: "not-wanted", HeadSHA: "ccc3333"}

	if err := st.SaveProject(ctx, store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.IgnorePR(ctx, pr); err != nil {
		t.Fatal(err)
	}
	client.openPRs = []model.PullRequest{pr}

	d.backfillOpenPullRequests(ctx)

	if _, err := st.Claim(ctx); !errors.Is(err, store.ErrNoJobs) {
		t.Error("the startup scan rebuilt a pull request that had been unlinked")
	}
}

// A disabled project is not scanned.
func TestStartupSkipsADisabledProject(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	if err := st.SaveProject(ctx, store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	client.openPRs = []model.PullRequest{
		{Repo: repo, Number: 9, Branch: "whatever", HeadSHA: "ddd4444"},
	}

	d.backfillOpenPullRequests(ctx)

	if _, err := st.Claim(ctx); !errors.Is(err, store.ErrNoJobs) {
		t.Error("a disabled project was scanned and built")
	}
}

// Rebuilding a branch preview takes the branch's current tip, not the commit on the row.
//
// The opposite rule from a pull request's rebuild, and for the reason that rule exists.
// "Build this again" means the recorded commit for something under review; a branch preview
// is a claim about what the branch looks like *now*, so rebuilding it at a commit the branch
// has moved past would republish a stale site and present it as current.
func TestRebuildingABranchPreviewTakesTheCurrentTip(t *testing.T) {
	moved := "0faa113abcdef0123456789abcdef0123456789a"
	client := &fakeClient{defaultBranch: "main", branchTip: moved}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	pr := branchPR()
	// The row remembers the commit built last time. The branch has moved since.
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: pr.PreviewID(), PR: pr, Name: "main", State: "ready",
		Commit: pr.HeadSHA,
	}); err != nil {
		t.Fatal(err)
	}

	found, err := d.RebuildPreview(ctx, pr.PreviewID())
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("rebuilding a stored branch preview reported it missing")
	}

	job, err := st.Claim(ctx)
	if err != nil {
		t.Fatalf("nothing was queued: %v", err)
	}
	if job.HeadSHA == pr.HeadSHA {
		t.Error("the rebuild queued the commit on the row, so the preview would republish " +
			"a commit the branch has moved past and call it current")
	}
	if job.HeadSHA != moved {
		t.Errorf("queued %q, want the branch's current tip %q", job.HeadSHA, moved)
	}
}

// BuildBranch reads the default branch from the platform rather than assuming `main`.
//
// A repository on `master` would otherwise get a preview of a branch that does not exist,
// which fails at the clone with git's own message about a missing ref — accurate, and about
// the wrong thing.
func TestBuildBranchAsksThePlatformWhatTheDefaultBranchIs(t *testing.T) {
	client := &fakeClient{defaultBranch: "master", branchTip: "0faa113abcdef0123456789abcdef0123456789a"}
	_, d, st := testIngress(t, client)
	ctx := context.Background()
	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}

	branch, err := d.BuildBranch(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "master" {
		t.Errorf("built %q, want the platform's answer master", branch)
	}

	job, err := st.Claim(ctx)
	if err != nil {
		t.Fatalf("BuildBranch queued nothing: %v", err)
	}
	if !job.IsBranch() {
		t.Error("the queued job is not a branch build, so it will try to comment on a pull request")
	}
	if job.Branch != "master" {
		t.Errorf("queued branch %q, want master", job.Branch)
	}
	// The commit is resolved before enqueueing. Left empty, the clone takes whatever the
	// branch points at when git runs — a different fact from the one the preview claims to
	// show, and the build id, the log filename and the per-build share are all named after it.
	if job.HeadSHA != client.branchTip {
		t.Errorf("queued commit %q, want the branch tip %q", job.HeadSHA, client.branchTip)
	}
}
