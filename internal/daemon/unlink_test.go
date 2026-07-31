package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

func unlinkPR() model.PullRequest {
	return model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  20,
		Branch:  "feature/pricing",
		HeadSHA: "a4fd6c9db1940992c8af5c48401462100bd7d2f1",
	}
}

// An unlinked pull request must not be built, whatever asks for the build.
//
// The check lives in handleBuild precisely because every route to a build passes through
// it — a webhook delivery, the scan that runs when a project is added, an operator's
// rebuild. A check in any one caller would leave the others rediscovering the pull request
// an operator had removed, which is what "you keep finding PRs and adding a build for
// stuff I don't want" described.
func TestAnUnlinkedPullRequestIsNotBuilt(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()
	pr := unlinkPR()

	if err := st.IgnorePR(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if err := d.handleBuild(ctx, scm.Event{Kind: scm.EventBuild, PR: pr}); err != nil {
		t.Fatalf("handleBuild on an unlinked pull request errored: %v", err)
	}

	// Nothing queued. A job written here is a build that runs, so this is the assertion
	// that matters rather than the absence of a log line.
	job, err := st.Claim(ctx)
	if !errors.Is(err, store.ErrNoJobs) {
		t.Errorf("an unlinked pull request queued a build for %s (err %v)", job.String(), err)
	}
}

// Nothing else is affected. A check that stopped every build would pass the test above and
// break the daemon, which is the failure mode worth one more case.
func TestALinkedPullRequestStillBuilds(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()
	pr := unlinkPR()

	if err := d.handleBuild(ctx, scm.Event{Kind: scm.EventBuild, PR: pr}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Claim(ctx); err != nil {
		t.Fatalf("an ordinary pull request queued nothing: %v", err)
	}
}

// Linking is what undoes an unlink, and it has to undo it *before* queueing.
//
// handleBuild is where the ignore is enforced, so a link that queued first would be
// dropped by its own check — a button that reported success and did nothing.
func TestLinkingUnignoresBeforeItQueues(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()
	pr := unlinkPR()

	if err := st.IgnorePR(ctx, pr); err != nil {
		t.Fatal(err)
	}
	client.openPRs = []model.PullRequest{pr}

	if err := d.LinkPR(ctx, pr.Repo, pr.Number); err != nil {
		t.Fatalf("linking an unlinked pull request: %v", err)
	}

	if _, err := st.Claim(ctx); err != nil {
		t.Fatalf("linking an unlinked pull request queued nothing: %v", err)
	}
	ignored, err := st.PRIgnored(ctx, pr.Repo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if ignored {
		t.Error("the pull request is still unlinked after being linked")
	}
}

// Unlinking records the decision even when the teardown is the part that can fail.
//
// The ignore is written first for that reason: a recorded ignore with a half-removed
// preview is recoverable by unlinking again, while the reverse rebuilds itself the moment
// somebody pushes.
func TestUnlinkingRecordsTheDecisionAndRemovesThePreview(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()
	pr := unlinkPR()
	id := pr.PreviewID()

	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "feature-pricing",
		URL: "https://feature-pricing.example/", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}

	found, err := d.UnlinkPreview(ctx, id)
	if err != nil {
		t.Fatalf("unlinking: %v", err)
	}
	if !found {
		t.Fatal("unlinking a stored preview reported it missing")
	}

	ignored, err := st.PRIgnored(ctx, pr.Repo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !ignored {
		t.Error("the pull request was not recorded as unlinked, so discovery will rebuild it")
	}

	previews, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range previews {
		if p.PreviewID == id {
			t.Error("the preview row survived the unlink")
		}
	}

	// An unknown preview is not an error: the button is offered from a page that may be a
	// second out of date.
	found, err = d.UnlinkPreview(ctx, "ffffffffffff")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("unlinking an unknown preview claimed to have found one")
	}
}
