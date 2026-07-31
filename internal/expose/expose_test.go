package expose

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
)

func pr(branch string) model.PullRequest {
	return model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 42,
		Branch: branch,
	}
}

func TestRenderNameDefaultsToTheBranch(t *testing.T) {
	// "the unique url should be the branchname" is the requirement; this is it.
	got, err := RenderName("{{.Name}}", pr("release-2-1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "release-2-1" {
		t.Errorf("RenderName = %q, want %q", got, "release-2-1")
	}
}

func TestRenderNameSanitizesTheBranch(t *testing.T) {
	got, err := RenderName("{{.Name}}", pr("feature/JIRA-123_new guide"), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, "/_ ") {
		t.Errorf("RenderName = %q, which is not a valid DNS label", got)
	}
	if !strings.HasPrefix(got, "feature-jira-123-new-guide") {
		t.Errorf("RenderName = %q, want it to remain readable", got)
	}
}

func TestRenderNameEmptyTemplateIsUniquePerRepository(t *testing.T) {
	// The fallback used to be the branch alone, matching "the URL is the branch
	// name". It is not unique: every repository with a `main` branch renders to
	// `main`, and whichever preview publishes second takes the first one's URL.
	// Under the local exposer that showed up as a dashboard full of Ready rows
	// whose links answered connection-refused.
	//
	// The fallback here must stay in step with config.DefaultNameTemplate; they
	// are separate constants so that config does not have to import this
	// package for a string.
	got, err := RenderName("", pr("main"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "docs-main" {
		t.Errorf("RenderName with an empty template = %q, want %q", got, "docs-main")
	}

	other := pr("main")
	other.Repo.Name = "handbook"
	got2, err := RenderName("", other, "")
	if err != nil {
		t.Fatal(err)
	}
	if got == got2 {
		t.Errorf("two repositories with a `main` branch both render to %q", got)
	}
}

func TestRenderNameCanDisambiguateRepositories(t *testing.T) {
	// The documented fix for two repositories sharing one zrok namespace, where
	// a `main` branch in each would otherwise fight over the same name.
	got, err := RenderName("{{.Repo.Name}}-{{.Name}}", pr("main"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "docs-main" {
		t.Errorf("RenderName = %q, want %q", got, "docs-main")
	}
}

func TestRenderNameResanitizesTemplateOutput(t *testing.T) {
	// A repository name can contain characters that are legal on GitHub but
	// not in a hostname. Templating them in must not produce an invalid label.
	p := pr("main")
	p.Repo.Name = "docs.internal_v2"

	got, err := RenderName("{{.Repo.Name}}-{{.Name}}", p, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
			t.Fatalf("RenderName = %q, which contains %q", got, r)
		}
	}
}

// The name prefix is what lets two installations share one exposer account.
//
// Names are scoped to the account and shares to the environment, so two docpreviews on one
// zrok account do not reap each other's shares — but both want `docs-main`, and the second
// to ask gets a 409 for a name the account already holds.
func TestRenderNamePrependsThePrefix(t *testing.T) {
	got, err := RenderName("", pr("main"), "a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "a-docs-main" {
		t.Errorf("RenderName = %q, want %q", got, "a-docs-main")
	}

	// A trailing hyphen is how anybody writes a prefix, and means the same thing. Accepting
	// it rather than refusing avoids `a--docs-main`, which is what carrying it through would
	// produce.
	withHyphen, err := RenderName("", pr("main"), "a-")
	if err != nil {
		t.Fatal(err)
	}
	if withHyphen != got {
		t.Errorf(`"a-" rendered %q and "a" rendered %q`, withHyphen, got)
	}

	// And the point of it: the same preview on two installations is two names.
	plain, err := RenderName("", pr("main"), "")
	if err != nil {
		t.Fatal(err)
	}
	if plain == got {
		t.Errorf("both installations rendered %q, so they would fight over the name", got)
	}
}

// The prefix is reserved out of the label *before* truncation, and this is the test that
// pins why.
//
// Put it back and a long branch spends the whole label on itself, leaving the prefix to
// overrun the 48-character limit or the name to be cut to nothing. The collision hash that
// makes truncated names unique must also still be the last thing in the label, so two long
// branches under one prefix stay distinct. Only branch names nobody writes by accident reach
// this path, which is why it is a test rather than a comment.
func TestRenderNameKeepsThePrefixWhenTheBranchIsTooLong(t *testing.T) {
	long := pr("feature/a-really-quite-extraordinarily-long-branch-name-nobody-should-have-written")
	got, err := RenderName("{{.Repo.Name}}-{{.Name}}", long, "a")
	if err != nil {
		t.Fatal(err)
	}

	if len(got) > model.MaxLabelLen {
		t.Errorf("RenderName = %q, %d characters, over the %d limit", got, len(got), model.MaxLabelLen)
	}
	if !strings.HasPrefix(got, "a-") {
		t.Errorf("RenderName = %q, which lost the prefix to truncation", got)
	}

	// Two long branches that truncate alike must still differ, and both keep the prefix.
	other := pr("feature/a-really-quite-extraordinarily-long-branch-name-nobody-would-ever-write")
	got2, err := RenderName("{{.Repo.Name}}-{{.Name}}", other, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got == got2 {
		t.Errorf("two long branches both rendered %q", got)
	}
	if !strings.HasPrefix(got2, "a-") {
		t.Errorf("RenderName = %q, which lost the prefix", got2)
	}
}

func TestValidPrefixRefusesWhatWouldBreakAHostname(t *testing.T) {
	// Empty is the default and means "one installation", which must stay valid — as must a
	// bare hyphen, which is what clearing the field in the dashboard leaves behind.
	for _, ok := range []string{"", "-", "a", "a-", "aws", "aws-staging", "sg4", "b2"} {
		if why := model.ValidPrefix(ok); why != "" {
			t.Errorf("ValidPrefix(%q) refused it: %s", ok, why)
		}
	}
	// Refused rather than sanitized: this string is in the URL a reviewer bookmarks, and
	// quietly turning `AWS_prod` into `aws-prod` is how two installations end up
	// disagreeing about which is which.
	for _, bad := range []string{"AWS", "aws_prod", "aws prod", "aws.prod",
		"far-too-long-to-be-a-prefix"} {
		if why := model.ValidPrefix(bad); why == "" {
			t.Errorf("ValidPrefix(%q) accepted it", bad)
		}
	}
}

func TestRenderNameRejectsABrokenTemplate(t *testing.T) {
	if _, err := RenderName("{{.Nope", pr("main"), ""); err == nil {
		t.Fatal("RenderName accepted an unparseable template")
	}
	if _, err := RenderName("{{.NoSuchField}}", pr("main"), ""); err == nil {
		t.Fatal("RenderName accepted a template naming a field that does not exist")
	}
}

func TestJoinURL(t *testing.T) {
	tests := []struct {
		origin, baseURL, want string
	}{
		{"https://x.share.zrok.io", "/", "https://x.share.zrok.io/"},
		{"https://x.share.zrok.io/", "/", "https://x.share.zrok.io/"},
		{"https://x.share.zrok.io", "", "https://x.share.zrok.io/"},
		{"https://x.share.zrok.io", "/docs/", "https://x.share.zrok.io/docs/"},
		{"https://x.share.zrok.io/", "/zrok/", "https://x.share.zrok.io/zrok/"},
	}
	for _, tt := range tests {
		if got := JoinURL(tt.origin, tt.baseURL); got != tt.want {
			t.Errorf("JoinURL(%q, %q) = %q, want %q", tt.origin, tt.baseURL, got, tt.want)
		}
	}
}

// localOnServer builds a path-mounting exposer behind a real HTTP server, which
// is how it runs for real: previews are paths on the daemon's own listener, not
// ports of their own. The origin has to be set after the server starts, since
// that is when the address exists.
func localOnServer(t *testing.T) *Local {
	t.Helper()
	ex := NewLocal(discardLogger(), "")
	srv := httptest.NewServer(ex.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { ex.Close() })
	ex.SetOrigin(srv.URL)
	return ex
}

func body(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestLocalExposerLifecycle(t *testing.T) {
	// The Exposer contract, exercised against the reference implementation.
	ctx := context.Background()
	ex := localOnServer(t)

	if err := ex.Validate(ctx); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "preview body")
	})

	pub, err := ex.Publish(ctx, Spec{PreviewID: "abc123", Name: "feature-x", BaseURL: "/"}, handler)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !strings.HasSuffix(pub.URL, "/preview/feature-x/") {
		t.Errorf("URL = %q, want it to end with the mount path", pub.URL)
	}

	if code, got := body(t, pub.URL); code != http.StatusOK || got != "preview body" {
		t.Errorf("GET %s = %d %q", pub.URL, code, got)
	}

	if err := pub.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close must be safe to call twice: the daemon closes publications on
	// teardown and the exposer closes them again on shutdown.
	if err := pub.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// The listener is shared, so a withdrawn preview does not refuse the
	// connection — it 404s, and says which name is missing.
	code, got := body(t, pub.URL)
	if code != http.StatusNotFound {
		t.Errorf("after Close, GET %s = %d %q, want 404", pub.URL, code, got)
	}
	if !strings.Contains(got, "feature-x") {
		t.Errorf("the 404 does not name the preview: %q", got)
	}
}

func TestLocalExposerURLIsStableAcrossRepublish(t *testing.T) {
	// Someone pushing three commits in a minute is the normal case: publishing
	// the same preview again must replace, not fail, and must not move the URL.
	//
	// The URL used to be an ephemeral port, so it moved on every publish and
	// did not survive a restart at all — which left the database full of `ready`
	// rows pointing at ports nothing was listening on. Deriving it from the name
	// is what makes a link in a pull request comment worth writing down.
	ctx := context.Background()
	ex := localOnServer(t)

	spec := Spec{PreviewID: "abc123", Name: "feature-x", BaseURL: "/"}

	first, err := ex.Publish(ctx, spec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "first")
	}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ex.Publish(ctx, spec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "second")
	}))
	if err != nil {
		t.Fatalf("republishing failed: %v", err)
	}

	if first.URL != second.URL {
		t.Errorf("the URL moved on republish: %q then %q", first.URL, second.URL)
	}
	if _, got := body(t, second.URL); got != "second" {
		t.Errorf("served %q, want the new handler", got)
	}
}

func TestLocalExposerSurvivesTheDaemonsReplaceThenCloseOrder(t *testing.T) {
	// The daemon replaces a preview in this order, and the order is deliberate:
	// publish the new one, then close the old Publication, so there is never a
	// moment when nothing is serving.
	//
	//	pub, _ := exposer.Publish(...)
	//	if old := d.live[id]; old != nil { old.Close() }
	//	d.live[id] = pub
	//
	// Both publications carry the same preview ID, so a close that deleted by
	// key alone tore down the mount its own replacement had just installed.
	// Every rebuilt preview went 404 while the database still said `ready`, and
	// the only ones that kept working were the ones nobody had pushed to twice.
	//
	// No existing test caught it: publishing twice was covered, and closing was
	// covered, but not closing the superseded handle *after* the replacement
	// was already in place.
	ctx := context.Background()
	ex := localOnServer(t)

	spec := Spec{PreviewID: "same-id", Name: "docs-main", BaseURL: "/"}

	old, err := ex.Publish(ctx, spec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "old")
	}))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := ex.Publish(ctx, spec, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "new")
	}))
	if err != nil {
		t.Fatal(err)
	}

	// The daemon's next line.
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	code, got := body(t, fresh.URL)
	if code != http.StatusOK {
		t.Fatalf("closing the superseded publication took the live one down: %d", code)
	}
	if got != "new" {
		t.Errorf("served %q, want the replacement", got)
	}
}

func TestLocalExposerRefusesANameAnotherPreviewHolds(t *testing.T) {
	// Branch names are not unique across repositories: four projects each with a
	// `new-install-guide` branch all render to the same label. The map of live
	// publications used to be keyed by that label, so every publish tore down a
	// different project's preview — silently, because withdrawing a preview you
	// believe you own is a normal thing to do. The only symptom was a dashboard
	// full of `ready` rows whose links did not answer.
	//
	// Now the name is the path, so the second preview cannot have it and is told
	// so. Serving the wrong site under somebody else's URL is the one outcome
	// worse than failing the build.
	ctx := context.Background()
	ex := localOnServer(t)

	same := "new-install-guide"
	a, err := ex.Publish(ctx, Spec{PreviewID: "preview-a", Name: same, BaseURL: "/"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "a") }))
	if err != nil {
		t.Fatal(err)
	}

	_, err = ex.Publish(ctx, Spec{PreviewID: "preview-b", Name: same, BaseURL: "/"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "b") }))
	if err == nil {
		t.Fatal("a second preview took a name already in use")
	}
	// The message has to name the other preview and the way out.
	for _, want := range []string{same, "preview-a", "name_template"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}

	// And the incumbent is untouched.
	if _, got := body(t, a.URL); got != "a" {
		t.Errorf("the first preview now serves %q", got)
	}
}

func TestLocalExposerServesTwoPreviewsAtOnce(t *testing.T) {
	ctx := context.Background()
	ex := localOnServer(t)

	a, err := ex.Publish(ctx, Spec{PreviewID: "id-a", Name: "docs-main", BaseURL: "/"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "a") }))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ex.Publish(ctx, Spec{PreviewID: "id-b", Name: "handbook-main", BaseURL: "/"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "b") }))
	if err != nil {
		t.Fatal(err)
	}

	if _, got := body(t, a.URL); got != "a" {
		t.Errorf("%s served %q", a.URL, got)
	}
	if _, got := body(t, b.URL); got != "b" {
		t.Errorf("%s served %q", b.URL, got)
	}

	// Closing one leaves the other alone.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, got := body(t, b.URL); got != "b" {
		t.Errorf("closing one preview affected the other: %q", got)
	}
}

func TestLocalExposerReapDropsUnknownMounts(t *testing.T) {
	// Reap used to be a no-op here, on the reasoning that a loopback listener
	// cannot outlive its process. That held while a preview owned a port. A
	// mount is a map entry, and one left behind after its preview is deleted
	// keeps serving a URL nothing records.
	ctx := context.Background()
	ex := localOnServer(t)

	keep, err := ex.Publish(ctx, Spec{PreviewID: "keep-me", Name: "docs-main", BaseURL: "/"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "kept") }))
	if err != nil {
		t.Fatal(err)
	}
	drop, err := ex.Publish(ctx, Spec{PreviewID: "drop-me", Name: "handbook-main", BaseURL: "/"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "dropped") }))
	if err != nil {
		t.Fatal(err)
	}

	if err := ex.Reap(ctx, map[string]bool{"keep-me": true}); err != nil {
		t.Fatal(err)
	}

	if _, got := body(t, keep.URL); got != "kept" {
		t.Errorf("the kept preview serves %q", got)
	}
	if code, _ := body(t, drop.URL); code != http.StatusNotFound {
		t.Errorf("the reaped preview still answers with %d", code)
	}
}

func TestLocalExposerMountPathFoldsIntoTheBuiltBaseURL(t *testing.T) {
	// MountPath has to be callable before anything is published, because the
	// daemon needs it before the build: Docusaurus bakes baseUrl in at build
	// time, and a site built for "/" and served under a prefix returns its HTML
	// and 404s every asset in it.
	ex := NewLocal(discardLogger(), "http://127.0.0.1:8471")
	got := ex.MountPath("docs-main")
	if got != "/preview/docs-main/" {
		t.Errorf("MountPath = %q", got)
	}
	if !strings.HasPrefix(got, "/") || !strings.HasSuffix(got, "/") {
		t.Errorf("MountPath = %q, want leading and trailing slashes", got)
	}
}

func TestLocalExposerUnknownPathIs404(t *testing.T) {
	ex := localOnServer(t)
	code, got := body(t, ex.origin+MountPrefix+"never-published/")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
	if !strings.Contains(got, "never-published") {
		t.Errorf("the 404 does not name the preview: %q", got)
	}
}

func TestLocalExposerCloseStopsEverything(t *testing.T) {
	ctx := context.Background()
	ex := localOnServer(t)

	var urls []string
	for _, name := range []string{"a", "b", "c"} {
		pub, err := ex.Publish(ctx, Spec{PreviewID: name, Name: name, BaseURL: "/"},
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "up") }))
		if err != nil {
			t.Fatal(err)
		}
		urls = append(urls, pub.URL)
	}

	if err := ex.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The listener belongs to the daemon and stays up, so a closed exposer
	// answers 404 rather than refusing the connection.
	for _, url := range urls {
		if code, _ := body(t, url); code != http.StatusNotFound {
			t.Errorf("%s answered %d after Close, want 404", url, code)
		}
	}
}

func TestPublicationCloseHandlesNil(t *testing.T) {
	var p *Publication
	if err := p.Close(); err != nil {
		t.Errorf("closing a nil publication: %v", err)
	}
}

// discardLogger keeps test output readable.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
