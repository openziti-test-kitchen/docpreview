package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testPR(number int, sha string) model.PullRequest {
	return model.PullRequest{
		Repo:           model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:         number,
		Branch:         "feature/x",
		HeadSHA:        sha,
		BaseBranch:     "main",
		InstallationID: 999,
	}
}

func TestEnqueueCoalescesPushesToOnePullRequest(t *testing.T) {
	// The behaviour that stops a reviewer's five typo commits from becoming
	// five builds and four wasted previews.
	ctx := context.Background()
	st := testStore(t)

	for _, sha := range []string{"aaa", "bbb", "ccc"} {
		if err := st.Enqueue(ctx, testPR(42, sha)); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.PendingCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pending = %d after three pushes to one PR, want 1", n)
	}

	got, err := st.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadSHA != "ccc" {
		t.Errorf("claimed sha = %q, want the newest push %q", got.HeadSHA, "ccc")
	}
}

func TestEnqueueKeepsPullRequestsSeparate(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	for _, n := range []int{1, 2, 3} {
		if err := st.Enqueue(ctx, testPR(n, "sha")); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.PendingCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("pending = %d for three distinct PRs, want 3", got)
	}
}

func TestClaimIsFIFO(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	for _, n := range []int{1, 2, 3} {
		if err := st.Enqueue(ctx, testPR(n, "sha")); err != nil {
			t.Fatal(err)
		}
		// enqueued_at has millisecond resolution; without a gap the ordering
		// between rows is unspecified and this test would be flaky rather than
		// meaningful.
		time.Sleep(2 * time.Millisecond)
	}

	for _, want := range []int{1, 2, 3} {
		pr, err := st.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if pr.Number != want {
			t.Errorf("claimed PR #%d, want #%d", pr.Number, want)
		}
	}
}

func TestPendingJobsCarryTheirEnqueueTime(t *testing.T) {
	// The dashboard renders a queued row's age against this. Falling back to the
	// preview record's own updated_at would be wrong for a rebuilt preview, where
	// that timestamp is whenever its *last* build finished: a job queued seconds
	// ago would display as hours old, on the one screen someone watches to see
	// whether the queue is moving.
	ctx := context.Background()
	st := testStore(t)

	before := time.Now()
	if err := st.Enqueue(ctx, testPR(42, "abc")); err != nil {
		t.Fatal(err)
	}
	after := time.Now()

	jobs, err := st.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d pending jobs, want 1", len(jobs))
	}
	if jobs[0].PR.Number != 42 {
		t.Errorf("PR = %d, want 42", jobs[0].PR.Number)
	}
	// Truncated to the millisecond on the way to sqlite, so the window has to
	// tolerate a rounding step in either direction.
	got := jobs[0].EnqueuedAt
	if got.Before(before.Add(-time.Millisecond)) || got.After(after.Add(time.Millisecond)) {
		t.Errorf("EnqueuedAt = %s, want between %s and %s", got, before, after)
	}
}

func TestEnqueueRefreshesTheEnqueueTimeOnCoalesce(t *testing.T) {
	// A second push to the same pull request replaces the job rather than adding
	// one. The row's age should then describe the newest push, because that is
	// the work actually waiting.
	ctx := context.Background()
	st := testStore(t)

	if err := st.Enqueue(ctx, testPR(42, "first")); err != nil {
		t.Fatal(err)
	}
	first, err := st.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)
	if err := st.Enqueue(ctx, testPR(42, "second")); err != nil {
		t.Fatal(err)
	}
	second, err := st.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(second) != 1 {
		t.Fatalf("got %d jobs after coalescing, want 1", len(second))
	}
	if !second[0].EnqueuedAt.After(first[0].EnqueuedAt) {
		t.Errorf("EnqueuedAt did not advance on the second push: %s then %s",
			first[0].EnqueuedAt, second[0].EnqueuedAt)
	}
}

func TestClaimOnEmptyQueue(t *testing.T) {
	if _, err := testStore(t).Claim(context.Background()); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("Claim on an empty queue = %v, want ErrNoJobs", err)
	}
}

func TestClaimIsAtomicUnderConcurrency(t *testing.T) {
	// Two workers must never get the same job. DELETE ... RETURNING is what
	// makes that true; this checks the claim actually reaches the database
	// rather than being served from a stale read.
	ctx := context.Background()
	st := testStore(t)

	const jobs = 20
	for n := 1; n <= jobs; n++ {
		if err := st.Enqueue(ctx, testPR(n, "sha")); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[int]int{}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				pr, err := st.Claim(ctx)
				if errors.Is(err, ErrNoJobs) {
					return
				}
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				mu.Lock()
				seen[pr.Number]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != jobs {
		t.Errorf("claimed %d distinct jobs, want %d", len(seen), jobs)
	}
	for n, count := range seen {
		if count != 1 {
			t.Errorf("job %d was claimed %d times", n, count)
		}
	}
}

func TestJobsSurviveReopen(t *testing.T) {
	// The reason the queue is on disk: a push landing during a deploy must not
	// vanish.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Enqueue(ctx, testPR(42, "abc")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	pr, err := second.Claim(ctx)
	if err != nil {
		t.Fatalf("the queued job did not survive a restart: %v", err)
	}
	if pr.Number != 42 || pr.HeadSHA != "abc" {
		t.Errorf("recovered job = #%d@%s, want #42@abc", pr.Number, pr.HeadSHA)
	}
}

func TestPreviewRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	pr := testPR(42, "abc")
	want := Preview{
		PreviewID:   pr.PreviewID(),
		PR:          pr,
		Name:        "feature-x",
		URL:         "https://feature-x.share.zrok.io/",
		BaseURL:     "/",
		ArtifactDir: "/tmp/artifacts/xyz",
		Commit:      "abc",
		State:       scm.StateReady,
		UpdatedAt:   time.Now(),
	}
	if err := st.SavePreview(ctx, want); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d previews, want 1", len(all))
	}

	got := all[0]
	if got.PreviewID != want.PreviewID || got.Name != want.Name || got.URL != want.URL {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got.State != scm.StateReady {
		t.Errorf("state = %q, want %q", got.State, scm.StateReady)
	}
	if got.PR.Repo.Platform != model.PlatformGitHub {
		t.Errorf("platform = %q", got.PR.Repo.Platform)
	}
	if got.PR.InstallationID != 999 {
		t.Errorf("installation id = %d, want 999", got.PR.InstallationID)
	}
}

func TestSavePreviewUpsertsRatherThanDuplicating(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	pr := testPR(42, "abc")
	base := Preview{PreviewID: pr.PreviewID(), PR: pr, BaseURL: "/", UpdatedAt: time.Now()}

	base.URL = "https://old.example.com/"
	if err := st.SavePreview(ctx, base); err != nil {
		t.Fatal(err)
	}
	base.URL = "https://new.example.com/"
	base.Commit = "def"
	if err := st.SavePreview(ctx, base); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d previews after two saves of one PR, want 1", len(all))
	}
	if all[0].URL != "https://new.example.com/" {
		t.Errorf("URL = %q, want the newer value", all[0].URL)
	}
}

func TestExpiredPreviews(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	fresh := testPR(1, "a")
	stale := testPR(2, "b")

	if err := st.SavePreview(ctx, Preview{
		PreviewID: fresh.PreviewID(), PR: fresh, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePreview(ctx, Preview{
		PreviewID: stale.PreviewID(), PR: stale, UpdatedAt: time.Now().Add(-96 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	expired, err := st.ExpiredPreviews(ctx, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 {
		t.Fatalf("got %d expired previews, want 1", len(expired))
	}
	if expired[0].PR.Number != 2 {
		t.Errorf("expired PR = #%d, want #2", expired[0].PR.Number)
	}
}

func TestDeletePreview(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	pr := testPR(42, "abc")
	if err := st.SavePreview(ctx, Preview{PreviewID: pr.PreviewID(), PR: pr, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePreview(ctx, pr.PreviewID()); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("got %d previews after delete, want 0", len(all))
	}
}
