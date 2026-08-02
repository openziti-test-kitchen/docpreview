package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/vault"
)

// These tests stand a fake api.github.com in front of the client, injected
// through the existing api_base config field. They exist because the paths they
// cover — rate-limit classification and installation-token revocation — are
// reachable only from GitHub's own responses.

// apiFixture is a fake GitHub API and a client pointed at it.
type apiFixture struct {
	srv    *httptest.Server
	client *Client

	// tokenMints counts installation-token exchanges, which is how a test tells
	// a cache hit from a cache miss.
	tokenMints atomic.Int64

	// handler answers everything that is not the token endpoint. Set per test.
	handler func(w http.ResponseWriter, r *http.Request)
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()

	// 1024 bits: this key signs an App JWT that never leaves the fixture, and
	// 2048 costs noticeably more on every test in the file.
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}

	f := &apiFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The token exchange is the one endpoint every test needs, so the
		// fixture owns it rather than making each test reimplement it.
		if r.URL.Path == "/app/installations/999/access_tokens" {
			f.tokenMints.Add(1)
			w.Header().Set("Content-Type", "application/json")
			// 201, which is what GitHub returns and what the client requires.
			w.WriteHeader(http.StatusCreated)
			// A far-future expiry, so nothing in these tests refreshes on the
			// clock. Revocation, not expiry, is what is under test.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      fmt.Sprintf("ghs_minted_%d", f.tokenMints.Load()),
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			return
		}
		if f.handler != nil {
			f.handler(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(f.srv.Close)

	cfg := config.GitHubConfig{AppID: 4420399, APIBase: f.srv.URL}
	auth, err := newAuthenticator(cfg.AppID, cfg.APIBase, testPEM(t, key), f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.client = &Client{
		cfg:           cfg,
		log:           slog.New(slog.DiscardHandler),
		auth:          auth,
		http:          f.srv.Client(),
		webhookSecret: vault.NewSecretString(testSecret),
	}
	return f
}

func testPEM(t *testing.T, key *rsa.PrivateKey) vault.Secret {
	t.Helper()
	return vault.NewSecretString(string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})))
}

func TestRateLimitResetIsEpochSeconds(t *testing.T) {
	// X-RateLimit-Reset is a Unix timestamp, not RFC3339: parsed as RFC3339 it
	// never matches, so RetryAfter would always be zero and any caller acting
	// on it would retry straight back into the same limit.
	f := newAPIFixture(t)
	reset := time.Now().Add(90 * time.Second).Unix()
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}

	// doOnce, not do: classification is what is under test here, and do would
	// honour the 90-second wait it correctly derives.
	err := f.client.doOnce(context.Background(), 999, http.MethodGet, "/repos/acme/docs", nil, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want an *APIError", err)
	}
	if !apiErr.RateLimited {
		t.Error("a 403 with zero remaining was not classified as rate limited")
	}
	if apiErr.RetryAfter < 60*time.Second || apiErr.RetryAfter > 90*time.Second {
		t.Errorf("RetryAfter = %s, want roughly 90s from the epoch reset", apiErr.RetryAfter)
	}
	if !apiErr.Retryable() {
		t.Error("Retryable() is false for a rate-limited response")
	}
}

func TestSecondaryRateLimitIsRetryable(t *testing.T) {
	// The secondary limit fires on burst rather than volume, which makes it the
	// one a supersede storm hits. It is a 403 with a NON-zero remaining count,
	// so the remaining-count test alone classified it as a permissions error —
	// turning a wait-and-retry into a failed build and a comment stuck on
	// "Building".
	f := newAPIFixture(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4823")
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}

	err := f.client.doOnce(context.Background(), 999, http.MethodGet, "/repos/acme/docs", nil, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want an *APIError", err)
	}
	if !apiErr.RateLimited {
		t.Fatal("a secondary rate limit was not classified as rate limited")
	}
	if apiErr.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %s, want 45s from Retry-After", apiErr.RetryAfter)
	}
}

func TestSecondaryRateLimitRecognisedWithoutRetryAfter(t *testing.T) {
	// GitHub documents sending Retry-After, so the message match is the
	// fallback. Without it, a burst rejection reads as a permissions failure.
	f := newAPIFixture(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4823")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit. Please wait."}`))
	}

	err := f.client.doOnce(context.Background(), 999, http.MethodGet, "/repos/acme/docs", nil, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.RateLimited {
		t.Fatalf("got %v, want a rate-limited *APIError", err)
	}
	// No header to derive a wait from. Zero must mean "no idea" rather than
	// "retry now", which is the caller's problem to honour.
	if apiErr.RetryAfter != 0 {
		t.Errorf("RetryAfter = %s, want 0 when no header says", apiErr.RetryAfter)
	}
}

func TestGenuinePermissionErrorIsNotRetryable(t *testing.T) {
	// The reason the classification has to be narrow. Retrying a real
	// permissions failure spins until the build times out.
	f := newAPIFixture(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}

	err := f.client.do(context.Background(), 999, http.MethodGet, "/repos/acme/docs", nil, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want an *APIError", err)
	}
	if apiErr.RateLimited || apiErr.Retryable() {
		t.Error("a permissions failure was classified as retryable")
	}
}

func TestRateLimitIsRetriedAndSucceeds(t *testing.T) {
	// Without retrying, a burst rejection goes straight to a failed build.
	//
	// Retry-After: 0 so the test does not sleep. Zero is a legal delay-seconds
	// value and exercises the same path a real wait would.
	f := newAPIFixture(t)
	var calls atomic.Int64
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}

	var out struct{ OK bool }
	if err := f.client.do(context.Background(), 999, http.MethodGet, "/repos/acme/docs", nil, &out); err != nil {
		t.Fatalf("a rate limit was not retried: %v", err)
	}
	if !out.OK {
		t.Error("the retried response was not decoded")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("%d calls, want 2", got)
	}
}

func TestRateLimitRetriesAreBounded(t *testing.T) {
	// Without a bound this is a loop the operator cannot see, inside a build
	// that has its own timeout.
	f := newAPIFixture(t)
	var calls atomic.Int64
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}

	err := f.client.do(context.Background(), 999, http.MethodGet, "/repos/acme/docs", nil, nil)
	if err == nil {
		t.Fatal("a persistent rate limit was reported as success")
	}
	if got := calls.Load(); got != rateLimitAttempts {
		t.Errorf("%d calls, want %d", got, rateLimitAttempts)
	}
}

func TestRetryAfterDistinguishesZeroFromAbsent(t *testing.T) {
	// Both leave RetryAfter at zero and they mean opposite things: an explicit
	// zero is "retry now", a missing header is "you decide". Collapsing them made
	// every explicit zero wait the fallback for no reason, and would make a real
	// absent header retry instantly into the same limit.
	// Built with Set, not a map literal. Get canonicalizes the key it looks up,
	// and "X-RateLimit-Reset" canonicalizes to "X-Ratelimit-Reset" — so a literal
	// would never be found, and the test would fail on its own spelling rather
	// than on the code. A real response is canonicalized by the HTTP parser.
	header := func(k, v string) http.Header {
		h := http.Header{}
		h.Set(k, v)
		return h
	}

	if d, ok := retryAfterFrom(header("Retry-After", "0")); d != 0 || !ok {
		t.Errorf("explicit zero: got (%s, %v), want (0s, true)", d, ok)
	}

	if d, ok := retryAfterFrom(http.Header{}); d != 0 || ok {
		t.Errorf("no header: got (%s, %v), want (0s, false)", d, ok)
	}

	// A reset already in the past is "retry now", not a negative wait.
	past := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	if d, ok := retryAfterFrom(header("X-RateLimit-Reset", past)); d != 0 || !ok {
		t.Errorf("past reset: got (%s, %v), want (0s, true)", d, ok)
	}

	// An unparseable value is no information, not zero seconds.
	if d, ok := retryAfterFrom(header("Retry-After", "soon")); d != 0 || ok {
		t.Errorf("junk header: got (%s, %v), want (0s, false)", d, ok)
	}

	// Retry-After wins over the reset when both are present: it is what GitHub
	// documents sending with a secondary limit.
	both := header("Retry-After", "7")
	both.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	if d, ok := retryAfterFrom(both); d != 7*time.Second || !ok {
		t.Errorf("both headers: got (%s, %v), want (7s, true)", d, ok)
	}
}

func TestRateLimitWithNoHeaderUsesTheFallback(t *testing.T) {
	// The path the zero/absent distinction exists to protect. Retrying instantly
	// into a limit that gave no hint would burn all three attempts in a
	// millisecond and report the same failure.
	f := newAPIFixture(t)
	var calls atomic.Int64
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}

	// Cancelled well inside the fallback, so the test asserts the wait happened
	// without waiting for it.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := f.client.do(ctx, 999, http.MethodGet, "/repos/acme/docs", nil, nil)
	if err == nil {
		t.Fatal("expected the rate limit to be reported")
	}
	if elapsed := time.Since(start); elapsed >= rateLimitFallback {
		t.Errorf("waited %s; the context did not cut the fallback short", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("%d calls, want 1 — the second attempt was still waiting", got)
	}
}

func TestRateLimitRetryHonoursTheContext(t *testing.T) {
	// The wait comes from GitHub and can be tens of seconds, so a cancelled
	// build must not sit in it. The rate limit is still reported, because that
	// is the thing the operator has to act on.
	f := newAPIFixture(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := f.client.do(ctx, 999, http.MethodGet, "/repos/acme/docs", nil, nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s; the context was ignored", elapsed)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.RateLimited {
		t.Errorf("got %v, want the rate limit reported alongside the cancellation", err)
	}
}

func TestServerErrorIsNotRetried(t *testing.T) {
	// Retryable() says a 5xx could be, and do deliberately does not. A rate
	// limit is refused before GitHub acts; a 500 may have created the comment
	// before failing to say so, and the upsert finds-then-creates, so a retry
	// there posts a second comment.
	f := newAPIFixture(t)
	var calls atomic.Int64
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Server Error"}`))
	}

	if err := f.client.do(context.Background(), 999, http.MethodPost, "/repos/acme/docs/issues/42/comments",
		map[string]string{"body": "x"}, nil); err == nil {
		t.Fatal("a 500 was reported as success")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("%d calls, want 1 — a 500 may already have had its effect", got)
	}
}

func TestUnauthorizedInvalidatesTheTokenAndRetriesOnce(t *testing.T) {
	// An installation token can be revoked without expiring: a permissions
	// change, a suspension, a reinstall. None of those move the clock, so the
	// refresh margin never notices and every request 401s until the cached
	// expiry finally passes.
	f := newAPIFixture(t)

	var calls atomic.Int64
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}

	var out struct{ OK bool }
	if err := f.client.do(context.Background(), 999, http.MethodGet, "/repos/acme/docs", nil, &out); err != nil {
		t.Fatalf("the retry did not recover: %v", err)
	}
	if !out.OK {
		t.Error("the retried response was not decoded")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("%d API calls, want 2 — one rejected and one retried", got)
	}
	if got := f.tokenMints.Load(); got != 2 {
		t.Errorf("%d tokens minted, want 2 — the 401 must invalidate the cached one", got)
	}
}

func TestUnauthorizedRetriesOnlyOnce(t *testing.T) {
	// A counter here instead of a flag would loop on a genuine auth failure,
	// minting a token per attempt against a rate limit.
	f := newAPIFixture(t)

	var calls atomic.Int64
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}

	err := f.client.do(context.Background(), 999, http.MethodGet, "/repos/acme/docs", nil, nil)
	if err == nil {
		t.Fatal("a persistent 401 was reported as success")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("%d API calls, want exactly 2", got)
	}
}

func TestInstallationTokenIsCachedAcrossRequests(t *testing.T) {
	// The counterpart to the invalidation test: without this, "invalidate on
	// 401" could be satisfied by never caching at all.
	f := newAPIFixture(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}

	for range 3 {
		if err := f.client.do(context.Background(), 999, http.MethodGet, "/repos/acme/docs", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.tokenMints.Load(); got != 1 {
		t.Errorf("%d tokens minted for 3 requests, want 1", got)
	}
}

func TestMissingInstallationIDIsNamedClearly(t *testing.T) {
	// A delivery with no installation.id cannot be authenticated at all, and the
	// error has to say that rather than surfacing as a 401 later.
	f := newAPIFixture(t)
	err := f.client.do(context.Background(), 0, http.MethodGet, "/repos/acme/docs", nil, nil)
	if err == nil {
		t.Fatal("a zero installation id was accepted")
	}
	if f.tokenMints.Load() != 0 {
		t.Error("a token was minted for a zero installation id")
	}
}
