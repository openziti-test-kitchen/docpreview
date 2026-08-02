package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/scm"
)

// A preview whose republish failed must not go on advertising its URL.
//
// A build can succeed and leave a row saying `ready` with the URL it published, and a
// later restore can then fail to bind a share, leaving nothing answering at that URL. The
// dashboard and the pull request comment both read this row, so the row is where the
// failure has to be reflected.
func TestFailPreviewLeavesNoURLToOffer(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	pr := testPR(42, "abc")
	if err := st.SavePreview(ctx, Preview{
		PreviewID: pr.PreviewID(), PR: pr, Name: "docs-feature-x",
		URL: "https://docs-feature-x.share.zrok.io/", BaseURL: "/",
		ArtifactDir: "/tmp/artifacts/xyz", Commit: "abc",
		State: scm.StateReady, UpdatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	const reason = "creating the share timed out — press Rebuild"
	if err := st.FailPreview(ctx, pr.PreviewID(), reason); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d previews, want the one that was marked", len(all))
	}
	got := all[0]

	if got.URL != "" {
		t.Errorf("URL = %q, want empty — the dashboard's Open button is decided by the "+
			"presence of a URL, so anything here is offered as live", got.URL)
	}
	if got.State != scm.StateFailed {
		t.Errorf("state = %q, want %q", got.State, scm.StateFailed)
	}
	if got.Reason != reason {
		t.Errorf("reason = %q, want %q — an operator with no reason has nothing to act on",
			got.Reason, reason)
	}

	// The name stays. Teardown reads it to hand the exposer's reserved name back, and on
	// zrok that name is counted against the account; clearing it here would leak one per
	// failed restore with nothing left to name it.
	if got.Name != "docs-feature-x" {
		t.Errorf("name = %q, want it kept so the name can still be released", got.Name)
	}
	// And the artifacts, because the row is kept exactly so Rebuild has something cheap
	// to do and recovery can be told to try again.
	if got.ArtifactDir == "" || got.BaseURL == "" || got.Commit != "abc" {
		t.Errorf("marking a preview failed lost what a rebuild needs: %+v", got)
	}
	if !got.UpdatedAt.After(time.Now().Add(-time.Minute)) {
		t.Errorf("updated_at = %v, want now — the reaper ages a row on this and the "+
			"dashboard renders it", got.UpdatedAt)
	}
}

// Only the preview named. Recovery restores previews concurrently, so a marker that
// touched more than its own row would empty the URL of a preview that came back fine.
func TestFailPreviewTouchesNoOtherRow(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	broken := testPR(1, "aaa")
	fine := testPR(2, "bbb")
	if err := st.SavePreview(ctx, Preview{
		PreviewID: broken.PreviewID(), PR: broken, Name: "one",
		URL: "https://one.example/", State: scm.StateReady, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePreview(ctx, Preview{
		PreviewID: fine.PreviewID(), PR: fine, Name: "two",
		URL: "https://two.example/", State: scm.StateReady, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.FailPreview(ctx, broken.PreviewID(), "timed out"); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.PreviewID == fine.PreviewID() && p.URL != "https://two.example/" {
			t.Errorf("a preview that restored fine lost its URL: %+v", p)
		}
	}
}

// Teardown has to remove the queued build, and this is the statement that does it.
//
// Claim was the only thing that ever deleted a `jobs` row, so a push landing just before
// a close left a job a worker claimed minutes later — republishing a preview somebody had
// deleted. Asserted at this level as well as through teardown because the queue is where
// the resurrection comes from, and Claim finding nothing is the whole of the fix.
func TestDequeueRemovesTheJobAWorkerWouldClaim(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	pr := testPR(42, "abc")
	if err := st.Enqueue(ctx, pr); err != nil {
		t.Fatal(err)
	}

	removed, err := st.Dequeue(ctx, pr.PreviewID())
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("Dequeue reported nothing to remove for a job it was just handed")
	}
	if job, err := st.Claim(ctx); !errors.Is(err, ErrNoJobs) {
		t.Errorf("a worker still claimed %s (err %v)", job.String(), err)
	}

	// Idempotent, because teardown runs for previews that have nothing queued at all —
	// a TTL sweep, a closed pull request nobody pushed to — and none of those is a
	// failure.
	removed, err = st.Dequeue(ctx, pr.PreviewID())
	if err != nil {
		t.Fatalf("dequeueing a preview with no queued build errored: %v", err)
	}
	if removed {
		t.Error("Dequeue claimed to remove a job twice")
	}
}
