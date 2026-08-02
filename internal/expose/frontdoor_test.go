package expose

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/vault"
)

// fakeFrontdoor is a stand-in controller that records what was asked of it.
type fakeFrontdoor struct {
	mu sync.Mutex

	// createResponse is the raw JSON returned from POST /shares. Empty means
	// "invent a distinct, well-formed share per call", which is what the tests
	// about lifecycle rather than wire format want: two previews sharing one
	// share id would collide in Reap's live-id set.
	createResponse string

	// createStatus and deleteStatus are per-attempt HTTP statuses, consumed in
	// order, for the retry tests. A short list followed by more requests falls
	// back to the success path, which is how "fail once then succeed" is spelled.
	createStatus []int
	deleteStatus []int

	// listPages are the raw bodies GET /shares returns, indexed by the requested
	// page. Beyond the end of the slice the listing is empty, which is how paging
	// terminates. Nil means the empty single-page listing.
	listPages []string

	// createRaw holds every create body received, undecoded, so a test can assert
	// on the field names actually sent rather than on what a struct would accept.
	createRaw []string

	// listQueries records the raw query string of every listing request.
	listQueries []string

	deleted []string
	created int
}

func (f *fakeFrontdoor) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/shares":
			raw, _ := io.ReadAll(r.Body)
			f.createRaw = append(f.createRaw, string(raw))
			f.created++

			if len(f.createStatus) > 0 {
				code := f.createStatus[0]
				f.createStatus = f.createStatus[1:]
				w.WriteHeader(code)
				io.WriteString(w, `{"error":"deliberate"}`)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			if f.createResponse != "" {
				io.WriteString(w, f.createResponse)
				return
			}
			var body shareRequest
			json.Unmarshal(raw, &body)
			json.NewEncoder(w).Encode(map[string]any{
				"id":               "share-" + strconv.Itoa(f.created),
				"name":             body.Name,
				"frontendEndpoint": "https://" + body.Name + ".shares.netfoundry.io",
			})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/shares"):
			if len(f.deleteStatus) > 0 {
				code := f.deleteStatus[0]
				f.deleteStatus = f.deleteStatus[1:]
				w.WriteHeader(code)
				return
			}
			f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/shares/"))
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && r.URL.Path == "/shares":
			f.listQueries = append(f.listQueries, r.URL.RawQuery)
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page < len(f.listPages) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, f.listPages[page])
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"content": []any{}})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *fakeFrontdoor) deletions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *fakeFrontdoor) creates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.createRaw...)
}

func (f *fakeFrontdoor) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// noBackoff collapses the retry waits for the duration of one test.
//
// zrokBackoff is seconds rather than milliseconds on purpose — the failure being
// retried is a gateway that did not answer in time, so retrying immediately asks
// the same overloaded thing the same question — which makes an honest retry test
// eight seconds long. The variable is shared with the zrok exposer and package
// tests do not run concurrently with each other, so saving and restoring it is
// safe; t.Cleanup rather than a defer so a t.Fatal cannot leave it collapsed for
// the rest of the suite.
func noBackoff(t *testing.T) {
	t.Helper()
	saved := zrokBackoff
	zrokBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { zrokBackoff = saved })
}

// staticToken is a TokenFunc that always succeeds, for the tests that are not
// about credential resolution.
func staticToken(s string) TokenFunc {
	return func() (vault.Secret, error) { return vault.NewSecretString(s), nil }
}

func newTestFrontdoor(t *testing.T, fake *fakeFrontdoor) (*Frontdoor, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	fd, err := NewFrontdoor(config.FrontdoorConfig{
		APIBase:            srv.URL,
		Frontend:           "public",
		AgentReachableHost: "127.0.0.1",
		NameTemplate:       "{{.Name}}",
	}, staticToken("test-token"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fd.Close() })
	return fd, srv
}

func TestFrontdoorPublishRejectsAResponseWithWrongFieldNames(t *testing.T) {
	// The failure this guard exists for. Frontdoor's real payload might call
	// these shareId and publicUrl; encoding/json does not complain about fields
	// it never saw, so without the check the publish "succeeds" with an empty
	// ID and an empty URL, the pull request gets a link to "/", and the remote
	// share leaks because cleanup has no ID to delete.
	fake := &fakeFrontdoor{
		createResponse: `{"shareId":"abc123","publicUrl":"https://x.example.com/"}`,
	}
	fd, _ := newTestFrontdoor(t, fake)

	_, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"},
		http.NotFoundHandler())

	if err == nil {
		t.Fatal("Publish accepted a response whose field names did not match")
	}
	// The error has to name the fix, or it is just a mystery at a distance.
	for _, want := range []string{"no id or url", "shareResponse", "frontdoor.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestFrontdoorPublishAcceptsAGoodResponse(t *testing.T) {
	fake := &fakeFrontdoor{
		createResponse: `{"id":"abc123","url":"https://x.example.com"}`,
	}
	fd, _ := newTestFrontdoor(t, fake)

	pub, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/docs/"},
		http.NotFoundHandler())
	if err != nil {
		t.Fatalf("Publish rejected a well-formed response: %v", err)
	}
	if pub.URL != "https://x.example.com/docs/" {
		t.Errorf("URL = %q, want the base URL appended", pub.URL)
	}
}

func TestFrontdoorPublishDeletesAHalfCreatedShare(t *testing.T) {
	// An ID but no URL means the share exists remotely and is unusable. Leaving
	// it would be an orphan nothing can reach or reap.
	fake := &fakeFrontdoor{
		createResponse: `{"id":"abc123"}`,
	}
	fd, _ := newTestFrontdoor(t, fake)

	if _, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"},
		http.NotFoundHandler()); err == nil {
		t.Fatal("Publish accepted a response with no url")
	}

	deleted := fake.deletions()
	if len(deleted) != 1 || deleted[0] != "abc123" {
		t.Errorf("half-created share was not cleaned up: deletions = %v", deleted)
	}
}

func TestFrontdoorPublishReleasesThePortOnFailure(t *testing.T) {
	// A rejected publish must not leave a listener bound. Publishing twice and
	// checking the port moves is the observable proxy for that.
	fake := &fakeFrontdoor{createResponse: `{}`}
	fd, _ := newTestFrontdoor(t, fake)

	for range 3 {
		if _, err := fd.Publish(context.Background(),
			Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"},
			http.NotFoundHandler()); err == nil {
			t.Fatal("Publish accepted an empty response")
		}
	}

	fd.mu.Lock()
	live := len(fd.live)
	fd.mu.Unlock()
	if live != 0 {
		t.Errorf("%d failed publishes were recorded as live", live)
	}
}

func TestFrontdoorRefusesToDeleteAnEmptyID(t *testing.T) {
	// "DELETE /shares/" + "" addresses the collection. On an API where that
	// means "delete everything", this guard is the difference between a bad
	// log line and a very bad afternoon.
	fake := &fakeFrontdoor{createResponse: `{}`}
	fd, _ := newTestFrontdoor(t, fake)

	err := fd.deleteShare(context.Background(), "")
	if err == nil {
		t.Fatal("deleteShare accepted an empty id")
	}
	if !strings.Contains(err.Error(), "no id") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
	if got := fake.deletions(); len(got) != 0 {
		t.Errorf("a request was sent despite the empty id: %v", got)
	}
}

func TestFrontdoorConstructsWithAnUnreadableToken(t *testing.T) {
	// NewFrontdoor must not resolve the token itself: doing so at construction
	// would open the vault during wiring, and a locked vault would then refuse
	// the daemon start, hiding the page that unlocks it. Construction must
	// succeed with a provider that cannot answer yet.
	locked := func() (vault.Secret, error) {
		return vault.Secret{}, errors.New("vault is locked")
	}

	fd, err := NewFrontdoor(config.FrontdoorConfig{
		APIBase:            "https://frontdoor.example",
		Frontend:           "public",
		AgentReachableHost: "127.0.0.1",
		NameTemplate:       "{{.Name}}",
	}, locked, discardLogger())
	if err != nil {
		t.Fatalf("NewFrontdoor refused a locked vault: %v", err)
	}
	t.Cleanup(func() { fd.Close() })

	// It must still fail when it actually needs the credential, rather than
	// sending an unauthenticated request and misreporting the result.
	err = fd.Validate(context.Background())
	if err == nil {
		t.Fatal("Validate succeeded with no token")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("the locked vault is not named in the error: %v", err)
	}
}

func TestFrontdoorRejectsANilTokenProvider(t *testing.T) {
	// A nil provider is a wiring bug, and it has to surface at construction
	// rather than as a panic on the first publish.
	_, err := NewFrontdoor(config.FrontdoorConfig{
		APIBase:  "https://frontdoor.example",
		Frontend: "public",
	}, nil, discardLogger())
	if err == nil {
		t.Fatal("NewFrontdoor accepted a nil token provider")
	}
}

func TestFrontdoorReresolvesTheTokenPerRequest(t *testing.T) {
	// Rotating the credential from the setup page has to take effect without a
	// restart, which it cannot if the token is captured at construction.
	fake := &fakeFrontdoor{createResponse: `{}`}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	var calls int
	fd, err := NewFrontdoor(config.FrontdoorConfig{
		APIBase:            srv.URL,
		Frontend:           "public",
		AgentReachableHost: "127.0.0.1",
		NameTemplate:       "{{.Name}}",
	}, func() (vault.Secret, error) {
		calls++
		return vault.NewSecretString("token-" + strconv.Itoa(calls)), nil
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fd.Close() })

	_ = fd.Validate(context.Background())
	_ = fd.Validate(context.Background())

	if calls != 2 {
		t.Errorf("the token provider was called %d times for 2 requests, want 2", calls)
	}
}

// --- The wire format, against the documented one ---------------------------

func TestFrontdoorSendsTheDocumentedShareFields(t *testing.T) {
	// The create payload was written from inference and three of its fields were
	// wrong. The documented example is
	// https://netfoundry.io/docs/frontdoor/learn/shares/http-shares/:
	//
	//	{"type":"http","name":"publicdemo","envZId":"ijcrWb-ZOq",
	//	 "frontendIds":["bMTHPrtQ"],"target":"http://backend…:8080"}
	//
	// This pins each correction, and pins the absence of the old names — sending
	// `targetUrl` alongside `target` would look like it worked while the share
	// pointed nowhere.
	fake := &fakeFrontdoor{}
	fd, _ := newTestFrontdoor(t, fake)

	if _, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"},
		http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}

	sent := fake.creates()
	if len(sent) != 1 {
		t.Fatalf("got %d create requests, want 1", len(sent))
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(sent[0]), &body); err != nil {
		t.Fatal(err)
	}

	if body["type"] != "http" {
		t.Errorf(`type = %v, want "http"`, body["type"])
	}
	if body["name"] != "my-branch" {
		t.Errorf("name = %v, want the spec's name", body["name"])
	}
	// The frontend is an array of IDs, not a single name.
	ids, ok := body["frontendIds"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "public" {
		t.Errorf("frontendIds = %v, want the configured frontend as a one-element array", body["frontendIds"])
	}
	target, _ := body["target"].(string)
	if !strings.HasPrefix(target, "http://127.0.0.1:") {
		t.Errorf("target = %q, want the bound loopback port", target)
	}

	// The names that were wrong must be gone, not merely joined by the right ones.
	for _, gone := range []string{"targetUrl", "frontend"} {
		if _, present := body[gone]; present {
			t.Errorf("the create payload still carries the pre-documentation field %q", gone)
		}
	}
}

func TestFrontdoorReadsTheDocumentedResponseFields(t *testing.T) {
	// The share object is documented to carry `frontendEndpoint` for its public
	// address, and its field list shows `zId` where every documented path takes an
	// `{id}`. Both are decoded, so a response using either spelling publishes
	// rather than failing the id-or-url guard.
	fake := &fakeFrontdoor{
		createResponse: `{"zId":"abc123","frontendEndpoint":"https://my-branch.shares.netfoundry.io"}`,
	}
	fd, _ := newTestFrontdoor(t, fake)

	pub, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/docs/"},
		http.NotFoundHandler())
	if err != nil {
		t.Fatalf("Publish rejected the documented response shape: %v", err)
	}
	if pub.URL != "https://my-branch.shares.netfoundry.io/docs/" {
		t.Errorf("URL = %q, want frontendEndpoint with the base URL appended", pub.URL)
	}

	// And the zId has to be what cleanup addresses, or the share leaks.
	if err := pub.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fake.deletions(); len(got) != 1 || got[0] != "abc123" {
		t.Errorf("deletions = %v, want the zId from the response", got)
	}
}

func TestFrontdoorGivesABareHostnameAScheme(t *testing.T) {
	// zrok's controller reports "name.share.zrok.io" rather than a URL, and without
	// a scheme the link is relative, so GitHub resolves it against github.com and
	// 404s there. Nobody has seen what Frontdoor puts in frontendEndpoint, so it
	// is defended against here too.
	fake := &fakeFrontdoor{
		createResponse: `{"id":"abc123","frontendEndpoint":"my-branch.shares.netfoundry.io"}`,
	}
	fd, _ := newTestFrontdoor(t, fake)

	pub, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"},
		http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if pub.URL != "https://my-branch.shares.netfoundry.io/" {
		t.Errorf("URL = %q, want a scheme added to the bare hostname", pub.URL)
	}
}

func TestFrontdoorRejectsAnAPIBaseWithNoFrontdoorID(t *testing.T) {
	// Frontdoor's routes are scoped to a frontdoor instance —
	// POST /frontdoor/{frontdoorId}/shares — and config's default api_base stops
	// at "/frontdoor", so every request would 404 and the operator would be
	// debugging a path rather than a setting.
	_, err := NewFrontdoor(config.FrontdoorConfig{
		APIBase:  "https://gateway.production.netfoundry.io/frontdoor",
		Frontend: "public",
	}, staticToken("t"), discardLogger())
	if err == nil {
		t.Fatal("NewFrontdoor accepted an api_base with no frontdoor ID")
	}
	// The error has to carry the shape of the fix, not just the complaint.
	for _, want := range []string{"frontdoorId", "api_base"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	// And an api_base that does carry one is accepted.
	fd, err := NewFrontdoor(config.FrontdoorConfig{
		APIBase:  "https://gateway.production.netfoundry.io/frontdoor/2xY9",
		Frontend: "public",
	}, staticToken("t"), discardLogger())
	if err != nil {
		t.Fatalf("NewFrontdoor refused an api_base with a frontdoor ID: %v", err)
	}
	t.Cleanup(func() { fd.Close() })
}

// --- The share listing, whose wrong shape would otherwise fail silently -----

func TestDecodeShareListingAcceptsEveryPlausibleEnvelope(t *testing.T) {
	// One of these is right and nobody knows which. The documented answer for
	// `Accept: application/json` is Spring's "content"; hal+json moves it to
	// `_embedded.shareList`. The rest are cheap insurance.
	for _, tc := range []struct{ name, body string }{
		{"spring content", `{"content":[{"id":"a"}],"totalPages":1}`},
		{"hal shareList", `{"_embedded":{"shareList":[{"id":"a"}]},"_links":{}}`},
		{"hal shares", `{"_embedded":{"shares":[{"id":"a"}]}}`},
		{"bare array", `[{"id":"a"}]`},
		{"named collection", `{"shares":[{"id":"a"}]}`},
		{"data envelope", `{"data":[{"id":"a"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeShareListing([]byte(tc.body))
			if err != nil {
				t.Fatalf("decodeShareListing: %v", err)
			}
			if len(got) != 1 || got[0].ID() != "a" {
				t.Errorf("decoded %+v, want one share with id \"a\"", got)
			}
		})
	}
}

func TestDecodeShareListingTreatsAnEmptyObjectAsNoShares(t *testing.T) {
	// A tenant with no shares is the ordinary state on day one, and failing
	// Validate for a correctly configured tenant would be a worse trade than the
	// one the strictness below is making.
	for _, body := range []string{`{}`, `{"_embedded":{}}`, `[]`, `{"content":[]}`} {
		got, err := decodeShareListing([]byte(body))
		if err != nil {
			t.Errorf("decodeShareListing(%s) failed: %v", body, err)
		}
		if len(got) != 0 {
			t.Errorf("decodeShareListing(%s) = %+v, want no shares", body, got)
		}
	}
}

func TestDecodeShareListingRefusesAnUnrecognisedEnvelope(t *testing.T) {
	// This is the failure the guard exists for, and it is worse than the create
	// path's because it is silent: a collection under a key this code does not
	// know decodes to zero shares and reports success, so Reap finds nothing to
	// reap on every restart while every restart creates a fresh share per
	// preview. The tenant fills up and the first symptom is a quota refusal
	// months later.
	_, err := decodeShareListing([]byte(`{"shareCollection":[{"id":"a"}],"total":1}`))
	if err == nil {
		t.Fatal("decodeShareListing accepted an envelope it could not read")
	}
	// It must name what it saw and where to fix it, or it is a mystery.
	for _, want := range []string{"shareCollection", "shareCollectionKeys", "frontdoor.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestFrontdoorListSharesRefusesAListingWithNoIDs(t *testing.T) {
	// Shares that decode with no id are shares Reap cannot delete. Every share has
	// one, so all of them missing it means shareResponse's field names are wrong.
	fake := &fakeFrontdoor{
		listPages: []string{`{"content":[{"shareId":"a"},{"shareId":"b"}]}`},
	}
	fd, _ := newTestFrontdoor(t, fake)

	_, err := fd.listShares(context.Background())
	if err == nil {
		t.Fatal("listShares accepted a listing in which no share had an id")
	}
	if !strings.Contains(err.Error(), "shareResponse") {
		t.Errorf("error does not name the struct to fix: %v", err)
	}
}

// --- Reap -------------------------------------------------------------------

func TestFrontdoorReapDeletesOnlyTaggedOrphans(t *testing.T) {
	// The test docs/design/17-exposer-frontdoor.md asks for first, because writing
	// it is what makes somebody say out loud what `keep` means. Four shares and
	// exactly one is doomed: the untagged ones belong to an operator who made them
	// by hand in another terminal, and deleting those would be rude.
	fake := &fakeFrontdoor{
		listPages: []string{`{"content":[
			{"id":"keep-me",  "tag":"docpreview:p1"},
			{"id":"orphan",   "tag":"docpreview:p2"},
			{"id":"not-ours", "tag":"someone-elses-thing"},
			{"id":"untagged"}
		]}`},
	}
	fd, _ := newTestFrontdoor(t, fake)

	if err := fd.Reap(context.Background(), map[string]bool{"p1": true}); err != nil {
		t.Fatal(err)
	}
	if got := fake.deletions(); len(got) != 1 || got[0] != "orphan" {
		t.Errorf("deletions = %v, want exactly [orphan]", got)
	}
}

func TestFrontdoorReapKeepsABuildShareOfAKeptPreview(t *testing.T) {
	// A build share's tag is "docpreview:<preview>/<build>", and keep carries that
	// same key. Trimming the prefix and matching the whole remainder is what makes
	// a kept build survive; matching the preview id alone would keep every build
	// ever made, and matching only the part before the slash would delete them all.
	fake := &fakeFrontdoor{
		listPages: []string{`{"content":[
			{"id":"branch", "tag":"docpreview:p1"},
			{"id":"b-kept", "tag":"docpreview:p1/build-7"},
			{"id":"b-gone", "tag":"docpreview:p1/build-3"}
		]}`},
	}
	fd, _ := newTestFrontdoor(t, fake)

	keep := map[string]bool{"p1": true, "p1/build-7": true}
	if err := fd.Reap(context.Background(), keep); err != nil {
		t.Fatal(err)
	}
	if got := fake.deletions(); len(got) != 1 || got[0] != "b-gone" {
		t.Errorf("deletions = %v, want exactly [b-gone]", got)
	}
}

func TestFrontdoorReapDoesNotDeleteAShareThisProcessIsServing(t *testing.T) {
	// The hourly reap runs while previews are live, and a preview whose database
	// row has just gone is not in keep. Deleting it here would tear down a share
	// this process is still serving on a local port, leaving a row that says
	// `ready` and a URL that answers nothing.
	fake := &fakeFrontdoor{}
	fd, _ := newTestFrontdoor(t, fake)

	if _, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "live-one", BaseURL: "/"},
		http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}

	// The listing reports it, tagged, and keep does not mention it.
	fake.mu.Lock()
	fake.listPages = []string{`{"content":[{"id":"share-1","tag":"docpreview:p1"}]}`}
	fake.mu.Unlock()

	if err := fd.Reap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := fake.deletions(); len(got) != 0 {
		t.Errorf("Reap deleted a share this process is serving: %v", got)
	}
}

func TestFrontdoorReapPagesThroughTheListing(t *testing.T) {
	// The listing is paginated and the documented default page size is 20. This
	// asked for no page at all, so an orphan past the first page was invisible —
	// and invisible to Reap means permanent. Frontdoor reaches twenty shares at
	// the second pull request, because a preview holds one share per kept build.
	fake := &fakeFrontdoor{
		listPages: []string{
			`{"content":[{"id":"page0","tag":"docpreview:p1"}]}`,
			`{"content":[{"id":"page1","tag":"docpreview:p2"}]}`,
		},
	}
	fd, _ := newTestFrontdoor(t, fake)

	if err := fd.Reap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	deleted := fake.deletions()
	if len(deleted) != 2 {
		t.Fatalf("deletions = %v, want both pages reaped", deleted)
	}

	// And the page parameter has to actually be on the wire, or the second page
	// would only have been reached by luck.
	fake.mu.Lock()
	queries := append([]string(nil), fake.listQueries...)
	fake.mu.Unlock()
	if len(queries) < 2 || !strings.Contains(queries[1], "page=1") {
		t.Errorf("listing queries = %v, want the second to ask for page=1", queries)
	}
}

func TestFrontdoorReapReportsATagThatDidNotRoundTrip(t *testing.T) {
	// No tag field is documented on a Frontdoor share, so `tag` is sent hopefully
	// and probably dropped. That fails safe — Reap deletes nothing rather than the
	// wrong thing — but it means orphans are never collected, and a silent leak is
	// what this whole file is written to avoid.
	//
	// A share *this process created* that comes back without its tag is proof, not
	// suspicion: we know what we sent. That is the one case worth an error log, and
	// it must not be mistaken for somebody else's share and skipped quietly.
	fake := &fakeFrontdoor{}
	fd, _ := newTestFrontdoor(t, fake)

	var logged strings.Builder
	fd.log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError}))

	if _, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "live-one", BaseURL: "/"},
		http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}

	// The gateway lists the share we just made, with no tag on it.
	fake.mu.Lock()
	fake.listPages = []string{`{"content":[{"id":"share-1","name":"live-one"}]}`}
	fake.mu.Unlock()

	if err := fd.Reap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := fake.deletions(); len(got) != 0 {
		t.Errorf("Reap deleted a live share: %v", got)
	}
	if !strings.Contains(logged.String(), "orphan detection is broken") {
		t.Errorf("a tag that did not round-trip was not reported: %q", logged.String())
	}
}

// --- Retry ------------------------------------------------------------------

func TestFrontdoorRetriesAGatewayFailure(t *testing.T) {
	// A publish that fails here fails the build, so a 503 from a load balancer
	// that never reached the thing behind it must not cost a documentation
	// preview without at least one retry.
	noBackoff(t)

	fake := &fakeFrontdoor{createStatus: []int{http.StatusServiceUnavailable}}
	fd, _ := newTestFrontdoor(t, fake)

	if _, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"},
		http.NotFoundHandler()); err != nil {
		t.Fatalf("Publish gave up on a retryable 503: %v", err)
	}
	if n := fake.attempts(); n != 2 {
		t.Errorf("%d create attempts, want a failure and a retry", n)
	}
}

func TestFrontdoorDoesNotRetryARefusal(t *testing.T) {
	// A 403 or a quota refusal retried three times is three times the same
	// refusal, plus two backoffs of startup the operator waits through before
	// being told what is wrong.
	noBackoff(t)

	fake := &fakeFrontdoor{createStatus: []int{http.StatusForbidden}}
	fd, _ := newTestFrontdoor(t, fake)

	if _, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"},
		http.NotFoundHandler()); err == nil {
		t.Fatal("Publish ignored a 403")
	}
	if n := fake.attempts(); n != 1 {
		t.Errorf("%d create attempts for a 403, want exactly 1", n)
	}
}

func TestFrontdoorDeleteTreatsA404OnARetryAsSuccess(t *testing.T) {
	// The deadline is this client's, not the gateway's, so a timed-out delete has
	// usually already happened. Measured against zrok: reporting those as failures
	// turned four successful deletions into four startup errors.
	noBackoff(t)

	fake := &fakeFrontdoor{
		deleteStatus: []int{http.StatusServiceUnavailable, http.StatusNotFound},
	}
	fd, _ := newTestFrontdoor(t, fake)

	if err := fd.deleteShare(context.Background(), "abc123"); err != nil {
		t.Errorf("a 404 after a retry was reported as a failure: %v", err)
	}
}

func TestFrontdoorDeleteReportsA404OnTheFirstAttempt(t *testing.T) {
	// The other half: a 404 straight away means the share was already gone when
	// the listing named it, which means something else is deleting docpreview's
	// shares. That is worth knowing rather than swallowing.
	fake := &fakeFrontdoor{deleteStatus: []int{http.StatusNotFound}}
	fd, _ := newTestFrontdoor(t, fake)

	if err := fd.deleteShare(context.Background(), "abc123"); err == nil {
		t.Fatal("a 404 on the first delete attempt was swallowed")
	}
}

func TestFrontdoorNamesTheTokenExchangeOnA401(t *testing.T) {
	// Frontdoor bearers come from an OAuth2 client-credentials exchange, so what
	// the vault holds is an access token with a lifetime. A daemon that outlives
	// it starts failing every publish with a 401 while nothing about the
	// configuration has changed, and "401 Unauthorized" sends the operator back to
	// the setup runbook instead of to a fresh token.
	fake := &fakeFrontdoor{}
	fd, _ := newTestFrontdoor(t, fake)

	// Validate lists rather than creates, so the 401 has to come from the listing.
	fake.mu.Lock()
	fake.listPages = nil
	fake.mu.Unlock()
	fd.http = &http.Client{Transport: statusTransport{http.StatusUnauthorized}}

	err := fd.Validate(context.Background())
	if err == nil {
		t.Fatal("Validate accepted a 401")
	}
	for _, want := range []string{"short-lived", "client-credentials", "frontdoor.api_token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the 401 error does not mention %q: %v", want, err)
		}
	}
}

// statusTransport answers every request with one status and an empty body.
type statusTransport struct{ code int }

func (s statusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: s.code,
		Status:     http.StatusText(s.code),
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
		Request:    r,
	}, nil
}

// --- Lifecycle --------------------------------------------------------------

func TestFrontdoorSurvivesTheDaemonsReplaceThenCloseOrder(t *testing.T) {
	// The daemon publishes the replacement and then closes the old Publication, in
	// that order. A close that deleted by key alone would delete the remote share
	// its own replacement had just created — so the preview would not merely 404,
	// it would stop existing on the tenant.
	fake := &fakeFrontdoor{}
	fd, _ := newTestFrontdoor(t, fake)

	spec := Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"}

	first, err := fd.Publish(context.Background(), spec, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fd.Publish(context.Background(), spec, http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}

	// Closing the superseded publication must not touch the live one.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	fd.mu.Lock()
	entry := fd.live[spec.Key()]
	fd.mu.Unlock()
	if entry == nil {
		t.Fatal("closing the superseded publication withdrew its replacement")
	}
	if entry.id != "share-2" {
		t.Errorf("live share is %q, want the second one", entry.id)
	}
	// One deletion, from the republish freeing the name — not two.
	if deleted := fake.deletions(); len(deleted) != 1 || deleted[0] != "share-1" {
		t.Errorf("deletions = %v, want only the first share", deleted)
	}
}

func TestFrontdoorNameCollisionKeepsTheRefusedPreviewsOwnShare(t *testing.T) {
	// Two previews rendering to one name is refused — a name_template that cannot
	// separate them is a configuration mistake, and quietly letting the second
	// take it would tear down a live preview somebody is reading.
	//
	// The refusal must happen before this publication's own share is withdrawn.
	// zrok withdraws first and can therefore refuse having already destroyed the
	// working preview it was replacing; under Frontdoor that deletes the share on
	// the tenant while the database row still says `ready`, so the pull request
	// comment goes on linking to a share that no longer exists.
	fake := &fakeFrontdoor{}
	fd, _ := newTestFrontdoor(t, fake)

	// p1 holds "shared-name".
	if _, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "shared-name", BaseURL: "/"},
		http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	// p2 has a working preview of its own under a different name.
	if _, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p2", Name: "its-own-name", BaseURL: "/"},
		http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}

	// p2 now rebuilds into the name p1 holds.
	_, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p2", Name: "shared-name", BaseURL: "/"},
		http.NotFoundHandler())
	if err == nil {
		t.Fatal("two previews were allowed to render to one name")
	}
	if !strings.Contains(err.Error(), "p1") {
		t.Errorf("the error does not name the preview holding the name: %v", err)
	}
	// The suggested template must differ from config.DefaultNameTemplate, or the
	// advice would be to change nothing.
	if !strings.Contains(err.Error(), "{{.Repo.Owner}}") {
		t.Errorf("the error suggests a template that is already the default: %v", err)
	}

	// Both previews keep the shares they had, and nothing was deleted.
	fd.mu.Lock()
	live := len(fd.live)
	fd.mu.Unlock()
	if live != 2 {
		t.Errorf("%d live publications after a refused publish, want 2", live)
	}
	if got := fake.deletions(); len(got) != 0 {
		t.Errorf("a refused publish deleted a share: %v", got)
	}
}

func TestFrontdoorTargetPortServesTheHandler(t *testing.T) {
	// The whole reason this exposer differs from zrok: the agent dials in, so the
	// target has to be an address that answers. Nothing else checks it — an agent
	// that cannot reach the port produces a green build and a dead link — and this
	// is the closest a test gets, while also pinning agent_reachable_host as the
	// bind address rather than only as the advertised one.
	fake := &fakeFrontdoor{}
	fd, _ := newTestFrontdoor(t, fake)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "the built site")
	})
	pub, err := fd.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"}, handler)
	if err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(fake.creates()[0]), &body); err != nil {
		t.Fatal(err)
	}
	target, _ := body["target"].(string)

	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("the target this exposer advertised is not dialable: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "the built site" {
		t.Errorf("the target served %q, want the published handler's response", got)
	}

	// And withdrawing has to give the port back, or a daemon leaks one listener
	// per rebuild. Checked by dialing rather than by inspecting `live`, which the
	// existing test does and which cannot tell a closed listener from a forgotten
	// one.
	if err := pub.Close(); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(target, "http://")
	if conn, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		conn.Close()
		t.Errorf("the preview port %s is still bound after the publication was closed", addr)
	}
}
