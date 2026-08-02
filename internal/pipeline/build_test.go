package pipeline

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
)

// writeIndex creates a build output directory containing index.html.
func writeIndex(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVerifyBaseURLAcceptsMatchingSite(t *testing.T) {
	dir := writeIndex(t, `<html><head>
<link rel="stylesheet" href="/assets/css/styles.abc.css">
<script src="/assets/js/main.def.js"></script>
<img src="/img/logo.svg">
</head><body><a href="/docs/intro">Intro</a><a href="/blog">Blog</a></body></html>`)

	if err := verifyBaseURL(dir, "/"); err != nil {
		t.Fatalf("verifyBaseURL rejected a matching site: %v", err)
	}
}

func TestVerifyBaseURLIgnoresNavigationLinks(t *testing.T) {
	// The shape that would break a correct build. A landing page links into /docs/ many
	// times, so counting every href would make "docs" the dominant first path segment,
	// reporting a site built for "/" as built for "/docs/" — while the asset the error
	// would quote, /img/favicon.ico, is not under /docs/ at all.
	//
	// Nav links outnumber assets here on purpose; that is what a landing page
	// looks like.
	dir := writeIndex(t, `<html><head>
<link rel=stylesheet href=/assets/css/styles.abc.css>
<script src=/assets/js/main.def.js defer></script>
<link rel=icon href=/img/favicon.ico>
</head><body>
<a href=/docs/intro>What this is</a>
<a href=/docs/quickstart>Quickstart</a>
<a href=/docs/architecture>Architecture</a>
<a href=/docs/exposers>Exposers</a>
<a href=/docs/reference/cli>CLI</a>
<a href=/docs/reference/security>Security</a>
<a href=/docs/runbooks/github-app>Runbook</a>
</body></html>`)

	if err := verifyBaseURL(dir, "/"); err != nil {
		t.Fatalf("navigation links were counted as evidence about the base URL: %v", err)
	}
}

func TestVerifyBaseURLStillCatchesAMisbuiltSiteAmongNavLinks(t *testing.T) {
	// The other half: the asset corroboration must not blind the check. A site
	// really built for /zrok/ prefixes everything Docusaurus emits — assets and
	// routes alike — so a page full of links is exactly what a misbuilt site looks
	// like, and volume must not hide it.
	dir := writeIndex(t, `<html><head>
<link rel=stylesheet href=/zrok/assets/css/styles.abc.css>
<script src=/zrok/assets/js/main.def.js defer></script>
<link rel=icon href=/zrok/img/favicon.ico>
</head><body>
<a href=/zrok/docs/intro>Intro</a>
<a href=/zrok/docs/quickstart>Quickstart</a>
<a href=/zrok/docs/architecture>Architecture</a>
<a href=/zrok/docs/exposers>Exposers</a>
</body></html>`)

	if err := verifyBaseURL(dir, "/"); err == nil {
		t.Fatal("a site built for /zrok/ was accepted at /")
	}
}

func TestAssetRefsKeepsFilesAndDropsRoutes(t *testing.T) {
	got := assetRefs([]string{
		"/assets/css/styles.abc.css",
		"/assets/js/main.def.js?v=2",
		"/img/favicon.ico",
		"/docs/intro",
		"/docs/reference/cli",
		"/",
		"/assets/fonts/x.woff2#iefix",
	})
	want := []string{
		"/assets/css/styles.abc.css",
		"/assets/js/main.def.js?v=2",
		"/img/favicon.ico",
		"/assets/fonts/x.woff2#iefix",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("assetRefs = %v, want %v", got, want)
	}
}

func TestVerifyBaseURLAcceptsMatchingPrefixedSite(t *testing.T) {
	dir := writeIndex(t, `<html><head>
<link rel="stylesheet" href="/zrok/assets/css/styles.abc.css">
<script src="/zrok/assets/js/main.def.js"></script>
</head><body><a href="/zrok/docs/intro">Intro</a></body></html>`)

	if err := verifyBaseURL(dir, "/zrok/"); err != nil {
		t.Fatalf("verifyBaseURL rejected a matching prefixed site: %v", err)
	}
}

func TestVerifyBaseURLAcceptsAMultiSegmentBase(t *testing.T) {
	// The path-mounting exposer serves previews at /preview/<name>/, which is two
	// segments. Inference only ever reports the *first* segment, so comparing an
	// inferred base URL for equality instead of checking the prefix directly would
	// infer /preview/handbook-new-install-guide/ as /preview/ and reject it for a
	// mismatch that does not exist — naming two base URLs that are the same one.
	base := "/preview/handbook-new-install-guide/"
	dir := writeIndex(t, `<html><head>
<link rel="stylesheet" href="`+base+`assets/css/styles.abc.css">
<script src="`+base+`assets/js/main.def.js"></script>
</head><body><a href="`+base+`docs/intro">Intro</a></body></html>`)

	if err := verifyBaseURL(dir, base); err != nil {
		t.Fatalf("verifyBaseURL rejected a correctly built multi-segment site: %v", err)
	}
}

func TestVerifyBaseURLCatchesAMultiSegmentMismatch(t *testing.T) {
	// And the check still has teeth at two segments: a site built for one
	// preview's path must not be accepted as another's.
	dir := writeIndex(t, `<html><head>
<link rel="stylesheet" href="/preview/docs-main/assets/css/styles.abc.css">
<script src="/preview/docs-main/assets/js/main.def.js"></script>
</head><body><a href="/preview/docs-main/docs/intro">Intro</a></body></html>`)

	if err := verifyBaseURL(dir, "/preview/handbook-main/"); err == nil {
		t.Fatal("verifyBaseURL accepted a site built for a different preview's path")
	}
}

func TestVerifyBaseURLCatchesTheClassicMismatch(t *testing.T) {
	// The failure this exists for: docusaurus.config hardcodes a GitHub Pages
	// baseUrl, we serve at the root of a zrok hostname, and every asset 404s
	// while index.html loads fine.
	dir := writeIndex(t, `<html><head>
<link rel="stylesheet" href="/my-project/assets/css/styles.abc.css">
<script src="/my-project/assets/js/main.def.js"></script>
</head><body><a href="/my-project/docs/intro">Intro</a></body></html>`)

	err := verifyBaseURL(dir, "/")
	if err == nil {
		t.Fatal("verifyBaseURL accepted a site built for a different base URL")
	}

	msg := err.Error()
	for _, want := range []string{"/my-project/", "DOCUSAURUS_BASE_URL", config.RepoConfigName} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
}

// TestVerifyBaseURLAcceptsARootSiteWhoseRefsAreMostlyAssets guards against a false
// refusal: a site built at "/" whose index.html is mostly asset references under
// /assets/ would otherwise fail with a diagnosis like:
//
//	preview base URL:    /
//	built-in base URL:   /assets/
//	asset in index.html: /assets/css/styles.68c3c27c.css
//
// "/assets/" is not a base URL any site has been built for. `assets` dominates the first
// path segment, and the corroborating question — are the assets under /assets/ — cannot
// answer no on its own, so dominance and corroboration alone read the same evidence twice.
// `routeUnder` is what breaks the tie: a real base prefix contains a route as well as the
// assets.
func TestVerifyBaseURLAcceptsARootSiteWhoseRefsAreMostlyAssets(t *testing.T) {
	dir := writeIndex(t, `<html><head>
<link rel="stylesheet" href="/assets/css/styles.68c3c27c.css">
<link rel="stylesheet" href="/assets/css/theme.11ab22cd.css">
<script src="/assets/js/runtime.9f8e7d.js"></script>
<script src="/assets/js/main.1a2b3c.js"></script>
</head><body>
<a href="/docs/customer-connect/intro">Intro</a>
</body></html>`)

	if err := verifyBaseURL(dir, "/"); err != nil {
		t.Fatalf("a site built at the root was refused:\n%v", err)
	}
}

// TestVerifyBaseURLStillCatchesAPrefixWhoseRoutesAgree is the other side of that fix: with
// a route *and* the assets under the prefix, the two signals are independent and the
// mismatch is real. Without this, the fix above could be "stop checking".
func TestVerifyBaseURLStillCatchesAPrefixWhoseRoutesAgree(t *testing.T) {
	dir := writeIndex(t, `<html><head>
<link rel="stylesheet" href="/zrok/assets/css/a.css">
<script src="/zrok/assets/js/b.js"></script>
</head><body><a href="/zrok/docs/intro">Intro</a></body></html>`)

	err := verifyBaseURL(dir, "/")
	if err == nil {
		t.Fatal("a site built for /zrok/ was accepted at /")
	}
	if !strings.Contains(err.Error(), "/zrok/") {
		t.Errorf("the error does not name the prefix it found:\n%v", err)
	}
}

func TestVerifyBaseURLToleratesAStrayAbsoluteLink(t *testing.T) {
	// A hand-written "/" in a footer must not fail the build.
	dir := writeIndex(t, `<html><head>
<link rel="stylesheet" href="/zrok/assets/css/a.css">
<script src="/zrok/assets/js/b.js"></script>
<script src="/zrok/assets/js/c.js"></script>
</head><body><a href="/">Home</a></body></html>`)

	if err := verifyBaseURL(dir, "/zrok/"); err != nil {
		t.Fatalf("verifyBaseURL failed on one stray link: %v", err)
	}
}

func TestVerifyBaseURLIgnoresRelativeAndExternalRefs(t *testing.T) {
	dir := writeIndex(t, `<html><head>
<link rel="stylesheet" href="assets/css/a.css">
<script src="//cdn.example.com/x.js"></script>
<img src="https://example.com/logo.png">
</head></html>`)

	if err := verifyBaseURL(dir, "/anything/"); err != nil {
		t.Fatalf("verifyBaseURL failed on a relative site: %v", err)
	}
}

// realDocusaurusIndex is the shape Docusaurus actually emits: minified HTML
// with unquoted attribute values. Captured from a real `docusaurus build` with
// DOCUSAURUS_BASE_URL=/zrok/.
//
// A reference scanner that only matched href="..." would find zero references in output
// like this, and would skip the base URL check on every real site.
const realDocusaurusIndex = `<!doctype html><html lang=en dir=ltr><head><meta charset=UTF-8>` +
	`<link rel=icon href=/zrok/img/favicon.ico><title>docpreview</title>` +
	`<link rel=canonical href=https://docpreview.example.com/zrok/>` +
	`<link rel=stylesheet href=/zrok/assets/css/styles.160a45a5.css>` +
	`<script src=/zrok/assets/js/runtime~main.ea36d912.js defer></script>` +
	`<script src=/zrok/assets/js/main.2fee3156.js defer></script></head>` +
	`<body><img src=/zrok/img/logo.svg><a href=/zrok/docs/intro>Docs</a>` +
	`<a href="https://github.com/netfoundry/docpreview">GitHub</a></body></html>`

func TestRefsFromHandlesMinifiedDocusaurusOutput(t *testing.T) {
	refs := refsFrom(realDocusaurusIndex)
	if len(refs) < 5 {
		t.Fatalf("found %d references in real Docusaurus output, want at least 5: %v", len(refs), refs)
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "/") {
			t.Errorf("extracted a non-root-relative reference: %q", ref)
		}
		if strings.Contains(ref, ">") || strings.Contains(ref, " ") {
			t.Errorf("extracted reference ran past the attribute: %q", ref)
		}
	}
}

func TestRefsFromHandlesAllQuotingForms(t *testing.T) {
	refs := refsFrom(`<a href="/a/x">1</a><a href='/b/y'>2</a><a href=/c/z>3</a>` +
		`<script src=//cdn.example.com/x.js></script><img src=relative.png>`)

	want := []string{"/a/x", "/b/y", "/c/z"}
	if len(refs) != len(want) {
		t.Fatalf("refsFrom = %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("refsFrom[%d] = %q, want %q", i, refs[i], want[i])
		}
	}
}

func TestVerifyBaseURLOnRealDocusaurusOutput(t *testing.T) {
	dir := writeIndex(t, realDocusaurusIndex)

	if err := verifyBaseURL(dir, "/zrok/"); err != nil {
		t.Errorf("real output built for /zrok/ was rejected at /zrok/: %v", err)
	}
	if err := verifyBaseURL(dir, "/"); err == nil {
		t.Error("real output built for /zrok/ was accepted at /")
	}
}

func TestVerifyBaseURLSkipsSitesWithoutIndex(t *testing.T) {
	if err := verifyBaseURL(t.TempDir(), "/"); err != nil {
		t.Fatalf("verifyBaseURL failed with no index.html: %v", err)
	}
}

func TestInferBaseURL(t *testing.T) {
	tests := []struct {
		name string
		refs []string
		want string
	}{
		{
			"prefixed docusaurus site",
			[]string{"/my-project/assets/css/a.css", "/my-project/assets/js/b.js", "/my-project/docs/intro"},
			"/my-project/",
		},
		{
			"root docusaurus site scatters across segments",
			[]string{"/assets/css/a.css", "/assets/js/b.js", "/img/logo.svg", "/docs/intro", "/blog"},
			"/",
		},
		{
			"a stray root link does not hide the base path",
			[]string{"/zrok/assets/css/a.css", "/zrok/assets/js/b.js", "/zrok/docs/intro", "/"},
			"/zrok/",
		},
		{
			"a shared asset directory is not a base path",
			[]string{"/assets/a.css", "/assets/b.js", "/docs/intro", "/blog/post", "/img/x.png"},
			"/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inferBaseURL(tt.refs); got != tt.want {
				t.Errorf("inferBaseURL(%v) = %q, want %q", tt.refs, got, tt.want)
			}
		})
	}
}

func TestBuildEnvSetsBaseURLAndIgnoresRepoOverrides(t *testing.T) {
	// A pull request that could set DOCUSAURUS_BASE_URL could break its own
	// preview in a way that looks like our bug.
	cfg := config.RepoConfig{Build: config.RepoBuild{
		BaseURL: "/zrok/",
		Env: map[string]string{
			"DOCUSAURUS_BASE_URL": "/evil/",
			"DOCPREVIEW_COMMIT":   "0000000000000000000000000000000000000000",
			"MY_FLAG":             "yes",
		},
	}}
	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: "4f0c2a1deadbeef",
	}

	env := NewBuilder(config.BuildDefaults{}, slog.New(slog.DiscardHandler)).buildEnv(pr, cfg, nil)

	var sawBase, sawFlag bool
	for _, kv := range env {
		if kv == "DOCUSAURUS_BASE_URL=/evil/" {
			t.Error("repo config overrode the base URL")
		}
		if kv == "DOCPREVIEW_COMMIT=0000000000000000000000000000000000000000" {
			// A site that stamps its own commit is trusted to be telling the
			// truth about which build it is. A pull request that could forge it
			// could make a stale preview claim to be the current one.
			t.Error("repo config overrode the commit stamp")
		}
		if kv == "DOCUSAURUS_BASE_URL=/zrok/" {
			sawBase = true
		}
		if kv == "MY_FLAG=yes" {
			sawFlag = true
		}
	}
	if !sawBase {
		t.Error("DOCUSAURUS_BASE_URL was not set")
	}
	if !sawFlag {
		t.Error("an ordinary repo env var was dropped")
	}

	// What is being built, so a preview can say which push produced it.
	for _, want := range []string{
		"DOCPREVIEW_COMMIT=4f0c2a1deadbeef",
		"DOCPREVIEW_BRANCH=feature/x",
		"DOCPREVIEW_PR=42",
		"DOCPREVIEW_REPO=github:acme/docs",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s\ngot: %v", want, env)
		}
	}
}
