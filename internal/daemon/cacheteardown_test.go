package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
)

// TestTeardownRemovesTheBuildCache is the reason the cache is keyed on the preview
// rather than on the repository.
//
// A cache that outlives the pull request that filled it has no moment at which
// anything knows it is safe to delete: the branch is gone, the preview row is gone,
// and the directory sits there until somebody notices the disk. Keyed per preview,
// it goes out with the workspace and the artifacts, and this is the assertion that
// it actually does.
func TestTeardownRemovesTheBuildCache(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 42, Branch: "feature/x",
	}
	id := pr.PreviewID()

	cache := d.cfg.PreviewCacheDir(id)
	if cache == "" {
		t.Fatal("no cache directory is configured, so this test proves nothing")
	}
	if err := os.MkdirAll(filepath.Join(cache, "npm", "_cacache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "npm", "_cacache", "entry"),
		[]byte("tarball"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A sibling preview's cache, which must survive. Teardown of one pull request
	// leaving every other one cold would be worse than not caching.
	other := d.cfg.PreviewCacheDir("7ac8b8042f54")
	if err := os.MkdirAll(filepath.Join(other, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := d.teardown(context.Background(), pr, id); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("the preview's build cache survived teardown: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("teardown removed another preview's cache: %v", err)
	}
}
