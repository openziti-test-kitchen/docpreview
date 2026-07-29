package expose

import (
	"context"
	"log/slog"
	"net/http"
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

// TestTwoPublicationsPerPreview is the property the key exists for.
//
// Publish withdraws whatever holds the key before taking it, so while the key was
// the preview id, publishing a build share tore down the branch share it was meant
// to sit beside — one live share per preview, by construction. The local exposer
// stands in for all four here: they share this structure, and it needs no network.
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

	// All three live at once, at three distinct URLs. Before the key change the
	// second Publish deleted the first and the third deleted the second, leaving
	// one.
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
