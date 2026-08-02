package daemon

import (
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// TestATerminalStateAlwaysReachesThePlatform.
//
// A build.timeout failure that reaches only the builds table and not the platform leaves the pull
// request comment reading "Building" indefinitely, since nothing else reports against that commit. The
// reviewer's only view of the build would say it is still running.
//
// Every terminal state is checked, not just failed, and the queued/building pair before it
// is included because the report path is stateful: staleReport tracks the furthest state per
// commit, and a terminal report arriving after them must not be mistaken for a late
// duplicate of one of them.
func TestATerminalStateAlwaysReachesThePlatform(t *testing.T) {
	for _, terminal := range []scm.State{scm.StateFailed, scm.StateReady, scm.StateSkipped} {
		t.Run(string(terminal), func(t *testing.T) {
			client := &fakeClient{}
			_, d, _ := testIngress(t, client)

			pr := model.PullRequest{
				Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
				Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
			}
			id := pr.PreviewID()

			for _, s := range []scm.State{scm.StateQueued, scm.StateBuilding, terminal} {
				d.report(t.Context(), scm.Report{
					PR: pr, PreviewID: id, State: s, Commit: pr.HeadSHA, UpdatedAt: time.Now(),
				})
			}

			// Close flushes the debounce window rather than sleeping through it, which is
			// what a shutdown does and is the one thing that must not lose a terminal
			// state.
			d.publisher.Close()

			states := client.reportStates()
			if len(states) == 0 {
				t.Fatal("nothing reached the platform at all")
			}
			if got := states[len(states)-1]; got != terminal {
				t.Errorf("the platform last heard %q, want %q — the comment would be left "+
					"describing a build that has finished", got, terminal)
			}
		})
	}
}

// TestACancelledBuildTellsThePullRequest — cancelling from the dashboard stops the work,
// and the comment is the only thing a reviewer sees. Left alone it reads "Building"
// indefinitely, because a cancelled build is precisely the case where nothing else is
// coming to correct it.
func TestACancelledBuildTellsThePullRequest(t *testing.T) {
	client := &fakeClient{}
	_, d, _ := testIngress(t, client)

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
	}
	id := pr.PreviewID()

	d.report(t.Context(), scm.Report{
		PR: pr, PreviewID: id, State: scm.StateBuilding, Commit: pr.HeadSHA, UpdatedAt: time.Now(),
	})
	d.mu.Lock()
	d.running[id] = &build{pr: pr, started: time.Now(), cancel: func() {}}
	d.mu.Unlock()

	if !d.CancelBuild(t.Context(), id) {
		t.Fatal("nothing was cancelled")
	}
	d.publisher.Close()

	states := client.reportStates()
	if len(states) == 0 || states[len(states)-1] != scm.StateFailed {
		t.Errorf("the platform last heard %v; a cancelled build must not leave the comment "+
			"saying Building", states)
	}
}
