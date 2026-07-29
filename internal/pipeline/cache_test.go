package pipeline

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
)

func testPR(owner, repo string) model.PullRequest {
	return model.PullRequest{Repo: model.Repo{
		Platform: model.PlatformGitHub, Owner: owner, Name: repo,
	}}
}

// TestCacheMountsPointEachManagerAtItsOwnDirectory is the assertion that keeps
// builds fast. Without these mounts every build re-downloads its whole dependency
// tree, because the workspace they would otherwise cache into is created per commit
// and pruned with its siblings.
func TestCacheMountsPointEachManagerAtItsOwnDirectory(t *testing.T) {
	root := t.TempDir()
	b := &Builder{
		defaults: config.BuildDefaults{CacheDir: root},
		log:      slog.New(slog.DiscardHandler),
	}

	args, err := b.cacheMounts(testPR("openziti-test-kitchen", "docpreview"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"npm_config_cache=/cache/npm",
		"YARN_CACHE_FOLDER=/cache/yarn",
		"npm_config_store_dir=/cache/pnpm",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no environment pointing a manager at its cache: want %s in\n%s", want, joined)
		}
	}

	// Created on the host first. A bind mount of a missing path creates it
	// root-owned, which on a Linux host leaves a cache the operator cannot clear.
	repoRoot := filepath.Join(root, "github-openziti-test-kitchen-docpreview")
	for _, m := range []string{"npm", "yarn", "pnpm"} {
		if _, err := os.Stat(filepath.Join(repoRoot, m)); err != nil {
			t.Errorf("the %s cache directory was not created on the host: %v", m, err)
		}
	}

	// Windows spellings must not survive into a mount argument — see hostMountPath.
	for _, a := range args {
		if strings.HasPrefix(a, "type=bind") && strings.ContainsAny(strings.SplitN(a, ",target=", 2)[0], `\`) {
			t.Errorf("a mount source is still in Windows form: %s", a)
		}
	}
}

// TestCacheMountsAreAbsentWithoutACacheDir — the docker driver has to work with no
// cache configured, since that is what every existing config says.
func TestCacheMountsAreAbsentWithoutACacheDir(t *testing.T) {
	b := &Builder{log: slog.New(slog.DiscardHandler)}
	args, err := b.cacheMounts(testPR("owner", "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none when no cache dir is set", args)
	}
}

// TestCachesAreNotSharedBetweenRepositories is the property the per-repository
// layout exists for: one project's corrupt entry must not be in the path of
// another's build, and clearing one must leave the rest warm.
func TestCachesAreNotSharedBetweenRepositories(t *testing.T) {
	root := t.TempDir()
	b := &Builder{
		defaults: config.BuildDefaults{CacheDir: root},
		log:      slog.New(slog.DiscardHandler),
	}

	first, err := b.cacheMounts(testPR("acme", "docs"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.cacheMounts(testPR("other", "docs"))
	if err != nil {
		t.Fatal(err)
	}

	// Same repository *name*, different owner. Keying on the name alone would put
	// these in one directory, which is the mistake worth pinning.
	if sourceOf(t, first) == sourceOf(t, second) {
		t.Errorf("two repositories share a cache directory: %s", sourceOf(t, first))
	}
}

// sourceOf returns the first bind source in a docker argument list.
func sourceOf(t *testing.T, args []string) string {
	t.Helper()
	for _, a := range args {
		if strings.HasPrefix(a, "type=bind,source=") {
			return strings.SplitN(strings.TrimPrefix(a, "type=bind,source="), ",", 2)[0]
		}
	}
	t.Fatalf("no bind mount in %v", args)
	return ""
}

// TestCacheKeyCannotEscapeTheCacheRoot — owner and repository arrive from a
// webhook. An owner of ".." would place a cache outside the root, and the
// clear-cache button would then delete something else.
func TestCacheKeyCannotEscapeTheCacheRoot(t *testing.T) {
	for _, c := range []struct{ owner, repo string }{
		{"..", "docs"},
		{"../..", "docs"},
		{"acme", "../../../etc"},
		{"a/b", "c\\d"},
		{"", ""},
	} {
		key := CacheKey(model.PlatformGitHub, c.owner, c.repo)
		if strings.ContainsAny(key, `/\`) || strings.Contains(key, "..") {
			t.Errorf("CacheKey(%q, %q) = %q, which is not a single safe path component",
				c.owner, c.repo, key)
		}
	}
}
