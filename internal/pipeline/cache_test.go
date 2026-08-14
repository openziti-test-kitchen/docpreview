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
// A **volume**, not a bind mount, and that is the interesting half. A bind mount on
// Windows fills the cache at 0.4 MB/s, since every package tarball crosses WSL to NTFS —
// making the thing meant to speed builds up the slowest part of one. See CacheVolume.
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

	// One volume per manager, named for the repository. Shared between managers, pnpm's
	// hard-linked store would land inside another manager's tree.
	for _, m := range []string{"npm", "yarn", "pnpm"} {
		want := "type=volume,source=" + CacheVolume(CacheScope(pr), m) + ",target=/cache/" + m
		if !strings.Contains(joined, want) {
			t.Errorf("no mount for the %s cache: want %s in\n%s", m, want, joined)
		}
	}

	// No host path anywhere: a bind mount here would cost twenty minutes a build on
	// Windows, which is the regression this guards against.
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

// Every pull request against one repository shares its cache, and two repositories never do.
//
// Sharing is the point: keyed per preview, the first build of every new pull request spent 49
// seconds in `[3/5] Fetching packages` re-downloading a tree the repository had already fetched
// many times. Safe because all three managers' caches are content-addressed and written by atomic
// rename, which is what makes concurrent installs safe in the first place. node_modules is not
// shared, and must not be — see cacheMounts.
//
// Two repositories must still be separate, including two that share a name on different platforms:
// a cache one repository can write and another reads is one repository's dependency serving
// another's build.
func TestOneCachePerRepositorySharedByItsPullRequests(t *testing.T) {
	b := &Builder{
		defaults: config.BuildDefaults{CacheDir: t.TempDir()},
		log:      slog.New(slog.DiscardHandler),
	}

	mounts := func(pr model.PullRequest) string {
		args, err := b.cacheMounts(pr)
		if err != nil {
			t.Fatal(err)
		}
		return sourceOf(t, args)
	}

	two := mounts(testPR("acme", "docs", 2))
	three := mounts(testPR("acme", "docs", 3))
	if two != three {
		t.Errorf("two pull requests on one repository have different caches: %s and %s", two, three)
	}

	if other := mounts(testPR("other", "docs", 2)); other == two {
		t.Errorf("acme/docs and other/docs share the cache %s", other)
	}

	// Same owner and name, different platform. They are unrelated repositories.
	bb := testPR("acme", "docs", 2)
	bb.Repo.Platform = model.PlatformBitbucket
	if mounts(bb) == two {
		t.Errorf("github and bitbucket acme/docs share the cache %s", two)
	}
}

// A force-push or a branch rename must not move the cache. The scope is the repository, so
// neither the branch nor the commit is in it.
func TestCacheFollowsTheRepositoryNotTheBranch(t *testing.T) {
	b := &Builder{
		defaults: config.BuildDefaults{CacheDir: t.TempDir()},
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
