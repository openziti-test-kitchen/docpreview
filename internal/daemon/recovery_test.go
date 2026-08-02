package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// The exposer that publishes nothing is buildshare_test.go's refusingExposer, reused
// rather than restated: recovery's failure is the same one — a controller that answers
// with no share — and a second fake for it would drift from the first.

// storedPreview writes a row that looks like a preview which built successfully and was
// published, with artifacts on disk — the only shape recovery will try to restore.
func storedPreview(t *testing.T, d *Daemon, st *store.Store, pr model.PullRequest) store.Preview {
	t.Helper()

	dir := filepath.Join(d.cfg.ArtifactsDir(), pr.PreviewID(), "site")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>ok</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := store.Preview{
		PreviewID:   pr.PreviewID(),
		PR:          pr,
		Name:        "docs-" + pr.Branch,
		URL:         "https://docs-" + pr.Branch + ".share.zrok.io/",
		BaseURL:     "/",
		ArtifactDir: dir,
		Commit:      pr.HeadSHA,
		State:       scm.StateReady,
	}
	if err := st.SavePreview(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAFailedRepublishStopsAdvertisingItsURL asserts that a preview whose republish fails stops
// advertising the URL it can no longer serve.
//
// A preview that built perfectly can still fail to restore at startup, if `CreateShare` times out.
// Without a write to the row here, `/status` would go on reporting `state: ready` with a URL that
// answers 502, and the dashboard, the status payload and the pull request comment would all go on
// offering an address with no listener behind it.
//
// The URL is the assertion that matters rather than the state, because the dashboard's Open
// button is enabled by the presence of a URL and not by the state — a rebuild leaves the
// previous share serving, so a state test would grey the button on every rebuild.
func TestAFailedRepublishStopsAdvertisingItsURL(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  7,
		Branch:  "pricing",
		HeadSHA: "a4fd6c9db1940992c8af5c48401462100bd7d2f1",
	}
	p := storedPreview(t, d, st, pr)
	d.exposer = refusingExposer{Exposer: d.exposer}

	if err := d.recover(ctx); err != nil {
		t.Fatalf("recovery failed outright; one preview that cannot publish must not stop it: %v", err)
	}

	rows, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got store.Preview
	for _, r := range rows {
		if r.PreviewID == p.PreviewID {
			got = r
		}
	}
	if got.PreviewID == "" {
		t.Fatal("the preview row was deleted; the artifacts are still good and Rebuild " +
			"is offered from the row, so there would be nothing left to rebuild from")
	}
	if got.URL != "" {
		t.Errorf("the row still advertises %q, which nothing is serving", got.URL)
	}
	if got.State != scm.StateFailed {
		t.Errorf("state = %q, want %q — `ready` with no share is the state that was reported "+
			"live all day", got.State, scm.StateFailed)
	}
	if !strings.Contains(got.Reason, "Rebuild") {
		t.Errorf("reason = %q, want it to name what the operator should do", got.Reason)
	}

	// And the status payload, because that is the copy the page actually reads.
	status, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range status.Previews {
		if sp.PreviewID == p.PreviewID && sp.URL != "" {
			t.Errorf("/status offers %q for a preview with no publication", sp.URL)
		}
	}
}

// The pull request comment is the copy a reviewer clicks, and nothing else would correct
// it: the row is not rebuilt until somebody pushes or presses Rebuild.
func TestAFailedRepublishReportsTheFailureToThePullRequest(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  7,
		Branch:  "pricing",
		HeadSHA: "a4fd6c9db1940992c8af5c48401462100bd7d2f1",
	}
	storedPreview(t, d, st, pr)
	d.exposer = refusingExposer{Exposer: d.exposer}

	if err := d.recover(ctx); err != nil {
		t.Fatal(err)
	}
	// Reports are debounced; Close is what a shutdown does and it flushes.
	d.publisher.Close()

	states := client.reportStates()
	if len(states) != 1 || states[0] != scm.StateFailed {
		t.Fatalf("reported %v to the pull request, want one %q", states, scm.StateFailed)
	}
	client.mu.Lock()
	r := client.reports[0]
	client.mu.Unlock()
	if r.URL != "" {
		t.Errorf("the failure report carries URL %q, which would be rendered as a live link", r.URL)
	}
	if r.Commit != pr.HeadSHA {
		t.Errorf("commit = %q, want the commit on the row: a report for a different commit "+
			"resets the lifecycle guard", r.Commit)
	}
}

// A branch preview must not be commented on, here as everywhere else.
//
// `report` returns early for IsBranch, and the failure path added here is a fifth caller
// of it — a report assembled at a call site that skipped the funnel would go looking for a
// comment on pull request 0.
func TestAFailedRepublishOfABranchPreviewCommentsOnNothing(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	// Number 0 is what makes it a branch preview.
	pr := model.PullRequest{
		Repo:       model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Branch:     "main",
		BaseBranch: "main",
		HeadSHA:    "9f1c0a2b3c4d5e6f708192a3b4c5d6e7f8091a2b",
	}
	if !pr.IsBranch() {
		t.Fatal("the fixture is not a branch preview")
	}
	p := storedPreview(t, d, st, pr)
	d.exposer = refusingExposer{Exposer: d.exposer}

	if err := d.recover(ctx); err != nil {
		t.Fatal(err)
	}
	d.publisher.Close()

	if states := client.reportStates(); len(states) != 0 {
		t.Errorf("a branch preview reported %v to a platform that has no pull request for it", states)
	}
	// The row is still corrected, because the dashboard is where a branch preview is read.
	rows, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.PreviewID == p.PreviewID && r.URL != "" {
			t.Errorf("a branch preview still advertises %q", r.URL)
		}
	}
}

// A preview that restores must be left exactly as it was.
//
// The counterpart that stops the fix above from being "empty every URL at startup", which
// would pass every assertion here except this one.
func TestASuccessfulRepublishKeepsItsURL(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  8,
		Branch:  "billing",
		HeadSHA: "0d5f1e2a3b4c5d6e7f8091a2b3c4d5e6f708192a",
	}
	p := storedPreview(t, d, st, pr)

	if err := d.recover(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.PreviewID != p.PreviewID {
			continue
		}
		if r.URL == "" {
			t.Error("a preview that republished has no URL")
		}
		if r.State != scm.StateReady {
			t.Errorf("state = %q, want %q for a preview that is being served", r.State, scm.StateReady)
		}
	}
	d.mu.Lock()
	_, live := d.live[p.PreviewID]
	d.mu.Unlock()
	if !live {
		t.Error("nothing is published for a preview that restored")
	}
}
