package daemon

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// releasingExposer records which names were released, and stands in for zrok — the
// only exposer whose names are objects with a quota. The wrapped exposer does the
// publishing, so the shares behave normally.
type releasingExposer struct {
	expose.Exposer

	mu       sync.Mutex
	released []string
	fail     bool
}

func (r *releasingExposer) ReleaseName(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("names api unavailable")
	}
	r.released = append(r.released, name)
	return nil
}

func (r *releasingExposer) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.released...)
	sort.Strings(out)
	return out
}

// TestRebuildMustNotReleaseTheName is the assertion that guards the whole name
// lifecycle.
//
// A name outliving its share is the feature, not the leak: it is what keeps a
// reviewer's URL working across every rebuild and restart. Releasing it on the
// withdraw that a rebuild performs would rehost the preview at a new address on every
// push, silently, while the pull request comment kept advertising the old one — worse
// than the leak and much harder to notice, which is why this is a test and not a
// comment.
func TestRebuildMustNotReleaseTheName(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})
	ex := &releasingExposer{Exposer: d.exposer}
	d.exposer = ex
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
	}

	// The same build published twice is the rebuild path: the second publish
	// withdraws the first, which closes a Publication.
	for range 2 {
		if url := d.publishBuildShare(ctx, pr, "20260729-190307-85912e2", "add-guide",
			config.RepoConfig{}, http.NotFoundHandler()); url == "" {
			t.Fatal("the build share did not publish, so this proves nothing")
		}
	}

	if got := ex.names(); len(got) != 0 {
		t.Errorf("a rebuild released %v; every rebuilt preview would change address", got)
	}
}

// TestTeardownReleasesEveryName — teardown is the one moment the URL is never coming
// back, so it is the only caller that may release. Every name the preview took goes,
// including the ones with no live publication: a preview whose republish failed after
// a restart, or a build whose artifacts were pruned, still holds a name against the
// account's quota and is the case most likely to be leaked already.
func TestTeardownReleasesEveryName(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ex := &releasingExposer{Exposer: d.exposer}
	d.exposer = ex
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
	}
	id := pr.PreviewID()

	// Recorded but not live: nothing is in d.live or d.liveBuilds, which is the state
	// after a restart whose republish did not get there.
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "add-guide", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBuild(ctx, store.Build{
		PreviewID: id, BuildID: "20260729-190307-85912e2", PR: pr, State: "ready",
		Name: "add-guide-85912e2", URL: "https://add-guide-85912e2.example/",
	}); err != nil {
		t.Fatal(err)
	}
	// A failed build never published, so it has no name to release.
	if err := st.SaveBuild(ctx, store.Build{
		PreviewID: id, BuildID: "20260729-180000-0000000", PR: pr, State: "failed",
	}); err != nil {
		t.Fatal(err)
	}

	// Another preview's name, which must survive: teardown of one pull request is not
	// a licence to free another's URL.
	other := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 9, Branch: "other-branch",
	}
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: other.PreviewID(), PR: other, Name: "other-branch", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}

	// One live build share as well, so both sources are covered in one teardown.
	d.mu.Lock()
	d.liveBuilds[buildKey(id, "20260729-191500-deadbee")] =
		expose.NewPublication("https://add-guide-deadbee.example/", "add-guide-deadbee", nil)
	d.mu.Unlock()

	if err := d.teardown(ctx, pr, id); err != nil {
		t.Fatal(err)
	}

	want := []string{"add-guide", "add-guide-85912e2", "add-guide-deadbee"}
	got := ex.names()
	if len(got) != len(want) {
		t.Fatalf("released %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("released %v, want %v", got, want)
		}
	}
}

// TestTeardownSurvivesAReleaseFailure — the pull request is gone either way. An
// exposer that cannot release leaves one name against a quota, which is a warning;
// failing the teardown over it would strand the artifacts, the workspace, the cache
// and the comment, which is a much larger mess.
func TestTeardownSurvivesAReleaseFailure(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	d.exposer = &releasingExposer{Exposer: d.exposer, fail: true}
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide",
	}
	id := pr.PreviewID()
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "add-guide", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}

	if err := d.teardown(ctx, pr, id); err != nil {
		t.Fatalf("a failed name release failed the teardown: %v", err)
	}
	previews, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(previews) != 0 {
		t.Errorf("%d previews remain, so the teardown stopped early", len(previews))
	}
}
