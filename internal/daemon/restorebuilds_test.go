package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// TestRestoreBuildSharesSurvivesARestart covers a bug that shipped and was found by
// clicking a link.
//
// Startup reaps with an empty keep-set — nothing this process owns can have survived
// the last one — and then republished previews only. So every per-build share died on
// restart while `builds.url` went on advertising it, and the URL 404'd. The report
// was "that should still be there", and it should have been.
func TestRestoreBuildSharesSurvivesARestart(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 4, Branch: "round-4", HeadSHA: "7cf20bcabcdef",
	}
	id := pr.PreviewID()

	// A build whose artifacts are still on disk, and one whose are not — which is
	// what keep_builds leaves behind.
	kept := "20260729-210224-a3aa98e"
	pruned := "20260729-205406-7cf20bc"

	dir := filepath.Join(d.cfg.ArtifactsDir(), id, kept)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>ok</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := store.Preview{
		PreviewID: id, PR: pr, Name: "round-4", URL: "https://round-4.example/",
		BaseURL: "/", ArtifactDir: dir, State: scm.StateReady,
	}
	if err := st.SavePreview(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, b := range []struct{ id, name, url string }{
		{kept, "round-4-a3aa98e", "https://round-4-a3aa98e.example/"},
		{pruned, "round-4-7cf20bc", "https://round-4-7cf20bc.example/"},
	} {
		if err := st.SaveBuild(ctx, store.Build{
			PreviewID: id, BuildID: b.id, PR: pr, State: "ready", Name: b.name, URL: b.url,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if n := d.restoreBuildShares(ctx, p); n != 1 {
		t.Errorf("restored %d build shares, want 1 — only one has artifacts", n)
	}

	d.mu.Lock()
	_, keptLive := d.liveBuilds[buildKey(id, kept)]
	_, prunedLive := d.liveBuilds[buildKey(id, pruned)]
	d.mu.Unlock()

	if !keptLive {
		t.Error("the build whose artifacts survived was not republished, so its URL 404s")
	}
	if prunedLive {
		t.Error("a build with no artifacts was published; it would serve nothing")
	}

	// And the pruned build must stop advertising a URL, or the dashboard keeps
	// offering a link to something that is not there — the same failure, slower.
	rows, err := st.BuildsFor(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		switch b.BuildID {
		case kept:
			if b.URL == "" {
				t.Error("the restored build lost its URL")
			}
		case pruned:
			if b.URL != "" {
				t.Errorf("a pruned build still advertises %q", b.URL)
			}
			// The row itself stays: the history happened.
			if b.State != "ready" {
				t.Errorf("clearing the share also changed the state to %q", b.State)
			}
		}
	}
}
