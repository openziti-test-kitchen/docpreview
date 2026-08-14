package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/pipeline"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// Clearing a preview's cache removes the volumes its builds actually mount.
//
// The cache is keyed on the repository, and the control is addressed by preview — so the handler
// has to resolve one to the other. Removing volumes named after the preview id instead is the
// failure this guards: `docker volume rm` succeeds on names that do not exist, so the button
// reports success and clears nothing, and the corrupt entry that prompted it is still there on the
// next build.
func TestClearingACacheRemovesTheRepositorysVolumes(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "aaaaaaaaaaaa",
	}
	if err := st.SavePreview(context.Background(), store.Preview{
		PreviewID: pr.PreviewID(), PR: pr, Name: "acme-docs-add-guide",
		URL: "https://example.invalid/", State: scm.StateReady, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}

	var removed []string
	h := NewProjectsAdmin(st, cfg, slog.New(slog.DiscardHandler)).
		WithVolumeOps(
			func(context.Context) ([]string, error) { return nil, nil },
			func(_ context.Context, scope string) error {
				removed = append(removed, pipeline.CacheVolumesFor(scope)...)
				return nil
			},
		).Handler()

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/cache/"+pr.PreviewID(), nil)
	r.RemoteAddr = "127.0.0.1:54321"
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	want := pipeline.CacheVolumesFor(pipeline.CacheScope(pr))
	if len(removed) != len(want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	for i := range want {
		if removed[i] != want[i] {
			t.Errorf("removed %q, want %q", removed[i], want[i])
		}
	}

	// And not the preview's own id, which is what the volumes used to be named for.
	for _, name := range removed {
		for _, stale := range pipeline.CacheVolumesFor(pr.PreviewID()) {
			if name == stale {
				t.Errorf("the clear targeted %q, a name no build mounts any more", name)
			}
		}
	}
}

// Closing a pull request must not clear the cache every other pull request on that repository is
// using. Teardown removes the preview's workspace, artifacts and logs; the cache outlives it.
func TestTearingDownAPreviewLeavesTheRepositoryCacheAlone(t *testing.T) {
	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7,
	}
	scope := pipeline.CacheScope(pr)

	for _, name := range pipeline.CacheVolumesFor(pr.PreviewID()) {
		for _, live := range pipeline.CacheVolumesFor(scope) {
			if name == live {
				t.Fatalf("a preview-keyed volume name collides with the repository's: %s", name)
			}
		}
	}
}
