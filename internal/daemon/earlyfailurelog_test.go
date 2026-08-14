package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// A build that fails before it starts still leaves a log saying why.
//
// The build log is opened once the workspace, the repository config and the detect decision are
// all in hand — so everything before that point (a clone that cannot authenticate, an unparsable
// `.docpreview.yml`, a detect script at a path that does not exist) failed with no log at all. The
// pane then said "No build log was kept for <commit>", which is the sentence a log *pruned by
// retention* produces. Somebody who clicks Rebuild and reads that concludes nothing ran, and goes
// looking for a broken button instead of the misconfiguration they just saved.
//
// Live on 2026-08-13: a project's detect script was set to `../scripts/vercel-ignore.sh
// unified-doc/`, two rebuilds failed on `lstat …/scripts: no such file or directory`, and the
// dashboard showed nothing at all.
func TestAFailureBeforeTheBuildStartsStillWritesALog(t *testing.T) {
	client := &fakeClient{cloneURLErr: errors.New("no installation token for acme/docs")}
	_, d, _ := testIngress(t, client)

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: "abcdef1234567890",
	}

	_, _, err := d.runPipeline(context.Background(), &build{}, client, pr, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("the pipeline succeeded with a clone URL that cannot be resolved")
	}

	metas, listErr := d.Logs().List(pr.PreviewID())
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(metas) != 1 {
		t.Fatalf("%d build logs, want 1 — the failure left nothing for the pane to show", len(metas))
	}

	f, _, openErr := d.Logs().Open(pr.PreviewID(), metas[0].BuildID)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer f.Close()

	body, readErr := io.ReadAll(f)
	if readErr != nil {
		t.Fatal(readErr)
	}
	text := string(body)

	if !strings.Contains(text, "no installation token") {
		t.Errorf("the log does not carry the reason:\n%s", text)
	}
	// "did not start", not "did not publish": the build never produced anything to publish, and
	// the wrong word sends somebody looking at the exposer for a fault in the clone.
	if !strings.Contains(text, "did not start") {
		t.Errorf("the log does not say the build never started:\n%s", text)
	}
	if strings.Contains(text, "did not publish") {
		t.Errorf("a build that never ran is reported as a failed publish:\n%s", text)
	}

	// And a row beside it, or the picker lists the log with no state — readable, but silent
	// about the one thing it was opened to say.
	builds, buildsErr := d.Builds(context.Background(), pr.PreviewID())
	if buildsErr != nil {
		t.Fatal(buildsErr)
	}
	if len(builds) != 1 {
		t.Fatalf("%d build rows, want 1", len(builds))
	}
	if builds[0].State != string(scm.StateFailed) {
		t.Errorf("the build row reads %q, want failed", builds[0].State)
	}
	if !strings.Contains(builds[0].Reason, "no installation token") {
		t.Errorf("the build row does not carry the reason: %q", builds[0].Reason)
	}
	if builds[0].BuildID != metas[0].BuildID {
		t.Errorf("the row is for build %q and the log is %q, so the picker cannot pair them",
			builds[0].BuildID, metas[0].BuildID)
	}
}

// A superseded build writes nothing. It is not a failure — a newer push is already building, with
// its own log — and a "this build did not start" line under the previous commit reads as a fault.
func TestASupersededBuildWritesNoLateLog(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		HeadSHA: "abcdef1234567890",
	}

	client := &fakeClient{cloneURLErr: errSuperseded}
	if _, _, err := d.runPipeline(context.Background(), &build{}, client, pr,
		slog.New(slog.DiscardHandler)); !errors.Is(err, errSuperseded) {
		t.Fatalf("runPipeline returned %v, want errSuperseded", err)
	}

	metas, err := d.Logs().List(pr.PreviewID())
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Errorf("a superseded build wrote %d log(s), want none", len(metas))
	}
}
