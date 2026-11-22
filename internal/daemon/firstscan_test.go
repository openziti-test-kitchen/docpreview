package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// The first listing of a repository builds none of its open pull requests, and a later listing
// builds one only when its head has moved.
func TestTheFirstListingBuildsNoneOfTheOpenPullRequests(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	if err := st.SaveProject(ctx, store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	old := model.PullRequest{Repo: repo, Number: 10, Branch: "old", HeadSHA: "978f2cf"}
	bump := model.PullRequest{Repo: repo, Number: 11, Branch: "deps", HeadSHA: "3b1be8f"}
	queued := func() []int {
		var n []int
		for {
			job, err := st.Claim(ctx)
			if errors.Is(err, store.ErrNoJobs) {
				return n
			}
			if err != nil {
				t.Fatal(err)
			}
			n = append(n, job.Number)
		}
	}

	client.openPRs = []model.PullRequest{old, bump}
	d.backfillOpenPullRequests(ctx)
	if got := queued(); len(got) != 0 {
		t.Fatalf("the first listing queued %v, want nothing", got)
	}

	// A restart with nothing changed builds nothing either: they are still at the commits the
	// first listing saw.
	d.backfillOpenPullRequests(ctx)
	if got := queued(); len(got) != 0 {
		t.Fatalf("a second listing with no new commits queued %v, want nothing", got)
	}

	// a push to one pull request whose webhook was missed shows up as a new head, so that one
	// builds and the other does not
	bump.HeadSHA = "c0ffee1"
	client.openPRs = []model.PullRequest{old, bump}
	d.backfillOpenPullRequests(ctx)
	if got := queued(); len(got) != 1 || got[0] != bump.Number {
		t.Fatalf("after a push to #%d the scan queued %v, want only #%d", bump.Number, got, bump.Number)
	}

	// a pull request opened after the first listing was never seen, so a missed webhook for it
	// is still caught
	opened := model.PullRequest{Repo: repo, Number: 12, Branch: "new", HeadSHA: "abc1234"}
	client.openPRs = []model.PullRequest{old, opened}
	d.backfillOpenPullRequests(ctx)
	if got := queued(); len(got) != 1 || got[0] != opened.Number {
		t.Fatalf("a pull request opened after the first listing: queued %v, want #%d", got, opened.Number)
	}
}

// A failed listing records nothing, so the next successful one still counts as the first.
func TestAFailedListingIsNotAFirstListing(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
	if err := st.SaveProject(ctx, store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// the App is not installed yet, so the listing fails
	client.openPRsErr = errors.New("the GitHub App is not installed on acme/docs")
	d.backfillOpenPullRequests(ctx)

	client.openPRsErr = nil
	client.openPRs = []model.PullRequest{{Repo: repo, Number: 13, Branch: "sdk", HeadSHA: "6933bd0"}}
	d.backfillOpenPullRequests(ctx)
	if _, err := st.Claim(ctx); !errors.Is(err, store.ErrNoJobs) {
		t.Error("the first successful listing after a failed one built a pull request")
	}
}
