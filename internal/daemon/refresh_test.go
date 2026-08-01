package daemon

import (
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// A push must refresh a branch preview that exists and must not create one that does not.
//
// The push handlers fire for every default-branch push on every repository the installation can
// see. Treating each as "build this" would publish a permanent `main` preview for repositories
// nobody asked about — the App is installed for the sake of pull requests, and a branch preview is
// a URL, a name against the exposer's quota, and a row that never expires.
//
// Creating one stays where it was: adding a project, the startup backfill, and the button on the
// project card. All three are somebody asking for it.
func TestARefreshOnlyRebuildsWhatAlreadyExists(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})
	ctx := t.Context()

	branch := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  0,
		Branch:  "main",
		HeadSHA: "abc123",
	}

	t.Run("no preview yet", func(t *testing.T) {
		if err := d.handleBuild(ctx, scm.Event{
			Kind: scm.EventBuild, PR: branch, Refresh: true,
		}); err != nil {
			t.Fatalf("handleBuild: %v", err)
		}
		// Nothing queued, and — the part that matters — no preview invented.
		jobs, err := d.store.ListPreviews(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 0 {
			t.Errorf("a push created %d preview(s) for a repository nobody asked about", len(jobs))
		}
	})

	t.Run("a preview exists", func(t *testing.T) {
		if err := d.store.SavePreview(ctx, store.Preview{
			PreviewID: branch.PreviewID(),
			PR:        branch,
			Name:      "docs-main",
			State:     "ready",
		}); err != nil {
			t.Fatal(err)
		}

		if err := d.handleBuild(ctx, scm.Event{
			Kind: scm.EventBuild, PR: branch, Refresh: true,
		}); err != nil {
			t.Fatalf("handleBuild: %v", err)
		}

		// Queued, which is what a refresh is for: the branch moved, so what the preview is
		// showing is now out of date.
		n, err := d.store.PendingCount(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Error("a push to a branch with a preview queued nothing, so the preview stays stale")
		}
	})
}

// A branch preview whose last build failed is retried at the next startup.
//
// The backfill skipped any project that had a branch preview *row*, and a failed build leaves one.
// So the permanent preview of `main` stayed broken until somebody noticed and pressed Rebuild — on
// the one preview nobody looks at, because the point of it is that it sits there working.
//
// The backfill itself is run, with a fake that resolves a default branch, and the queue is what is
// asserted — the decision matters only through what it causes.
func TestAFailedBranchPreviewIsRetriedAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     scm.State
		wantQueue bool
	}{
		// The bug: a row exists, so the old test skipped it and `main` stayed broken.
		{"failed", scm.StateFailed, true},
		// And the rule it must not break: a working preview is left alone. Rebuilding every
		// branch preview at every startup would mean a build per project per restart, on
		// previews that are by design never out of date for long.
		{"ready", scm.StateReady, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, d, st := testIngress(t, &fakeClient{
				defaultBranch: "master",
				branchTip:     "cafe1234",
			})
			ctx := t.Context()

			repo := model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}
			if err := st.SaveProject(ctx, store.Project{
				Platform: string(repo.Platform), Owner: repo.Owner, Repo: repo.Name,
				Enabled: true,
			}); err != nil {
				t.Fatal(err)
			}

			branch := model.PullRequest{Repo: repo, Number: 0, Branch: "master", HeadSHA: "old"}
			if err := st.SavePreview(ctx, store.Preview{
				PreviewID: branch.PreviewID(),
				PR:        branch,
				Name:      "docs-master",
				State:     tc.state,
			}); err != nil {
				t.Fatal(err)
			}

			d.backfillBranchPreviews(ctx)

			n, err := st.PendingCount(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantQueue && n == 0 {
				t.Error("a failed branch preview was not retried, so main stays broken until " +
					"somebody presses Rebuild on the one preview nobody looks at")
			}
			if !tc.wantQueue && n != 0 {
				t.Errorf("a working branch preview was rebuilt anyway (%d queued), which is a "+
					"build per project on every restart", n)
			}
		})
	}
}

// A build that is not a refresh still creates. Asserted so the guard cannot quietly grow to cover
// the paths that are supposed to create previews — adding a project, the backfill, and Build now.
func TestAnOrdinaryBuildStillCreatesAPreview(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})
	ctx := t.Context()

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  7,
		Branch:  "feature/x",
		HeadSHA: "abc123",
	}
	if err := d.handleBuild(ctx, scm.Event{Kind: scm.EventBuild, PR: pr}); err != nil {
		t.Fatalf("handleBuild: %v", err)
	}

	n, err := d.store.PendingCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("an ordinary build queued nothing")
	}
}
