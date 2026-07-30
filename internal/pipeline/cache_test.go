package pipeline

import (
	"log/slog"
	"regexp"
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

// TestCacheMountsPointEachManagerAtItsOwnVolume is the assertion that keeps builds
// fast. Without these mounts every build re-downloads its whole dependency tree,
// because the workspace they would otherwise cache into is created per commit and
// pruned with its siblings.
//
// A **volume**, not a bind mount, and that is the interesting half. As a bind mount on
// Windows the cache was measured filling at 0.4 MB/s — every package tarball crossing
// WSL to NTFS — which made the thing meant to speed builds up the slowest part of one.
// See CacheVolume.
func TestCacheMountsPointEachManagerAtItsOwnVolume(t *testing.T) {
	b := &Builder{log: slog.New(slog.DiscardHandler)}

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

	// One volume per manager, named for the preview. Shared, pnpm's hard-linked store
	// would land inside another manager's tree.
	for _, m := range []string{"npm", "yarn", "pnpm"} {
		want := "type=volume,source=" + CacheVolume(pr.PreviewID(), m) + ",target=/cache/" + m
		if !strings.Contains(joined, want) {
			t.Errorf("no mount for the %s cache: want %s in\n%s", m, want, joined)
		}
	}

	// No host path anywhere: a bind source is what this replaced, and one reappearing is
	// the regression that costs twenty minutes a build on Windows.
	if strings.Contains(joined, "type=bind") {
		t.Errorf("a cache is still bind-mounted from the host:\n%s", joined)
	}
}

// TestCachesExistWithoutACacheDir — a volume needs no configuration at all, where the
// bind mount needed a path. Every existing config says nothing about a cache, and the
// docker driver has to be fast anyway.
func TestCachesExistWithoutACacheDir(t *testing.T) {
	b := &Builder{log: slog.New(slog.DiscardHandler)}
	args, err := b.cacheMounts(testPR("owner", "repo", 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(args) == 0 {
		t.Error("no cache mounts without a cache_dir; the docker cache needs no host path")
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

// sourceOf returns the first cache volume name in a docker argument list.
func sourceOf(t *testing.T, args []string) string {
	t.Helper()
	for _, a := range args {
		if strings.HasPrefix(a, "type=volume,source=") {
			return strings.SplitN(strings.TrimPrefix(a, "type=volume,source="), ",", 2)[0]
		}
	}
	t.Fatalf("no volume mount in %v", args)
	return ""
}

// TestCacheNameIsNotBuiltFromWebhookText — the owner and repository names arrive from a
// webhook. As a path, an owner of ".." put a cache outside the cache root; as a docker
// volume name, a stray slash or dot produces either a name docker refuses or, worse, one
// that collides with another preview's.
//
// The preview ID is a hex digest, so neither is reachable. This pins that: the name is the
// documented prefix, twelve hex characters, and a manager — whatever the webhook said.
func TestCacheNameIsNotBuiltFromWebhookText(t *testing.T) {
	b := &Builder{log: slog.New(slog.DiscardHandler)}
	valid := regexp.MustCompile(`^docpreview-cache-[0-9a-f]{12}-(npm|yarn|pnpm)$`)

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
		for _, name := range volumesIn(args) {
			if !valid.MatchString(name) {
				t.Errorf("owner %q repo %q produced the volume name %q", c.owner, c.repo, name)
			}
		}
	}
}

// volumesIn returns every cache volume name in a docker argument list.
func volumesIn(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "type=volume,source=") {
			out = append(out,
				strings.SplitN(strings.TrimPrefix(a, "type=volume,source="), ",", 2)[0])
		}
	}
	return out
}
