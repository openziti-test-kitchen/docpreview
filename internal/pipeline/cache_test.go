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

func testPR(owner, repo string, number int) model.PullRequest {
	return model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: owner, Name: repo},
		Number: number,
	}
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

	pr := testPR("openziti-test-kitchen", "docpreview", 2)
	args, err := b.cacheMounts(pr)
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
	previewRoot := filepath.Join(root, pr.PreviewID())
	for _, m := range []string{"npm", "yarn", "pnpm"} {
		if _, err := os.Stat(filepath.Join(previewRoot, m)); err != nil {
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
	args, err := b.cacheMounts(testPR("owner", "repo", 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none when no cache dir is set", args)
	}
}

// TestCachesAreNotSharedBetweenPullRequests is the property the per-preview layout
// exists for: one pull request's corrupt entry must not be in the path of another's
// build, and its cache must be deletable with it.
func TestCachesAreNotSharedBetweenPullRequests(t *testing.T) {
	root := t.TempDir()
	b := &Builder{
		defaults: config.BuildDefaults{CacheDir: root},
		log:      slog.New(slog.DiscardHandler),
	}

	// Two pull requests on the same repository, and one on another repository whose
	// number collides — keying on the number alone would merge those two.
	first, err := b.cacheMounts(testPR("acme", "docs", 2))
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.cacheMounts(testPR("acme", "docs", 3))
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.cacheMounts(testPR("other", "docs", 2))
	if err != nil {
		t.Fatal(err)
	}

	dirs := map[string]string{
		"acme/docs#2":  sourceOf(t, first),
		"acme/docs#3":  sourceOf(t, second),
		"other/docs#2": sourceOf(t, other),
	}
	seen := map[string]string{}
	for name, dir := range dirs {
		if prev, dup := seen[dir]; dup {
			t.Errorf("%s and %s share a cache directory: %s", prev, name, dir)
		}
		seen[dir] = name
	}
}

// TestCacheFollowsThePullRequestNotTheBranch — PreviewID excludes the branch and the
// commit, so a force-push or a rename must keep the cache the pull request filled.
// This is the reason it is keyed on the preview rather than on the head branch.
func TestCacheFollowsThePullRequestNotTheBranch(t *testing.T) {
	root := t.TempDir()
	b := &Builder{
		defaults: config.BuildDefaults{CacheDir: root},
		log:      slog.New(slog.DiscardHandler),
	}

	before := testPR("acme", "docs", 2)
	before.Branch, before.HeadSHA = "add-guide", "aaaaaaaaaaaa"

	after := testPR("acme", "docs", 2)
	after.Branch, after.HeadSHA = "add-guide-renamed", "bbbbbbbbbbbb"

	first, err := b.cacheMounts(before)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.cacheMounts(after)
	if err != nil {
		t.Fatal(err)
	}
	if sourceOf(t, first) != sourceOf(t, second) {
		t.Errorf("a rename moved the cache: %s then %s", sourceOf(t, first), sourceOf(t, second))
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

// TestCacheDirIsNotBuiltFromWebhookText — the owner and repository names arrive from
// a webhook, and joining one onto a path is how an owner of ".." would put a cache
// outside the cache root. The preview ID is a hex digest, so it cannot; this pins
// that the mount source stays inside the root even for hostile names.
func TestCacheDirIsNotBuiltFromWebhookText(t *testing.T) {
	root := t.TempDir()
	b := &Builder{
		defaults: config.BuildDefaults{CacheDir: root},
		log:      slog.New(slog.DiscardHandler),
	}

	for _, c := range []struct{ owner, repo string }{
		{"..", "docs"},
		{"../..", "docs"},
		{"acme", "../../../etc"},
		{"a/b", `c\d`},
		{"", ""},
	} {
		args, err := b.cacheMounts(testPR(c.owner, c.repo, 1))
		if err != nil {
			t.Fatal(err)
		}
		src := sourceOf(t, args)
		wantPrefix, err := hostMountPath(root)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(src, wantPrefix+"/") {
			t.Errorf("owner %q repo %q produced a cache outside the root: %s", c.owner, c.repo, src)
		}
		if strings.Contains(src, "..") {
			t.Errorf("owner %q repo %q produced a traversal: %s", c.owner, c.repo, src)
		}
	}
}
