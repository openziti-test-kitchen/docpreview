package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/scm/github"
	"github.com/netfoundry/docpreview/internal/scm/local"
	"github.com/netfoundry/docpreview/internal/store"
)

// fakeClient is a scm.Client that records what it was asked to do.
type fakeClient struct {
	mu sync.Mutex

	verifyErr error
	events    []scm.Event
	reports   []scm.Report
	retracted []model.PullRequest

	// openPRs is what OpenPullRequests answers. Implementing the optional
	// scm.PullRequestLister here rather than in a second fake, because the daemon
	// discovers the capability by type assertion — a fake that lacked it would make the
	// scan and link paths silently untestable rather than failing.
	openPRs []model.PullRequest

	// What scm.BranchResolver answers. `master` rather than `main` in the tests that use
	// it, so an implementation that assumed the name instead of asking would fail.
	defaultBranch string
	branchTip     string
}

func (f *fakeClient) Platform() model.Platform { return model.PlatformGitHub }

func (f *fakeClient) VerifyWebhook(context.Context, map[string][]string, []byte) ([]scm.Event, error) {
	if f.verifyErr != nil {
		return nil, f.verifyErr
	}
	return f.events, nil
}

func (f *fakeClient) CloneURL(context.Context, model.PullRequest) (string, error) {
	return "https://example.invalid/repo.git", nil
}

func (f *fakeClient) ChangedFiles(context.Context, model.PullRequest) ([]string, error) {
	return []string{"docs/intro.md"}, nil
}

func (f *fakeClient) Publish(_ context.Context, r scm.Report) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, r)
	return nil
}

func (f *fakeClient) Retract(_ context.Context, pr model.PullRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retracted = append(f.retracted, pr)
	return nil
}

func (f *fakeClient) OpenPullRequests(context.Context, model.Repo) ([]model.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openPRs, nil
}

func (f *fakeClient) DefaultBranch(context.Context, model.Repo) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.defaultBranch, f.branchTip, nil
}

func (f *fakeClient) BranchTip(_ context.Context, _ model.Repo, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.branchTip, nil
}

func (f *fakeClient) reportStates() []scm.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]scm.State, len(f.reports))
	for i, r := range f.reports {
		out[i] = r.State
	}
	return out
}

func testIngress(t *testing.T, client *fakeClient) (*Ingress, *Daemon, *store.Store) {
	t.Helper()

	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.Exposer.Kind = "local"

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.DiscardHandler)
	ex := expose.NewLocal(log, "")
	t.Cleanup(func() { ex.Close() })

	clients := map[model.Platform]scm.Client{model.PlatformGitHub: client}
	d := New(cfg, st, ex, clients, log)
	// No docker in tests. Teardown removes the docker cache volumes, and the real call is a
	// `docker volume rm` subprocess per torn-down preview, too slow to run for every test in this suite.
	d.removeCacheVolumes = func(context.Context, string) error { return nil }
	ingress := NewIngress(d, clients, st, log)

	// The local exposer serves previews as paths on the daemon's own listener,
	// so a test that fetches a preview URL needs that listener to exist. The
	// origin is only knowable once it has bound.
	srv := httptest.NewServer(ingress.Handler())
	t.Cleanup(srv.Close)
	ex.SetOrigin(srv.URL)

	return ingress, d, st
}

func post(t *testing.T, h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	return rec
}

func TestIngressRejectsUnverifiedWebhook(t *testing.T) {
	// A forged pull_request payload is a request to clone and build a
	// repository of the attacker's choosing. The endpoint is internet-facing
	// by design, so this is the boundary that matters.
	client := &fakeClient{verifyErr: github.ErrBadSignature}
	ingress, _, _ := testIngress(t, client)

	rec := post(t, ingress.Handler(), "/webhook/github", []byte(`{"action":"opened"}`))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// The response must not explain what was wrong with the signature.
	if strings.Contains(strings.ToLower(rec.Body.String()), "signature") {
		t.Errorf("the 401 body leaks why verification failed: %q", rec.Body.String())
	}
}

func TestIngressRejectsUnverifiedWebhookOnEveryPlatform(t *testing.T) {
	// scm.ErrBadSignature is shared across platforms, and every route must honour it. A platform-specific
	// sentinel would let a bad signature on some other route answer 400, which tells a caller guessing at
	// the secret that its guess was structurally fine instead of distinguishing a wrong secret from a
	// malformed body.
	for _, route := range []string{"/webhook/github", "/webhook/bitbucket", "/webhook/local"} {
		t.Run(route, func(t *testing.T) {
			client := &fakeClient{verifyErr: scm.ErrBadSignature}
			ingress, _, _ := testIngress(t, client)
			// testIngress registers the client under GitHub, so point every
			// route at it: what is under test is the status code the ingress
			// derives from the error, not the routing.
			ingress.SetClient(model.PlatformBitbucket, client)
			ingress.SetClient(model.PlatformLocal, client)

			rec := post(t, ingress.Handler(), route, []byte(`{"action":"opened"}`))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestSetupPageIsServedAtItsOwnPath(t *testing.T) {
	// /secrets exists so credential management has an address of its own — one a
	// runbook can link and a proxy can gate. It serves the same document as "/",
	// switched client-side on pathname, so the only thing to assert here is that
	// the route exists and only when a secrets admin is wired.
	bare, d, _ := testIngress(t, &fakeClient{})

	// Without an admin the route is absent rather than empty: a daemon with no
	// credential management should not advertise a page for it.
	rec := httptest.NewRecorder()
	bare.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/secrets", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /secrets with no secrets admin = %d, want 404", rec.Code)
	}

	cfg := config.DefaultServer()
	cfg.DataDir = t.TempDir()
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}
	admin := NewSecretsAdmin(cfg, slog.New(slog.DiscardHandler), func(string) {})

	withSecrets := NewIngress(d, nil, nil, slog.New(slog.DiscardHandler)).WithSecrets(admin)
	for _, path := range []string{"/secrets", "/secrets/"} {
		rec := httptest.NewRecorder()
		withSecrets.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestLocalClientReturnsTheSharedBadSignatureError(t *testing.T) {
	// The other half: the ingress can only answer 401 if the client says so, and
	// the local client returned a bare fmt.Errorf.
	dir := t.TempDir()
	c, err := local.New(config.LocalSCMConfig{
		Enabled:       true,
		ReposDir:      dir,
		WebhookSecret: "a-webhook-secret",
	}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256=deadbeef")
	_, err = c.VerifyWebhook(context.Background(), h, []byte(`{"repo":"x","number":1}`))

	if !errors.Is(err, scm.ErrBadSignature) {
		t.Fatalf("got %v, want scm.ErrBadSignature so the ingress can answer 401", err)
	}
}

func TestIngressAcceptsVerifiedWebhookImmediately(t *testing.T) {
	// GitHub gives a webhook ten seconds before marking the delivery failed.
	// The response must not wait on the work.
	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: "abc123",
	}
	client := &fakeClient{events: []scm.Event{{Kind: scm.EventBuild, PR: pr, Delivery: "d1"}}}
	ingress, _, st := testIngress(t, client)

	rec := post(t, ingress.Handler(), "/webhook/github", []byte(`{}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	// The event is handled asynchronously; wait for it to land in the queue.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := st.PendingCount(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the accepted webhook never reached the queue")
}

func TestIngressRejectsOversizedBody(t *testing.T) {
	client := &fakeClient{}
	ingress, _, _ := testIngress(t, client)

	rec := post(t, ingress.Handler(), "/webhook/github", bytes.Repeat([]byte("a"), maxWebhookBody+1))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestIngressBitbucketNotConfigured(t *testing.T) {
	client := &fakeClient{}
	ingress, _, _ := testIngress(t, client)

	rec := post(t, ingress.Handler(), "/webhook/bitbucket", []byte(`{}`))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestIngressHealthz(t *testing.T) {
	ingress, _, _ := testIngress(t, &fakeClient{})

	rec := httptest.NewRecorder()
	ingress.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestIngressStatus(t *testing.T) {
	ingress, _, st := testIngress(t, &fakeClient{})

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 42, Branch: "feature/x",
	}
	if err := st.SavePreview(context.Background(), store.Preview{
		PreviewID: pr.PreviewID(), PR: pr, Name: "feature-x",
		URL: "https://feature-x.share.zrok.io/", State: scm.StateReady, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	ingress.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var out Status
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding status: %v\n%s", err, rec.Body.String())
	}
	if out.Exposer != "local" {
		t.Errorf("exposer = %q", out.Exposer)
	}
	if len(out.Previews) != 1 {
		t.Fatalf("got %d previews, want 1", len(out.Previews))
	}
	if out.Previews[0].Branch != "feature/x" {
		t.Errorf("branch = %q", out.Previews[0].Branch)
	}
}

func TestHandleBuildReportsQueuedAndEnqueues(t *testing.T) {
	// The queued report is what tells a reviewer their push was seen, before
	// the build has produced anything.
	client := &fakeClient{}
	_, d, st := testIngress(t, client)

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: "abc123",
	}
	if err := d.Handle(context.Background(), scm.Event{Kind: scm.EventBuild, PR: pr}); err != nil {
		t.Fatal(err)
	}

	n, err := st.PendingCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pending = %d, want 1", n)
	}

	// Reports are debounced, so the write lands after a short delay rather than
	// inside Handle. Polling rather than sleeping the full window keeps the test
	// fast when it passes and still fails when nothing ever arrives.
	waitForStates(t, client, scm.StateQueued)
}

// waitForStates waits for the client to have received exactly want.
func waitForStates(t *testing.T, client *fakeClient, want ...scm.State) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var states []scm.State
	for time.Now().Before(deadline) {
		states = client.reportStates()
		if len(states) == len(want) {
			match := true
			for i := range want {
				if states[i] != want[i] {
					match = false
					break
				}
			}
			if match {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("reports = %v, want %v", states, want)
}

func TestHandleTeardownRetractsTheComment(t *testing.T) {
	client := &fakeClient{}
	_, d, st := testIngress(t, client)
	ctx := context.Background()

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 42, Branch: "feature/x",
	}
	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: pr.PreviewID(), PR: pr, State: scm.StateReady, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := d.Handle(ctx, scm.Event{Kind: scm.EventTeardown, PR: pr}); err != nil {
		t.Fatal(err)
	}

	client.mu.Lock()
	retracted := len(client.retracted)
	client.mu.Unlock()
	if retracted != 1 {
		t.Errorf("retracted %d comments, want 1", retracted)
	}

	all, err := st.ListPreviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("%d previews remain after teardown, want 0", len(all))
	}
}

func TestHandleRejectsUnknownEventKind(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})
	err := d.Handle(context.Background(), scm.Event{Kind: "nonsense"})
	if err == nil {
		t.Fatal("an unknown event kind was accepted")
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("error does not name the kind: %v", err)
	}
}

func TestDashboardIsServed(t *testing.T) {
	ingress, _, _ := testIngress(t, &fakeClient{})

	rec := get(t, ingress.Handler(), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	// Markup only the current layout has. go:embed guarantees the embedded bytes are present; this checks
	// they are the right bytes, which a stale dashboard file would not be.
	for _, want := range []string{
		`<div class="seg" id="counters">`,
		`<div class="feed-filters" id="feed-filters">`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the dashboard is missing %q", want)
		}
	}
}

func TestV2RedirectsToTheDashboard(t *testing.T) {
	// /v2 must redirect to /: the path is in browser histories and bookmarks.
	ingress, _, _ := testIngress(t, &fakeClient{})

	rec := get(t, ingress.Handler(), "/v2")
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
}

var _ scm.Client = (*fakeClient)(nil)
