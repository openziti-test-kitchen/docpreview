package daemon

import (
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// TestRebuildQueuesTheRecordedCommit.
//
// "Build this again" and "build what the branch has now" are different requests, and a push
// already covers the second. Rebuild is for what a push cannot fix: a build that failed on a
// bad cache entry, a timeout, an image since corrected, or a project setting changed under a
// preview nobody is pushing to.
//
// So it queues the commit on the row. Taking the branch tip instead would silently build
// something the operator did not ask for and, on a branch that has moved, would report
// against a commit they were not looking at.
func TestRebuildQueuesTheRecordedCommit(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := t.Context()

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
	}
	id := pr.PreviewID()
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "add-guide", URL: "https://add-guide.example/",
		Commit: "85912e2abcdef", State: scm.StateReady, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	found, err := d.RebuildPreview(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the preview was not found, so nothing was queued")
	}

	// Queued through the ordinary path, so a worker will claim it exactly as it would a
	// webhook's.
	jobs, err := st.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs queued, want 1", len(jobs))
	}
	if got := jobs[0].PR.HeadSHA; got != "85912e2abcdef" {
		t.Errorf("queued commit = %q, want the recorded one", got)
	}
	if jobs[0].PR.Number != 7 || jobs[0].PR.Branch != "add-guide" {
		t.Errorf("queued the wrong pull request: %+v", jobs[0].PR)
	}
}

// TestRebuildingAnUnknownPreviewIsNotAnError — the button is drawn from a status payload
// that can be a second out of date, so a preview torn down between the render and the click
// is the ordinary case rather than a failure. The caller distinguishes it from a real error
// by the boolean, which is why there is one.
func TestRebuildingAnUnknownPreviewIsNotAnError(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	found, err := d.RebuildPreview(t.Context(), "000000000000")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if found {
		t.Error("an unknown preview reported as rebuilt")
	}
}

// TestCancelReportsWhetherAnythingWasRunning — same reasoning, other direction: a build
// that finished on its own between the render and the click must not read as a failure, and
// the caller can only tell the difference if this says so.
func TestCancelReportsWhetherAnythingWasRunning(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	if d.CancelBuild(t.Context(), "000000000000") {
		t.Error("cancelling nothing reported success")
	}

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
	}
	id := pr.PreviewID()

	cancelled := false
	d.mu.Lock()
	d.running[id] = &build{pr: pr, started: time.Now(), cancel: func() { cancelled = true }}
	d.mu.Unlock()

	if !d.CancelBuild(t.Context(), id) {
		t.Fatal("cancelling a running build reported nothing to cancel")
	}
	if !cancelled {
		t.Error("the build's context was not cancelled")
	}

	// Cleared, not merely cancelled. Supersede relies on the same thing: an abandoned build
	// that is still registered passes its own isCurrent check and goes on to publish, which
	// is worse than not cancelling it at all.
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.running[id]; ok {
		t.Error("the cancelled build is still registered as running, so it may still publish")
	}
}
