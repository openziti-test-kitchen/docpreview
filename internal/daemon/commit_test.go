package daemon

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// The commit phase's contract: a published share and a database row that
// records it either both exist or neither does. A live URL with no row is the
// worst outcome — it works, the comment says the build failed, /status is
// empty, and the next restart reaps it as an unknown orphan.

func TestUnpublishWithdrawsTheShareAndTheRecord(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: "abc",
	}
	id := pr.PreviewID()

	pub, err := d.exposer.Publish(ctx,
		expose.Spec{PreviewID: id, Name: "feature-x", BaseURL: "/", PR: pr},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "live")
		}))
	if err != nil {
		t.Fatal(err)
	}

	d.mu.Lock()
	d.live[id] = pub
	d.mu.Unlock()

	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "feature-x", URL: pub.URL,
		State: scm.StateReady, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Confirm the preconditions, or the test proves nothing.
	if resp, err := http.Get(pub.URL); err != nil {
		t.Fatalf("the preview was not serving before unpublish: %v", err)
	} else {
		resp.Body.Close()
	}

	d.unpublish(id)

	// The listener is the daemon's own and stays up, so a withdrawn preview
	// answers 404 rather than refusing the connection.
	resp, err := http.Get(pub.URL)
	if err != nil {
		t.Fatalf("GET after unpublish: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the share still answers %d after unpublish", resp.StatusCode)
	}

	d.mu.Lock()
	_, stillLive := d.live[id]
	d.mu.Unlock()
	if stillLive {
		t.Error("unpublish left the publication in the live map")
	}

	all, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("unpublish left %d preview rows behind", len(all))
	}
}

func TestUnpublishIsSafeWithNothingPublished(t *testing.T) {
	// Reached whenever SavePreview fails, including paths where the publish
	// never landed in the map.
	_, d, _ := testIngress(t, &fakeClient{})
	d.unpublish("no-such-preview")
}

func TestFailedRecordLeavesNothingLive(t *testing.T) {
	// The end-to-end shape of the invariant: force the durable write to fail
	// and assert the share does not outlive it.
	_, d, st := testIngress(t, &fakeClient{})
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  7,
		Branch:  "feature/y",
		HeadSHA: "def",
	}
	id := pr.PreviewID()

	pub, err := d.exposer.Publish(ctx,
		expose.Spec{PreviewID: id, Name: "feature-y", BaseURL: "/", PR: pr},
		http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.live[id] = pub
	d.mu.Unlock()

	// Closing the store makes every subsequent write fail, which is the
	// observable stand-in for a full disk or a locked database.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	saveErr := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "feature-y", URL: pub.URL,
		State: scm.StateReady, UpdatedAt: time.Now(),
	})
	if saveErr == nil {
		t.Fatal("SavePreview succeeded against a closed store; the test cannot prove anything")
	}

	// This is what runPipeline now does on that error.
	d.unpublish(id)

	resp, err := http.Get(pub.URL)
	if err != nil {
		t.Fatalf("GET after unpublish: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a preview that could not be recorded still answers %d", resp.StatusCode)
	}

	d.mu.Lock()
	_, stillLive := d.live[id]
	d.mu.Unlock()
	if stillLive {
		t.Error("a preview that could not be recorded is still in the live map")
	}
}
