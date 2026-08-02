package bitbucket

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/vault"
)

// authRecorder captures the Authorization header of every request, so a test can assert
// *which identity* a call was made as — the thing per-repository credentials get wrong.
type authRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (a *authRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.seen = append(a.seen, r.Header.Get("Authorization"))
		a.mu.Unlock()
		// Enough of a repository object for CheckRepo, and a full hash for resolveCommit.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"full_name": "netfoundry/docs", "is_private": true, "size": 0,
			"hash": "a4fd6c9db1940992c8af5c48401462100bd7d2f1",
		})
	})
}

func (a *authRecorder) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.seen) == 0 {
		return ""
	}
	return a.seen[len(a.seen)-1]
}

// clientWithGlobal builds a client whose only credential is the global one.
func clientWithGlobal(t *testing.T, base string, global string) *Client {
	t.Helper()

	dir := t.TempDir()
	v, err := vault.OpenWithKey(dir+"/vault.age", "a-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set(vault.KeyBitbucketHookSec, vault.NewSecretString(testSecret)); err != nil {
		t.Fatal(err)
	}
	if global != "" {
		if err := v.Set(vault.KeyBitbucketAccessToken, vault.NewSecretString(global)); err != nil {
			t.Fatal(err)
		}
	}

	c, err := New(config.BitbucketConfig{
		Enabled: true, APIBase: base, Auth: config.BitbucketAuthAccessToken,
	}, v, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestAProjectsOwnTokenWinsOverTheGlobalOne.
//
// The reason this whole mechanism exists: a Bitbucket access token is scoped to one
// repository unless a workspace administrator permits wider ones, so an operator with two
// repositories has two tokens and no global one that reaches both. Resolving to the wrong
// one is a 403 that looks like a scope problem.
func TestAProjectsOwnTokenWinsOverTheGlobalOne(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c := clientWithGlobal(t, srv.URL, "the-global-token")
	c = c.WithProjectCredentials(func(owner, repo string) ProjectCredential {
		if owner == "netfoundry" && repo == "docs" {
			return ProjectCredential{AccessToken: "the-project-token"}
		}
		return ProjectCredential{}
	})

	if _, err := c.CheckRepo(t.Context(),
		model.Repo{Platform: model.PlatformBitbucket, Owner: "netfoundry", Name: "docs"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(); got != "Bearer the-project-token" {
		t.Errorf("called as %q, want the project's own token", got)
	}

	// A repository with no credential of its own falls back to the global one rather than
	// failing: a workspace token, where one is allowed, should still cover everything.
	if _, err := c.CheckRepo(t.Context(),
		model.Repo{Platform: model.PlatformBitbucket, Owner: "netfoundry", Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(); got != "Bearer the-global-token" {
		t.Errorf("called as %q, want the global token", got)
	}
}

// TestAProjectEmailAndAPITokenBecomeBasicAuth. The fallback mode, and the one that needs
// the email: a bearer token has the literal x-token-auth as its username, an account API
// token has the person's address.
func TestAProjectEmailAndAPITokenBecomeBasicAuth(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c := clientWithGlobal(t, srv.URL, "")
	c = c.WithProjectCredentials(func(owner, repo string) ProjectCredential {
		return ProjectCredential{Email: "someone@example.com", APIToken: "an-api-token"}
	})

	if _, err := c.CheckRepo(t.Context(),
		model.Repo{Platform: model.PlatformBitbucket, Owner: "acme", Name: "docs"}); err != nil {
		t.Fatal(err)
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("someone@example.com:an-api-token"))
	if got := rec.last(); got != want {
		t.Errorf("called with %q, want basic auth built from the email and token", got)
	}
}

// TestAnAccessTokenBeatsAnAPITokenOnOneProject — the narrower credential wins when both
// are stored, which happens when somebody fills in the form twice.
func TestAnAccessTokenBeatsAnAPITokenOnOneProject(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c := clientWithGlobal(t, srv.URL, "")
	c = c.WithProjectCredentials(func(owner, repo string) ProjectCredential {
		return ProjectCredential{
			AccessToken: "the-repo-token",
			Email:       "someone@example.com",
			APIToken:    "an-api-token",
		}
	})

	if _, err := c.CheckRepo(t.Context(),
		model.Repo{Platform: model.PlatformBitbucket, Owner: "acme", Name: "docs"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(); got != "Bearer the-repo-token" {
		t.Errorf("called as %q, want the repository access token", got)
	}
}

// TestNoCredentialAnywhereNamesBothPlaces.
//
// The failure an operator meets first, and the message is the whole value of the test: it
// has to say that a token can go on the project *or* in the vault, because a daemon with
// neither looks identical to one whose token is simply wrong.
func TestNoCredentialAnywhereNamesBothPlaces(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c := clientWithGlobal(t, srv.URL, "")

	_, err := c.CheckRepo(t.Context(),
		model.Repo{Platform: model.PlatformBitbucket, Owner: "acme", Name: "docs"})
	if err == nil {
		t.Fatal("a repository with no credential anywhere was accepted")
	}
	for _, want := range []string{"acme/docs", "/projects", vault.KeyBitbucketAccessToken} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	if rec.last() != "" {
		t.Error("a request was made with no credential")
	}
}

// TestTheClientBuildsWithNoGlobalCredential.
//
// A workspace whose administrator refuses wide tokens has no global credential to store.
// Requiring one here would refuse to build a Bitbucket client at all, and so could not use
// the projects page to supply the per-repository tokens that would make it work. The webhook
// secret is still required, because without it deliveries are unauthenticated.
func TestTheClientBuildsWithNoGlobalCredential(t *testing.T) {
	dir := t.TempDir()
	v, err := vault.OpenWithKey(dir+"/vault.age", "a-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set(vault.KeyBitbucketHookSec, vault.NewSecretString(testSecret)); err != nil {
		t.Fatal(err)
	}

	c, err := New(config.BitbucketConfig{
		Enabled: true, APIBase: config.BitbucketAPIBase, Auth: config.BitbucketAuthAccessToken,
	}, v, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("a client with per-project credentials only was refused: %v", err)
	}
	if c.Platform() != model.PlatformBitbucket {
		t.Errorf("platform = %q", c.Platform())
	}
	// And Validate says so rather than reporting a failure: there is nothing global to
	// check, which is a configuration and not a fault.
	if err := c.Validate(t.Context()); err != nil {
		t.Errorf("Validate with no global credential = %v, want nil", err)
	}
}

// TestCloneURLUsesTheProjectsOwnToken. The clone is the one place the credential becomes a
// string in an argument list, so the wrong one here is a 403 twenty seconds into a build.
func TestCloneURLUsesTheProjectsOwnToken(t *testing.T) {
	c := clientWithGlobal(t, config.BitbucketAPIBase, "the-global-token")
	c = c.WithProjectCredentials(func(owner, repo string) ProjectCredential {
		return ProjectCredential{AccessToken: "the-project-token"}
	})

	got, err := c.CloneURL(t.Context(), model.PullRequest{
		Repo: model.Repo{Platform: model.PlatformBitbucket, Owner: "netfoundry", Name: "docs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "x-token-auth:the-project-token@") {
		t.Errorf("CloneURL = %s, want the project's own token", got)
	}
	if strings.Contains(got, "the-global-token") {
		t.Error("the clone URL carries the global token")
	}
}
