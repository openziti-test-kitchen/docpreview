package bitbucket

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/vault"
)

const testSecret = "a-bitbucket-webhook-secret"

// newTestClient builds a client pointed at a fake API.
//
// The fake matters for more than convenience: resolveCommit makes a real request
// during VerifyWebhook, so a client with no reachable API cannot verify a single
// delivery — which is itself worth knowing when reading these tests.
func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	v, err := vault.OpenWithKey(dir+"/vault.age", "a-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	for k, val := range map[string]string{
		vault.KeyBitbucketHookSec:     testSecret,
		vault.KeyBitbucketAccessToken: "a-repository-access-token",
	} {
		if err := v.Set(k, vault.NewSecretString(val)); err != nil {
			t.Fatal(err)
		}
	}

	c, err := New(config.BitbucketConfig{
		Enabled: true,
		APIBase: srv.URL,
		Auth:    config.BitbucketAuthAccessToken,
	}, v, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// commitResolver answers the one endpoint VerifyWebhook calls, returning a full hash.
func commitResolver(full string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/commit/") {
			_ = json.NewEncoder(w).Encode(map[string]string{"hash": full})
			return
		}
		http.NotFound(w, r)
	})
}

// delivery builds a pull request webhook body. The hash is deliberately the twelve
// characters Bitbucket actually sends.
func delivery(sourceRepo string, id int) []byte {
	body := map[string]any{
		"pullrequest": map[string]any{
			"id":     id,
			"draft":  false,
			"source": map[string]any{
				"branch":     map[string]string{"name": "add-guide"},
				"commit":     map[string]string{"hash": "a4fd6c9db194"},
				"repository": map[string]string{"full_name": sourceRepo},
			},
			"destination": map[string]any{
				"branch":     map[string]string{"name": "main"},
				"repository": map[string]string{"full_name": "netfoundry/customer-connect-docs"},
			},
		},
		"repository": map[string]any{
			"slug":      "customer-connect-docs",
			"full_name": "netfoundry/customer-connect-docs",
			"workspace": map[string]string{"slug": "netfoundry"},
		},
	}
	out, _ := json.Marshal(body)
	return out
}

// headers builds the header map the ingress passes in.
//
// Through http.Header.Set rather than as a literal map, because net/http canonicalizes
// incoming header names — "X-Request-UUID" arrives as "X-Request-Uuid" — and a literal
// map keyed the way Atlassian spells it is a fixture that fails while the code is
// right.
func headers(event, sig string) map[string][]string {
	h := http.Header{}
	h.Set("X-Event-Key", event)
	h.Set("X-Request-UUID", "11111111-2222-3333-4444-555555555555")
	h.Set("X-Hub-Signature", sig)
	h.Set("X-Attempt-Number", "1")
	return h
}

// TestVerifyWebhookAcceptsASignedDelivery, and resolves the abbreviated hash while it
// is at it — a 12-character HeadSHA is a bug, not a shorter answer.
func TestVerifyWebhookAcceptsASignedDelivery(t *testing.T) {
	const full = "a4fd6c9db1940992c8af5c48401462100bd7d2f1"
	c, _ := newTestClient(t, commitResolver(full))

	body := delivery("netfoundry/customer-connect-docs", 20)
	events, err := c.VerifyWebhook(t.Context(), headers("pullrequest:created", sign(body)), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	e := events[0]
	if e.Kind != scm.EventBuild {
		t.Errorf("kind = %q, want %q", e.Kind, scm.EventBuild)
	}
	if e.PR.HeadSHA != full {
		t.Errorf("HeadSHA = %q (%d chars), want the full %q — an abbreviated hash checks "+
			"out fine and then fails every comparison", e.PR.HeadSHA, len(e.PR.HeadSHA), full)
	}
	if e.PR.Repo.Platform != model.PlatformBitbucket {
		t.Errorf("platform = %q", e.PR.Repo.Platform)
	}
	if e.PR.Repo.Owner != "netfoundry" || e.PR.Repo.Name != "customer-connect-docs" {
		t.Errorf("repo = %s", e.PR.Repo.Slug())
	}
	if e.PR.Number != 20 || e.PR.Branch != "add-guide" || e.PR.BaseBranch != "main" {
		t.Errorf("pr = %+v", e.PR)
	}
	if e.Delivery == "" {
		t.Error("no delivery id, so a report of 'nothing happened' cannot be traced")
	}
}

// TestVerifyWebhookRefusesAnUnsignedDelivery.
//
// A blank Secret field in Bitbucket's form produces deliveries with no signature at
// all — a webhook that works perfectly and authenticates nothing. Accepting those
// turns one empty form field into an unauthenticated build trigger, so the absence of
// the header must be a verification failure and the error must name the field.
func TestVerifyWebhookRefusesAnUnsignedDelivery(t *testing.T) {
	c, _ := newTestClient(t, commitResolver("a4fd6c9db1940992c8af5c48401462100bd7d2f1"))

	body := delivery("netfoundry/customer-connect-docs", 20)
	h := headers("pullrequest:created", "")
	delete(h, "X-Hub-Signature")

	_, err := c.VerifyWebhook(t.Context(), h, body)
	if !errors.Is(err, scm.ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature — the ingress chooses 401 over 400 on that", err)
	}
	if !strings.Contains(err.Error(), vault.KeyBitbucketHookSec) {
		t.Errorf("the error does not name the vault key to set: %v", err)
	}
}

func TestVerifyWebhookRefusesAWrongSignature(t *testing.T) {
	c, _ := newTestClient(t, commitResolver("a4fd6c9db1940992c8af5c48401462100bd7d2f1"))

	body := delivery("netfoundry/customer-connect-docs", 20)
	mac := hmac.New(sha256.New, []byte("the-wrong-secret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if _, err := c.VerifyWebhook(t.Context(), headers("pullrequest:created", sig), body); !errors.Is(err, scm.ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

// TestVerifyWebhookRefusesAnUnknownHMACMethod. Atlassian reserves the right to send
// something other than sha256, and an accepted-but-unverified delivery is a build
// trigger — so an unknown method is a failure rather than an unknown to wave through.
func TestVerifyWebhookRefusesAnUnknownHMACMethod(t *testing.T) {
	c, _ := newTestClient(t, commitResolver("a4fd6c9db1940992c8af5c48401462100bd7d2f1"))

	body := delivery("netfoundry/customer-connect-docs", 20)
	// A correct SHA-1 HMAC, announced as sha1. Correct for its algorithm and still
	// not something this code accepts.
	sig := strings.Replace(sign(body), "sha256=", "sha1=", 1)

	if _, err := c.VerifyWebhook(t.Context(), headers("pullrequest:created", sig), body); !errors.Is(err, scm.ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

// TestVerifyWebhookRefusesAForkPullRequest.
//
// Building a fork means cloning and running a stranger's build scripts under our
// credential. Invariant on every platform, and a new client that omitted the check
// would satisfy every existing test.
func TestVerifyWebhookRefusesAForkPullRequest(t *testing.T) {
	c, _ := newTestClient(t, commitResolver("a4fd6c9db1940992c8af5c48401462100bd7d2f1"))

	body := delivery("someone-else/customer-connect-docs", 21)
	events, err := c.VerifyWebhook(t.Context(), headers("pullrequest:created", sign(body)), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a fork pull request produced %d events, want none", len(events))
	}
}

// TestVerifyWebhookRefusesAnAbsentSourceRepository.
//
// GitHub's client has a known gap here — a null head repo from a deleted fork skips
// the refusal — and reproducing it on a new platform would be choosing to. Absent
// counts as untrusted.
func TestVerifyWebhookRefusesAnAbsentSourceRepository(t *testing.T) {
	c, _ := newTestClient(t, commitResolver("a4fd6c9db1940992c8af5c48401462100bd7d2f1"))

	var payload map[string]any
	if err := json.Unmarshal(delivery("netfoundry/customer-connect-docs", 22), &payload); err != nil {
		t.Fatal(err)
	}
	source := payload["pullrequest"].(map[string]any)["source"].(map[string]any)
	delete(source, "repository")
	body, _ := json.Marshal(payload)

	events, err := c.VerifyWebhook(t.Context(), headers("pullrequest:created", sign(body)), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a payload with no source repository produced %d events, want none", len(events))
	}
}

// TestVerifyWebhookMapsEvents covers the table in docs/design/15-bitbucket.md,
// including the events that must be ignored rather than treated as failures.
func TestVerifyWebhookMapsEvents(t *testing.T) {
	c, _ := newTestClient(t, commitResolver("a4fd6c9db1940992c8af5c48401462100bd7d2f1"))

	cases := []struct {
		event string
		kind  scm.EventKind
		count int
	}{
		{"pullrequest:created", scm.EventBuild, 1},
		// Not GitHub's synchronize: it also fires for a description edit. Accepted
		// deliberately; Enqueue collapses the churn.
		{"pullrequest:updated", scm.EventBuild, 1},
		{"pullrequest:fulfilled", scm.EventTeardown, 1},
		{"pullrequest:rejected", scm.EventTeardown, 1},
		{"repo:push", "", 0},
		{"pullrequest:approved", "", 0},
		{"pullrequest:comment_created", "", 0},
	}

	for _, tc := range cases {
		body := delivery("netfoundry/customer-connect-docs", 20)
		events, err := c.VerifyWebhook(t.Context(), headers(tc.event, sign(body)), body)
		if err != nil {
			t.Fatalf("%s: %v", tc.event, err)
		}
		if len(events) != tc.count {
			t.Errorf("%s: %d events, want %d", tc.event, len(events), tc.count)
			continue
		}
		if tc.count > 0 && events[0].Kind != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.event, events[0].Kind, tc.kind)
		}
	}
}

// TestVerifyWebhookFailsWhenTheHashCannotBeResolved.
//
// A hard error rather than a fallback to the twelve characters. The abbreviation
// clones fine, which is exactly why carrying it would be a preview whose identity
// silently disagrees with itself.
func TestVerifyWebhookFailsWhenTheHashCannotBeResolved(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"type":"error","error":{"message":"Not found"}}`, http.StatusNotFound)
	}))

	body := delivery("netfoundry/customer-connect-docs", 20)
	_, err := c.VerifyWebhook(t.Context(), headers("pullrequest:created", sign(body)), body)
	if err == nil {
		t.Fatal("an unresolvable commit hash was accepted")
	}
	if !strings.Contains(err.Error(), "resolving the head commit") {
		t.Errorf("error does not say what failed: %v", err)
	}
}

// TestResolveCommitLeavesAFullHashAlone — cheap check, and it makes the day Bitbucket
// stops abbreviating a no-op rather than a wasted request per delivery.
func TestResolveCommitLeavesAFullHashAlone(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))

	const full = "a4fd6c9db1940992c8af5c48401462100bd7d2f1"
	got, err := c.resolveCommit(t.Context(), model.Repo{Owner: "netfoundry", Name: "docs"}, full)
	if err != nil {
		t.Fatal(err)
	}
	if got != full {
		t.Errorf("resolveCommit(%q) = %q", full, got)
	}
	if calls != 0 {
		t.Errorf("made %d API calls for a hash that was already full", calls)
	}
}

// TestTeardownNeedsNoCommitResolution. Nothing is built, so the extra request would be
// spent for nothing — and a teardown that failed because the commit endpoint was
// unreachable would leave a share published forever.
func TestTeardownNeedsNoCommitResolution(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))

	body := delivery("netfoundry/customer-connect-docs", 20)
	events, err := c.VerifyWebhook(t.Context(), headers("pullrequest:fulfilled", sign(body)), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != scm.EventTeardown {
		t.Fatalf("events = %+v", events)
	}
	if calls != 0 {
		t.Errorf("a teardown made %d API calls", calls)
	}
}

// TestPayloadFallsBackToFullName. The workspace/slug pair is the documented shape and
// full_name carries the same two values, so one spelling missing does not have to be
// fatal — this is the one part of the payload not read off a live REST object.
func TestPayloadFallsBackToFullName(t *testing.T) {
	body := []byte(`{
      "pullrequest": {"id": 7,
        "source": {"branch": {"name": "b"}, "commit": {"hash": "abc123abc123"},
                   "repository": {"full_name": "acme/docs"}},
        "destination": {"branch": {"name": "main"}}},
      "repository": {"full_name": "acme/docs"}}`)

	p, repo, err := parsePayload(body)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Owner != "acme" || repo.Name != "docs" {
		t.Errorf("repo = %+v", repo)
	}
	if p.ID != 7 {
		t.Errorf("id = %d", p.ID)
	}
}

func TestPayloadWithNoRepositoryIsAnError(t *testing.T) {
	if _, _, err := parsePayload([]byte(`{"pullrequest": {"id": 7}}`)); err == nil {
		t.Fatal("a payload naming no repository was accepted")
	}
}

// TestCloneURLIsBuiltNotDecorated.
//
// Bitbucket's own clone href already carries a username, so inserting credentials
// into it produces two `@` in the authority and a git failure whose message contains
// the token. The username here is the literal x-token-auth.
func TestCloneURLIsBuiltNotDecorated(t *testing.T) {
	c, _ := newTestClient(t, commitResolver("a4fd6c9db1940992c8af5c48401462100bd7d2f1"))

	pr := model.PullRequest{Repo: model.Repo{
		Platform: model.PlatformBitbucket, Owner: "netfoundry", Name: "customer-connect-docs",
	}, Number: 20}

	got, err := c.CloneURL(t.Context(), pr)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://x-token-auth:a-repository-access-token@bitbucket.org/netfoundry/customer-connect-docs.git"
	if got != want {
		t.Errorf("CloneURL =\n  %s\nwant\n  %s", got, want)
	}
	if strings.Count(got, "@") != 1 {
		t.Errorf("more than one @ in the authority: %s", got)
	}
}

// TestAPIBaseMustBeTheAPIHost. Since 4 May 2026 authenticated calls must go to
// api.bitbucket.org; bitbucket.org/api answers 403 with a body that does not say so.
func TestAPIBaseMustBeTheAPIHost(t *testing.T) {
	dir := t.TempDir()
	v, err := vault.OpenWithKey(dir+"/vault.age", "a-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set(vault.KeyBitbucketHookSec, vault.NewSecretString(testSecret)); err != nil {
		t.Fatal(err)
	}

	_, err = New(config.BitbucketConfig{
		Enabled: true,
		APIBase: "https://bitbucket.org/api",
		Auth:    config.BitbucketAuthAccessToken,
	}, v, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("bitbucket.org was accepted as an API base")
	}
	if !strings.Contains(err.Error(), config.BitbucketAPIBase) {
		t.Errorf("the error does not name the right value: %v", err)
	}
}

// TestAuthModeIsNotInferred. A vault mid-setup holds a subset of everything, so a
// client that picked a mode from which keys happen to be present would silently choose
// the credential with the wider blast radius.
func TestAuthModeIsNotInferred(t *testing.T) {
	dir := t.TempDir()
	v, err := vault.OpenWithKey(dir+"/vault.age", "a-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	// Every credential present, both modes satisfiable.
	for k, val := range map[string]string{
		vault.KeyBitbucketHookSec:     testSecret,
		vault.KeyBitbucketAccessToken: "an-access-token",
		vault.KeyBitbucketEmail:       "someone@example.com",
		vault.KeyBitbucketAPIToken:    "an-api-token",
	} {
		if err := v.Set(k, vault.NewSecretString(val)); err != nil {
			t.Fatal(err)
		}
	}

	// api_token asked for explicitly: the email is the clone username, which is how
	// the mode is observable from outside.
	c, err := New(config.BitbucketConfig{
		Enabled: true, APIBase: "https://api.bitbucket.org", Auth: config.BitbucketAuthAPIToken,
	}, v, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	url, err := c.CloneURL(t.Context(), model.PullRequest{
		Repo: model.Repo{Owner: "acme", Name: "docs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "someone%40example.com") {
		t.Errorf("api_token mode did not use the email as the clone username, "+
			"or did not escape it: %s", url)
	}

	// An unknown mode is refused rather than defaulted, so a typo in the config is
	// not silently the wider credential.
	if _, err := New(config.BitbucketConfig{
		Enabled: true, APIBase: "https://api.bitbucket.org", Auth: "apitoken",
	}, v, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("an unknown bitbucket.auth was accepted")
	}
}

// TestFindCommentIgnoresDeletedAndPending.
//
// The list endpoint returns soft-deleted comments with their bodies intact, so a
// marker match against one produces an update to a comment nobody can see — the
// preview reports success and shows nothing. Named in the design doc as a "must not"
// with a test, and this is it.
func TestFindCommentIgnoresDeletedAndPending(t *testing.T) {
	const previewID = "a3f22ffd2ef8"
	marker := scm.MarkerFor(previewID, scm.MarkerLinkRef)

	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"values": []map[string]any{
				{"id": 1, "deleted": true, "content": map[string]string{"raw": marker + "\ndeleted"}},
				{"id": 2, "pending": true, "content": map[string]string{"raw": marker + "\npending"}},
				{"id": 3, "content": map[string]string{"raw": marker + "\nthe live one"}},
			},
		})
	}))

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformBitbucket, Owner: "netfoundry", Name: "docs"},
		Number: 20,
	}
	got, err := c.findComment(t.Context(), pr, previewID)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("findComment = %d, want 3 — a deleted or pending comment was matched", got)
	}
}

// TestChangedFilesPaginatesAndHandlesRenames.
//
// pagelen defaults to 500, so the pagination loop almost never runs a second time in
// production, which means it is almost never exercised. Tested against two short
// pages rather than trusted.
func TestChangedFilesPaginatesAndHandlesRenames(t *testing.T) {
	var srv *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"size": 4,
				"next": srv.URL + r.URL.Path + "?page=2",
				"values": []map[string]any{
					// Modified: one path, not two.
					{"status": "modified",
						"old": map[string]string{"path": "docs/a.md"},
						"new": map[string]string{"path": "docs/a.md"}},
					// Renamed: both paths, because a file moved out of docs/ is still
					// a documentation change.
					{"status": "renamed",
						"old": map[string]string{"path": "docs/old.md"},
						"new": map[string]string{"path": "guide/new.md"}},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"size": 4,
			"values": []map[string]any{
				// Added: no `old`. A naive entry.Old.Path panics here.
				{"status": "added", "new": map[string]string{"path": "docs/added.md"}},
				// Removed: no `new`.
				{"status": "removed", "old": map[string]string{"path": "docs/gone.md"}},
			},
		})
	})

	c, s := newTestClient(t, handler)
	srv = s

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformBitbucket, Owner: "netfoundry", Name: "docs"},
		Number: 20,
	}
	files, err := c.ChangedFiles(t.Context(), pr)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"docs/a.md": true, "guide/new.md": true, "docs/old.md": true,
		"docs/added.md": true, "docs/gone.md": true,
	}
	if len(files) != len(want) {
		t.Fatalf("files = %v, want %d entries", files, len(want))
	}
	for _, f := range files {
		if !want[f] {
			t.Errorf("unexpected path %q", f)
		}
	}
}

// TestChangedFilesRefusesAnOffHostNextLink. Following a server-supplied URL blindly is
// how an API response becomes a request to wherever it likes.
func TestChangedFilesRefusesAnOffHostNextLink(t *testing.T) {
	var reached int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		_ = json.NewEncoder(w).Encode(map[string]any{"values": []map[string]any{}})
	}))
	defer elsewhere.Close()

	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"next": elsewhere.URL + "/2.0/whatever",
			"values": []map[string]any{
				{"status": "modified", "new": map[string]string{"path": "docs/a.md"}},
			},
		})
	}))

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformBitbucket, Owner: "netfoundry", Name: "docs"},
		Number: 20,
	}
	if _, err := c.ChangedFiles(t.Context(), pr); err != nil {
		t.Fatal(err)
	}
	if reached != 0 {
		t.Errorf("followed a next link to another host %d time(s)", reached)
	}
}

// TestBuildStatusIsSkippedWithNoURL. Bitbucket requires `url`, so this cannot be
// called at all before the preview has an address — and a placeholder would be worse
// than silence.
func TestBuildStatusIsSkippedWithNoURL(t *testing.T) {
	var posts int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.WriteHeader(http.StatusOK)
	}))

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformBitbucket, Owner: "netfoundry", Name: "docs"},
		Number: 20,
	}
	err := c.postBuildStatus(t.Context(), scm.Report{
		PR: pr, State: scm.StateBuilding, Commit: "a4fd6c9db1940992c8af5c48401462100bd7d2f1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Errorf("posted %d build statuses with no URL to point at", posts)
	}
}

// TestErrorEnvelopeIsRead. Bitbucket's shape is neither GitHub's nor a bare string, so
// this is written fresh — and an error whose message is dropped is a support
// conversation.
func TestErrorEnvelopeIsRead(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"type":"error","error":{"message":"Your credentials lack one or more required privilege scopes.","detail":"pullrequest:write"}}`)
	}))

	err := c.do(t.Context(), http.MethodGet, "/2.0/user", nil, nil)
	if err == nil {
		t.Fatal("a 403 was not an error")
	}
	if !strings.Contains(err.Error(), "required privilege scopes") ||
		!strings.Contains(err.Error(), "pullrequest:write") {
		t.Errorf("the message and detail did not survive: %v", err)
	}
}
