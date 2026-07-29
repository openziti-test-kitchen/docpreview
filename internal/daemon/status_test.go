package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// Status composes three sources: the preview table, the job queue, and the map
// of builds currently running. Only the first is on disk.
//
// It used to read the table alone, and the table only ever holds committed
// states — a preview row is written when a build succeeds and left untouched
// while the next one runs. So `building` and `queued` could not appear in the
// payload at all: the dashboard's Building counter read 0 while a build was
// visibly running, and a branch building for the first time had no row
// anywhere. The activity feed said "building" because events carry the state
// directly, which is what made the disagreement visible.

func testPR(repo string, number int, branch string) model.PullRequest {
	return model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: repo},
		Number:  number,
		Branch:  branch,
		HeadSHA: "0123456789abcdef0123456789abcdef01234567",
	}
}

func findPreview(s Status, id string) (StatusPreview, bool) {
	for _, p := range s.Previews {
		if p.PreviewID == id {
			return p, true
		}
	}
	return StatusPreview{}, false
}

func TestEventsCarryThePreviewTheyDescribe(t *testing.T) {
	// The activity rail turns an entry into a button that opens the build it
	// names, and it needs an exact handle to do it. Repo plus number would
	// nearly always identify the same thing, and "nearly" means opening the
	// wrong build rather than none — which is worse than no link at all.
	//
	// Both recorders matter. `record` covers every state change and `recordf`
	// covers teardown, and an empty ID from either renders as inert text with
	// no error anywhere.
	_, d, _ := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := testPR("docs", 7, "new-guide")
	id := pr.PreviewID()

	d.record(scm.Report{PR: pr, PreviewID: id, State: scm.StateReady}, "ready")
	d.recordf(pr, "torn-down", "preview withdrawn")

	s, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Events) < 2 {
		t.Fatalf("got %d events, want at least 2", len(s.Events))
	}
	for _, e := range s.Events {
		if e.PreviewID != id {
			t.Errorf("event %q carries preview_id %q, want %q", e.Kind, e.PreviewID, id)
		}
	}
}

func TestStatusReportsARunningBuildAsBuilding(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := testPR("docs", 7, "new-guide")
	id := pr.PreviewID()

	// A finished preview, exactly as a successful build leaves it.
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Commit: pr.HeadSHA, Name: "new-guide",
		URL: "http://127.0.0.1:9000/", State: scm.StateReady,
		UpdatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	before, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := findPreview(before, id); p.State != string(scm.StateReady) {
		t.Fatalf("state before the build = %q, want ready", p.State)
	}

	// The next push starts building. Nothing is written to the store.
	d.mu.Lock()
	d.running[id] = &build{cancel: func() {}, pr: pr, started: time.Now()}
	d.mu.Unlock()

	during, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := findPreview(during, id)
	if !ok {
		t.Fatal("the preview vanished while building")
	}
	if p.State != string(scm.StateBuilding) {
		t.Errorf("state = %q, want building — the counter reads this field", p.State)
	}
	if during.Running != 1 {
		t.Errorf("running = %d, want 1", during.Running)
	}
	// The URL of the preview still being served is not the URL of the build in
	// flight, and offering it as this row's link sends the reader to whichever
	// of the two happens to answer.
	if p.State == string(scm.StateBuilding) && p.URL != "http://127.0.0.1:9000/" {
		t.Errorf("URL = %q, want the previous preview's — it is still serving", p.URL)
	}
}

func TestStatusReportsAFirstBuildWithNoStoredRow(t *testing.T) {
	// The first build of a branch exists nowhere but memory until it commits,
	// which is exactly the moment somebody is watching the dashboard.
	_, d, _ := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := testPR("docs", 11, "brand-new")
	id := pr.PreviewID()

	d.mu.Lock()
	d.running[id] = &build{cancel: func() {}, pr: pr, started: time.Now()}
	d.mu.Unlock()

	s, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := findPreview(s, id)
	if !ok {
		t.Fatal("a build with no stored preview does not appear at all")
	}
	if p.State != string(scm.StateBuilding) {
		t.Errorf("state = %q, want building", p.State)
	}
	if p.Branch != "brand-new" || p.Number != 11 {
		t.Errorf("row = %s#%d, want brand-new#11", p.Branch, p.Number)
	}
}

func TestStatusReportsQueuedBuilds(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := testPR("docs", 3, "waiting")
	if err := st.Enqueue(ctx, pr); err != nil {
		t.Fatal(err)
	}

	s, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := findPreview(s, pr.PreviewID())
	if !ok {
		t.Fatal("a queued build does not appear")
	}
	if p.State != string(scm.StateQueued) {
		t.Errorf("state = %q, want queued", p.State)
	}
	if s.Pending != 1 {
		t.Errorf("pending = %d, want 1", s.Pending)
	}
}

func TestStatusPrefersBuildingOverQueued(t *testing.T) {
	// A push that supersedes a running build cancels it and enqueues the
	// replacement, so for a moment the same preview is both. Building is the
	// half doing work, and a row that flips to Queued mid-build reads as
	// progress going backwards.
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := testPR("docs", 5, "busy")
	id := pr.PreviewID()

	if err := st.Enqueue(ctx, pr); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.running[id] = &build{cancel: func() {}, pr: pr, started: time.Now()}
	d.mu.Unlock()

	s, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := findPreview(s, id); p.State != string(scm.StateBuilding) {
		t.Errorf("state = %q, want building", p.State)
	}

	// And it appears once, not twice.
	var n int
	for _, p := range s.Previews {
		if p.PreviewID == id {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the preview appears %d times, want 1", n)
	}
}
