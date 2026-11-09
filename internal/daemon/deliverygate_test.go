package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// gatePR is the pull request every test here delivers, on a repository the fixture
// does not add a project for unless the test says so.
func gatePR() model.PullRequest {
	return model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "stranger", Name: "docs"},
		Number:  1,
		Branch:  "feature/x",
		HeadSHA: "abc123",
	}
}

// TestAVerifiedDeliveryWithNoProjectBuildsNothing is the property that makes a public
// GitHub App safe to install.
//
// The webhook secret is one value for the whole App and GitHub signs every installation's
// deliveries with it, so a valid signature says the delivery came from GitHub and nothing
// about whether the operator wanted this repository. Without this check, installing the App
// is enough to get a branch of your choosing cloned and its own `.nfpreview.yml` honoured on
// the host holding the App private key.
//
// Three assertions, and the second and third matter as much as the first. Nothing is queued.
// Nothing is written back to the platform — the queued report is a comment on somebody's pull
// request, so a check sited after it would let an uninvited repository make the App post into
// it. And the delivery is still answered 202, because refusing it with a distinguishable
// status would report the operator's project list one probe at a time.
func TestAVerifiedDeliveryWithNoProjectBuildsNothing(t *testing.T) {
	client := &fakeClient{}
	pr := gatePR()
	client.events = []scm.Event{{Kind: scm.EventBuild, PR: pr}}

	ingress, _, st := testIngress(t, client)

	rec := post(t, ingress.Handler(), "/webhook/github", []byte("{}"))
	if rec.Code != 202 {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	// A negative, so it is given time to be wrong in. The positive control below proves
	// this window is long enough to have seen a build.
	time.Sleep(500 * time.Millisecond)

	n, err := st.PendingCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("pending = %d, want 0: a repository with no project row was built", n)
	}
	if states := client.reportStates(); len(states) != 0 {
		t.Errorf("reports = %v, want none: nfpreview wrote to a pull request "+
			"on a repository the operator never added", states)
	}
}

// TestADeliveryWithAProjectStillBuilds is the positive control for the test above.
//
// Without it, that one passes just as well against a daemon that has stopped building
// anything at all, which is the failure this whole change could plausibly cause.
func TestADeliveryWithAProjectStillBuilds(t *testing.T) {
	client := &fakeClient{}
	pr := gatePR()
	client.events = []scm.Event{{Kind: scm.EventBuild, PR: pr}}

	ingress, _, st := testIngress(t, client)

	if err := st.SaveProject(context.Background(), store.Project{
		Platform: string(pr.Repo.Platform), Owner: pr.Repo.Owner, Repo: pr.Repo.Name,
	}); err != nil {
		t.Fatal(err)
	}

	rec := post(t, ingress.Handler(), "/webhook/github", []byte("{}"))
	if rec.Code != 202 {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := st.PendingCount(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending = %d, want 1: a repository with a project row was not built", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTheLocalPlatformNeedsNoProjectRow — the exemption, and the reason for it.
//
// A local repository is a directory the operator put under `local.repos_dir` themselves, so
// the choice a project row exists to record has already been made on the filesystem. There is
// also nobody who can install anything: the demo has no project rows at all, and requiring one
// would have made it build nothing while looking correctly wired.
func TestTheLocalPlatformNeedsNoProjectRow(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	repo := model.Repo{Platform: model.PlatformLocal, Owner: "demo", Name: "mydocs"}
	if !d.DeliveryAllowed(t.Context(), repo) {
		t.Error("a local repository with no project row was refused")
	}
}

// TestAFailedProjectLookupRefuses — fail closed, which is the opposite of how applyProject
// treats the same error and is right for the opposite reason.
//
// applyProject is choosing settings for a repository already known to be allowed, so a failed
// read costs a default. This is deciding whether it is allowed at all, and a failed query is
// not evidence of a project row. The delivery GitHub can replay is the cheaper mistake.
func TestAFailedProjectLookupRefuses(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})

	repo := gatePR().Repo
	if err := st.SaveProject(t.Context(), store.Project{
		Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name,
	}); err != nil {
		t.Fatal(err)
	}
	if !d.DeliveryAllowed(t.Context(), repo) {
		t.Fatal("the row is there and the delivery was refused; the rest of this test proves nothing")
	}

	// The lookup now fails for a reason that is not "no such project".
	st.Close()

	if d.DeliveryAllowed(t.Context(), repo) {
		t.Error("a repository was allowed on a failed lookup")
	}
}
