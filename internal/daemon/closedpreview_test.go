package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// previewExists reports whether a row survives, which is the observable half of a teardown.
func previewExists(t *testing.T, st *store.Store, previewID string) bool {
	t.Helper()
	previews, err := st.ListPreviews(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range previews {
		if p.PreviewID == previewID {
			return true
		}
	}
	return false
}

// The startup scan tears down a preview whose pull request is no longer open.
//
// Teardown otherwise runs only from a closed webhook, so a pull request merged while the daemon
// was stopped keeps its preview, its artifacts and one reserved exposer name per retained build
// for as long as the installation lives.
func TestStartupTearsDownAClosedPullRequestsPreview(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	stillOpen := model.PullRequest{Repo: repo, Number: 5, Branch: "open", HeadSHA: "aaa1111"}
	merged := model.PullRequest{Repo: repo, Number: 4, Branch: "merged", HeadSHA: "bbb2222"}

	if err := st.SaveProject(ctx, store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, pr := range []model.PullRequest{stillOpen, merged} {
		if err := st.SavePreview(ctx, store.Preview{
			PreviewID: pr.PreviewID(), PR: pr, Name: pr.Branch, State: "ready",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Only one of the two comes back from the platform.
	client.openPRs = []model.PullRequest{stillOpen}

	d.backfillOpenPullRequests(ctx)

	if previewExists(t, st, merged.PreviewID()) {
		t.Error("the preview of a closed pull request survived the startup scan")
	}
	if !previewExists(t, st, stillOpen.PreviewID()) {
		t.Error("the scan tore down a preview whose pull request is still open")
	}
}

// A branch preview is never torn down by the scan.
//
// It belongs to a branch rather than to a pull request, so it appears in no listing of open pull
// requests and would otherwise be absent-and-therefore-closed on every restart. The permanent
// preview of `main` disappearing on each boot is the whole failure this guards.
func TestStartupDoesNotTearDownABranchPreview(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	open := model.PullRequest{Repo: repo, Number: 5, Branch: "open", HeadSHA: "aaa1111"}
	branch := model.PullRequest{Repo: repo, Number: 0, Branch: "main", HeadSHA: "ccc3333"}

	if err := st.SaveProject(ctx, store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, pr := range []model.PullRequest{open, branch} {
		if err := st.SavePreview(ctx, store.Preview{
			PreviewID: pr.PreviewID(), PR: pr, Name: pr.Branch, State: "ready",
		}); err != nil {
			t.Fatal(err)
		}
	}
	client.openPRs = []model.PullRequest{open}

	d.backfillOpenPullRequests(ctx)

	if !previewExists(t, st, branch.PreviewID()) {
		t.Error("the scan tore down a branch preview")
	}
}

// A listing that fails tears down nothing.
//
// The sweep reads absence from a listing as "this pull request is closed", so a platform that
// cannot be reached would otherwise read as every pull request closing at once — and a daemon
// booting before its network is the ordinary case, not the exceptional one.
func TestAFailedListingTearsDownNothing(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	pr := model.PullRequest{Repo: repo, Number: 4, Branch: "whatever", HeadSHA: "bbb2222"}

	if err := st.SaveProject(ctx, store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: pr.PreviewID(), PR: pr, Name: pr.Branch, State: "ready",
	}); err != nil {
		t.Fatal(err)
	}
	client.openPRsErr = errors.New("the platform is unreachable")

	d.backfillOpenPullRequests(ctx)

	if !previewExists(t, st, pr.PreviewID()) {
		t.Error("a failed listing tore down a preview")
	}
}

// One repository's listing decides nothing about another's.
//
// The candidates are grouped by repository and each group is judged only against its own
// listing. Judging every stored preview against one repository's open pull requests would
// delete every other project's previews on the first sweep.
func TestOneRepositorysListingDoesNotTearDownAnother(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	scanned := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	other := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "handbook"}
	open := model.PullRequest{Repo: scanned, Number: 5, Branch: "open", HeadSHA: "aaa1111"}
	elsewhere := model.PullRequest{Repo: other, Number: 5, Branch: "elsewhere", HeadSHA: "ddd4444"}

	// Only the first repository is a project, so only it is listed.
	if err := st.SaveProject(ctx, store.Project{
		Platform: string(scanned.Platform), Owner: scanned.Owner, Repo: scanned.Name, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, pr := range []model.PullRequest{open, elsewhere} {
		if err := st.SavePreview(ctx, store.Preview{
			PreviewID: pr.PreviewID(), PR: pr, Name: pr.Branch, State: "ready",
		}); err != nil {
			t.Fatal(err)
		}
	}
	client.openPRs = []model.PullRequest{open}

	d.backfillOpenPullRequests(ctx)

	if !previewExists(t, st, elsewhere.PreviewID()) {
		t.Error("scanning one repository tore down another repository's preview")
	}
}

// An open pull request's preview does not expire, however long it has been idle.
//
// The TTL measures time since the last build, so a review that sits over a long weekend is
// indistinguishable from one abandoned months ago. Expiring the first deletes the link a reviewer
// was about to open and, because teardown retracts, the comment explaining it as well. Live on
// 3 August 2026: four previews expired at 72h44m, one of them an open pull request.
func TestAnOpenPullRequestsPreviewDoesNotExpire(t *testing.T) {
	client := &fakeClient{}
	_, d, _ := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	open := model.PullRequest{Repo: repo, Number: 145, Branch: "testing-pr", HeadSHA: "aaa1111"}
	closed := model.PullRequest{Repo: repo, Number: 4, Branch: "merged", HeadSHA: "bbb2222"}
	client.openPRs = []model.PullRequest{open}

	stale := []store.Preview{
		{PreviewID: open.PreviewID(), PR: open, Name: open.Branch},
		{PreviewID: closed.PreviewID(), PR: closed, Name: closed.Branch},
	}

	stillOpen := d.openPullRequests(ctx, stale)

	if !stillOpen[open.PreviewID()] {
		t.Error("an open pull request was not recognised as open, so its preview would expire")
	}
	if stillOpen[closed.PreviewID()] {
		t.Error("a closed pull request was treated as open, so its preview would never expire")
	}
}

// A pull request is assumed open when the platform cannot say otherwise.
//
// Every caller uses this to decide whether to destroy a preview, so an unreachable platform must
// not read as every pull request closing at once.
func TestAnUnreachablePlatformMeansAssumeOpen(t *testing.T) {
	client := &fakeClient{}
	_, d, _ := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	pr := model.PullRequest{Repo: repo, Number: 4, Branch: "whatever", HeadSHA: "bbb2222"}
	stale := []store.Preview{{PreviewID: pr.PreviewID(), PR: pr, Name: pr.Branch}}

	for _, tc := range []struct {
		name  string
		setup func()
	}{
		{"the listing failed", func() { client.openPRsErr = errors.New("unreachable") }},
		{"the listing was empty", func() { client.openPRsErr = nil; client.openPRs = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			if !d.openPullRequests(ctx, stale)[pr.PreviewID()] {
				t.Error("a preview would have expired on an answer the platform never gave")
			}
		})
	}
}

// An empty listing tears down nothing.
//
// Zero open pull requests beside stored previews is the shape a revoked credential takes, and it
// is indistinguishable from a repository whose pull requests were all merged at once. Acting on
// it would delete every preview the project has on the strength of an API call that may not have
// been answered honestly, so the ambiguity is resolved by doing nothing.
func TestAnEmptyListingTearsDownNothing(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	pr := model.PullRequest{Repo: repo, Number: 4, Branch: "whatever", HeadSHA: "bbb2222"}

	if err := st.SaveProject(ctx, store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: pr.PreviewID(), PR: pr, Name: pr.Branch, State: "ready",
	}); err != nil {
		t.Fatal(err)
	}
	client.openPRs = nil

	d.backfillOpenPullRequests(ctx)

	if !previewExists(t, st, pr.PreviewID()) {
		t.Error("an empty listing tore down a preview")
	}
}
