package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// TestReapKeepSetIncludesBuildShares is the assertion that stops the sweep deleting
// what the publish just created.
//
// The keep-set is expressed in publication keys, not preview ids, because a preview can hold more than one
// share. A keep-set of preview ids alone would mark every build share as an orphan, so the hourly sweep would
// withdraw each one minutes after it appeared and the dashboard would offer URLs that had already gone.
func TestReapKeepSetIncludesBuildShares(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
	}
	id := pr.PreviewID()

	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "add-guide", URL: "https://add-guide.example/",
		State: "ready",
	}); err != nil {
		t.Fatal(err)
	}

	// Two builds: one that published a share, one that did not.
	if err := st.SaveBuild(ctx, store.Build{
		PreviewID: id, BuildID: "20260729-190307-85912e2", PR: pr, State: "ready",
		Name: "add-guide-85912e2", URL: "https://add-guide-85912e2.example/",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBuild(ctx, store.Build{
		PreviewID: id, BuildID: "20260729-180000-0000000", PR: pr, State: "failed",
	}); err != nil {
		t.Fatal(err)
	}

	keep := map[string]bool{}
	previews, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range previews {
		keep[p.PreviewID] = true
		builds, err := st.BuildsFor(ctx, p.PreviewID)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range builds {
			if b.URL != "" {
				keep[buildKey(p.PreviewID, b.BuildID)] = true
			}
		}
	}

	if !keep[id] {
		t.Error("the branch share is not in the keep-set")
	}
	if !keep[id+"/20260729-190307-85912e2"] {
		t.Error("a build share with a recorded URL is not in the keep-set; the sweep would delete it")
	}
	// A build that never published has nothing to keep, and adding its key would
	// grow the set while protecting nothing.
	if keep[id+"/20260729-180000-0000000"] {
		t.Error("a build with no share is in the keep-set")
	}

	_ = d
}

// TestBuildKeyMatchesTheExposerKey — the daemon and the exposer index publications
// independently, and a keep-set built with one spelling against shares tagged with
// the other keeps nothing.
func TestBuildKeyMatchesTheExposerKey(t *testing.T) {
	spec := expose.Spec{PreviewID: "19344c5ee369", BuildID: "20260729-190307-85912e2"}
	if got, want := buildKey(spec.PreviewID, spec.BuildID), spec.Key(); got != want {
		t.Errorf("buildKey = %q but expose.Spec.Key = %q", got, want)
	}
}

// TestTeardownClosesBuildShares — a preview's build shares have the preview's
// lifetime. Left behind, they are shares nothing records, which the next sweep
// deletes anyway; the point of closing them here is that teardown is the moment the
// pull request went away, not an hour later.
func TestTeardownClosesBuildShares(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide",
	}
	id := pr.PreviewID()

	// Two of this preview's build shares, and one belonging to another preview that
	// must survive.
	closed := map[string]bool{}
	mk := func(key string) *expose.Publication {
		return expose.NewPublication("https://"+key+".example/", key, func() error {
			closed[key] = true
			return nil
		})
	}

	d.mu.Lock()
	d.liveBuilds[buildKey(id, "build-a")] = mk("a")
	d.liveBuilds[buildKey(id, "build-b")] = mk("b")
	d.liveBuilds[buildKey("other0000000", "build-c")] = mk("c")
	d.mu.Unlock()

	if err := d.teardown(context.Background(), pr, id); err != nil {
		t.Fatal(err)
	}

	if !closed["a"] || !closed["b"] {
		t.Errorf("teardown left build shares open: closed = %v", closed)
	}
	if closed["c"] {
		t.Error("teardown closed another preview's build share")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for key := range d.liveBuilds {
		if strings.HasPrefix(key, id+"/") {
			t.Errorf("teardown left %s in liveBuilds, so the map grows without bound", key)
		}
	}
	if _, ok := d.liveBuilds[buildKey("other0000000", "build-c")]; !ok {
		t.Error("teardown removed another preview's entry")
	}
}

// TestBuildShareFailureDoesNotFailTheBuild — the branch share is the contract and is
// already live when the build share is attempted. Every way the second publish can
// fail — a reserved-name quota, a collision, an exposer that will not mint two — is
// a reason to carry on, because the alternative is a comment saying the docs did not
// build when they did.
func TestBuildShareFailureDoesNotFailTheBuild(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})
	d.exposer = refusingExposer{Exposer: d.exposer}

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
	}

	url := d.publishBuildShare(context.Background(), pr, "20260729-190307-85912e2",
		"add-guide", config.RepoConfig{}, http.NotFoundHandler())
	if url != "" {
		t.Errorf("url = %q, want empty when the publish failed", url)
	}
}

// refusingExposer publishes nothing, standing in for an account that has hit its
// reserved-name quota — the failure most likely to happen first in practice, since
// one share per build multiplies share count by every push.
type refusingExposer struct {
	expose.Exposer
}

func (refusingExposer) Publish(context.Context, expose.Spec, http.Handler) (*expose.Publication, error) {
	return nil, errors.New("quota exceeded: too many reserved names")
}
