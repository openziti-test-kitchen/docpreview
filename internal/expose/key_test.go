package expose

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
)

func TestSpecKey(t *testing.T) {
	pv := Spec{PreviewID: "19344c5ee369"}
	if got := pv.Key(); got != "19344c5ee369" {
		t.Errorf("a preview's own share keyed %q, want the bare preview id", got)
	}

	// The bare preview id for the branch share is load-bearing, not cosmetic: it is
	// the tag on shares that already exist remotely. Change it and the first Reap
	// after an upgrade treats every restored preview as an orphan and deletes it.
	build := Spec{PreviewID: "19344c5ee369", BuildID: "20260729-190307-85912e2"}
	if got := build.Key(); got != "19344c5ee369/20260729-190307-85912e2" {
		t.Errorf("key = %q, want preview/build", got)
	}
	if build.Key() == pv.Key() {
		t.Error("a build share and its branch share share a key, so one would withdraw the other")
	}
}

// TestRebuildingOneCommitTakesItsOwnBuildShare covers a collision that would leave
// "Open build" greyed out for a build that just succeeded.
//
// A build share's name embeds the commit, so rebuilding the same commit asks for the
// same name under a new build id — and a check that refused every key it did not
// recognise would refuse the preview its own name. The build would succeed, the
// share would not be created, and the dashboard would have no URL to offer for it.
func TestRebuildingOneCommitTakesItsOwnBuildShare(t *testing.T) {
	l := NewLocal(slog.New(slog.DiscardHandler), "")
	t.Cleanup(func() { l.Close() })
	l.SetOrigin("http://127.0.0.1:8471")

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide",
	}
	spec := func(buildID string) Spec {
		return Spec{
			PreviewID: pr.PreviewID(), BuildID: buildID,
			Name: "add-guide-85912e2", BaseURL: "/", PR: pr,
		}
	}

	if _, err := l.Publish(context.Background(), spec("20260729-190307-85912e2"), http.NotFoundHandler()); err != nil {
		t.Fatalf("publishing the first build of a commit: %v", err)
	}

	// Same commit, rebuilt: same name, new build id.
	again, err := l.Publish(context.Background(), spec("20260730-182502-85912e2"), http.NotFoundHandler())
	if err != nil {
		t.Fatalf("rebuilding one commit must reuse its own build share: %v", err)
	}
	if again.URL == "" {
		t.Error("the rebuilt commit got no URL, which is what leaves Open build greyed out")
	}

	// The older publication is gone rather than left holding a name nothing serves.
	if n := l.count(); n != 1 {
		t.Errorf("the exposer holds %d publications of one commit, want 1", n)
	}
}

// TestOneNameForTwoPreviewsIsStillRefused is the other half: the takeover above must
// not be reachable across previews.
//
// Two pull requests rendering to one name is a name_template that cannot separate
// them. Letting the second take the name would point a reviewer's URL at another
// pull request's site, which is worse than a failed publish.
func TestOneNameForTwoPreviewsIsStillRefused(t *testing.T) {
	l := NewLocal(slog.New(slog.DiscardHandler), "")
	t.Cleanup(func() { l.Close() })
	l.SetOrigin("http://127.0.0.1:8471")

	mine := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide",
	}
	theirs := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "handbook"},
		Number: 3, Branch: "add-guide",
	}

	if _, err := l.Publish(context.Background(), Spec{
		PreviewID: mine.PreviewID(), Name: "add-guide", BaseURL: "/", PR: mine,
	}, http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}

	_, err := l.Publish(context.Background(), Spec{
		PreviewID: theirs.PreviewID(), Name: "add-guide", BaseURL: "/", PR: theirs,
	}, http.NotFoundHandler())
	if err == nil {
		t.Fatal("a second preview took another's name; a reviewer's URL now serves the wrong site")
	}
	if !strings.Contains(err.Error(), "name_template") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
}

// TestTwoPublicationsPerPreview is the property the key exists for.
//
// Publish withdraws whatever holds the key before taking it, so if the key were the
// preview id alone, publishing a build share would tear down the branch share it is
// meant to sit beside — one live share per preview, by construction. The local
// exposer stands in for all four here: they share this structure, and it needs no
// network.
func TestTwoPublicationsPerPreview(t *testing.T) {
	l := NewLocal(slog.New(slog.DiscardHandler), "")
	t.Cleanup(func() { l.Close() })
	l.SetOrigin("http://127.0.0.1:8471")

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide",
	}
	h := http.NotFoundHandler()

	branch, err := l.Publish(context.Background(), Spec{
		PreviewID: pr.PreviewID(), Name: "add-guide", BaseURL: "/", PR: pr,
	}, h)
	if err != nil {
		t.Fatalf("publishing the branch share: %v", err)
	}

	first, err := l.Publish(context.Background(), Spec{
		PreviewID: pr.PreviewID(), BuildID: "20260729-190307-85912e2",
		Name: "add-guide-85912e2", BaseURL: "/", PR: pr,
	}, h)
	if err != nil {
		t.Fatalf("publishing a build share: %v", err)
	}

	second, err := l.Publish(context.Background(), Spec{
		PreviewID: pr.PreviewID(), BuildID: "20260729-190702-0faa113",
		Name: "add-guide-0faa113", BaseURL: "/", PR: pr,
	}, h)
	if err != nil {
		t.Fatalf("publishing a second build share: %v", err)
	}

	// All three live at once, at three distinct URLs. Keyed by preview id alone, the
	// second Publish would delete the first and the third would delete the second,
	// leaving one.
	urls := map[string]bool{branch.URL: true, first.URL: true, second.URL: true}
	if len(urls) != 3 {
		t.Errorf("three publications produced %d distinct URLs: %v", len(urls), urls)
	}
	if n := l.count(); n != 3 {
		t.Errorf("the exposer holds %d publications, want 3", n)
	}

	// Closing a build share must leave the branch share alone. The daemon closes
	// the old publication *after* publishing the new one, so a close that matched
	// by preview rather than by key would tear down its own replacement.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if n := l.count(); n != 2 {
		t.Errorf("after closing one build share the exposer holds %d, want 2", n)
	}
}
