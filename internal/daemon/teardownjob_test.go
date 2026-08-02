package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// A pending job must not survive the teardown of its preview.
//
// Teardown must remove a `jobs` row itself, not rely on `Claim` alone: a push landing moments before a
// close could otherwise leave a job behind for a worker to claim minutes later, build, and publish a
// share for something that had been deliberately removed. From the operator's side that is a deleted
// preview reappearing on its own.
//
// Asserted through `Claim` rather than through `PendingCount`, because a worker is what does
// the damage and `Claim` is the call it makes.
func TestTeardownRemovesThePendingJob(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  20,
		Branch:  "feature/pricing",
		HeadSHA: "a4fd6c9db1940992c8af5c48401462100bd7d2f1",
	}
	id := pr.PreviewID()

	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "feature-pricing",
		URL: "https://feature-pricing.example/", State: scm.StateReady,
	}); err != nil {
		t.Fatal(err)
	}
	// The push that lands just before the close.
	if err := st.Enqueue(ctx, pr); err != nil {
		t.Fatal(err)
	}

	if err := d.teardown(ctx, pr, id); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if job, err := st.Claim(ctx); !errors.Is(err, store.ErrNoJobs) {
		t.Errorf("a worker claimed %s after its preview was torn down (err %v)", job.String(), err)
	}
}

// Unlinking is the case that makes this user-facing. It is a button an operator presses to
// make docpreview stop, and the ignore it records is only checked by handleBuild — a job
// already in the queue never passes through that check again, so without the delete the
// unlinked pull request builds one more time and puts its preview back.
func TestUnlinkingRemovesTheQueuedBuild(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := unlinkPR()
	id := pr.PreviewID()

	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "feature-pricing",
		URL: "https://feature-pricing.example/", State: scm.StateReady,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Enqueue(ctx, pr); err != nil {
		t.Fatal(err)
	}

	found, err := d.UnlinkPreview(ctx, id)
	if err != nil {
		t.Fatalf("unlinking: %v", err)
	}
	if !found {
		t.Fatal("unlinking a stored preview reported it missing")
	}

	n, err := st.PendingCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d job(s) still queued for an unlinked pull request", n)
	}
}

// Only this preview's job. Teardown runs per preview — a closed pull request, a TTL sweep —
// and one that emptied the queue would silently drop the builds every other pull request is
// waiting for.
func TestTeardownLeavesOtherPreviewsQueued(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	closed := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  20,
		Branch:  "feature/pricing",
		HeadSHA: "a4fd6c9db1940992c8af5c48401462100bd7d2f1",
	}
	other := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  21,
		Branch:  "feature/billing",
		HeadSHA: "0d5f1e2a3b4c5d6e7f8091a2b3c4d5e6f708192a",
	}
	for _, pr := range []model.PullRequest{closed, other} {
		if err := st.Enqueue(ctx, pr); err != nil {
			t.Fatal(err)
		}
	}

	if err := d.teardown(ctx, closed, closed.PreviewID()); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	job, err := st.Claim(ctx)
	if err != nil {
		t.Fatalf("the other pull request's queued build is gone: %v", err)
	}
	if job.PreviewID() != other.PreviewID() {
		t.Errorf("claimed %s, want the pull request that was not torn down", job.String())
	}
}

// A branch preview torn down by the TTL sweep or by hand loses its queued build too.
//
// Its own case because everything else about a branch preview's teardown is a special case
// — no comment to retract, not torn down by a closing pull request — and a queue row is the
// one part that is not: a build of a preview that no longer exists is as wrong for `main` as
// it is for a pull request.
func TestTeardownOfABranchPreviewRemovesItsJob(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:       model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Branch:     "main",
		BaseBranch: "main",
		HeadSHA:    "9f1c0a2b3c4d5e6f708192a3b4c5d6e7f8091a2b",
	}
	if !pr.IsBranch() {
		t.Fatal("the fixture is not a branch preview")
	}
	if err := st.Enqueue(ctx, pr); err != nil {
		t.Fatal(err)
	}

	if err := d.teardown(ctx, pr, pr.PreviewID()); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if job, err := st.Claim(ctx); !errors.Is(err, store.ErrNoJobs) {
		t.Errorf("a worker claimed %s after its branch preview was torn down (err %v)",
			job.String(), err)
	}
	// And still no comment anywhere near pull request 0.
	client.mu.Lock()
	retracted := len(client.retracted)
	client.mu.Unlock()
	if retracted != 0 {
		t.Errorf("tearing down a branch preview retracted %d comment(s)", retracted)
	}
}
