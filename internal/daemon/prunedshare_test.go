package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// TestPruningRetiresTheBuildShare.
//
// Retention deleted a build's artifacts and nothing else, which left a live share
// serving a directory that no longer existed, a URL the dashboard still offered, and a
// reserved name against the exposer account's quota.
//
// The leak protected itself: Reap's keep-set is built from builds with a recorded url,
// and the row still had one — so the hourly sweep treated the dead share as something to
// preserve. That is why this is a test rather than a comment.
func TestPruningRetiresTheBuildShare(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ex := &releasingExposer{Exposer: d.exposer}
	d.exposer = ex
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide",
	}
	id := pr.PreviewID()

	// Two builds' artifacts on disk, and keep_builds of 1, so the older one is pruned.
	const kept, pruned = "20260729-191500-bbbbbbb", "20260729-190000-aaaaaaa"
	for _, b := range []string{kept, pruned} {
		if err := os.MkdirAll(filepath.Join(d.cfg.ArtifactsDir(), id, b), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveBuild(ctx, store.Build{
			PreviewID: id, BuildID: b, PR: pr, State: "ready",
			Name: "add-guide-" + b[len(b)-7:], URL: "https://add-guide.example/",
		}); err != nil {
			t.Fatal(err)
		}
	}

	closed := false
	d.mu.Lock()
	d.liveBuilds[buildKey(id, pruned)] = expose.NewPublication(
		"https://add-guide-aaaaaaa.example/", "add-guide-aaaaaaa", func() error {
			closed = true
			return nil
		})
	d.cfg.Preview.KeepBuilds = 1
	d.mu.Unlock()

	d.pruneArtifacts(id, kept)

	if !closed {
		t.Error("the pruned build's share is still live, serving artifacts that are gone")
	}
	if got := ex.names(); len(got) != 1 || got[0] != "add-guide-aaaaaaa" {
		t.Errorf("released %v, want the pruned build's name; it stays against the quota", got)
	}

	// The row must lose its URL, or Reap's keep-set goes on protecting the share that
	// was just withdrawn — and the dashboard goes on offering a dead link.
	builds, err := st.BuildsFor(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range builds {
		switch b.BuildID {
		case pruned:
			if b.URL != "" || b.Name != "" {
				t.Errorf("the pruned build still records a share: name=%q url=%q", b.Name, b.URL)
			}
		case kept:
			if b.URL == "" {
				t.Error("the kept build lost its share")
			}
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.liveBuilds[buildKey(id, pruned)]; ok {
		t.Error("the pruned build is still in liveBuilds, so the map grows without bound")
	}
	if _, ok := d.liveBuilds[buildKey(id, kept)]; ok {
		// Never added, but the point is that pruning must not reach past its victim.
		t.Error("pruning touched the build it was told to keep")
	}
}
