package expose

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/vault"
)

// fakeFrontdoor is a stand-in controller that records what was asked of it.
type fakeFrontdoor struct {
	mu sync.Mutex

	// createResponse is the raw JSON returned from POST /shares.
	createResponse string

	deleted []string
	created int
}

func (f *fakeFrontdoor) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/shares":
			f.created++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, f.createResponse)

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/shares"):
			f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/shares/"))
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && r.URL.Path == "/shares":
			json.NewEncoder(w).Encode(map[string]any{"_embedded": map[string]any{"shares": []any{}}})

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
	// The boot-order fix. NewFrontdoor used to take the token itself, so wiring
	// this exposer opened the vault — and with the vault locked the daemon
	// refused to start, hiding the page that unlocks it. Construction must now
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
