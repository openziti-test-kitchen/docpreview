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
