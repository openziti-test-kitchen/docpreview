package daemon

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/store"
	"github.com/netfoundry/docpreview/internal/vault"
)

// projectSecretsFixture is a projects admin with an open vault behind it.
func projectSecretsFixture(t *testing.T) (http.Handler, *vault.Vault, *store.Store) {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	v, err := vault.OpenWithKey(filepath.Join(dir, "vault.age"), "a-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}
	cfg.Build.Secrets = map[string]string{"GLOBAL_TOKEN": "some.vault.key"}

	admin := NewProjectsAdmin(st, cfg, slog.New(slog.DiscardHandler)).
		WithVault(func() *vault.Vault { return v })
	return admin.Handler(), v, st
}

func secretCall(t *testing.T, h http.Handler, method, path, body, remote string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.RemoteAddr = remote
	h.ServeHTTP(rec, r)
	return rec
}

const localCaller = "127.0.0.1:54321"

// TestProjectSecretRoundTrip — set, see the name, delete. The name is what the page
// needs; the value must never come back, which the next test asserts separately.
func TestProjectSecretRoundTrip(t *testing.T) {
	h, v, st := projectSecretsFixture(t)
	if err := st.SaveProject(t.Context(), store.Project{
		Platform: "github", Owner: "netfoundry", Repo: "unified-doc", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	const path = "/api/projects/github/netfoundry/unified-doc/secrets/BB_REPO_TOKEN_ONPREM"
	rec := secretCall(t, h, "PUT", path, `{"value":"a-token-value"}`, localCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Stored under the project's namespace, so it cannot collide with a global key
	// or with another project's variable of the same name.
	want := vault.ProjectSecretKey("github", "netfoundry", "unified-doc", "BB_REPO_TOKEN_ONPREM")
	if _, err := v.Get(want); err != nil {
		t.Fatalf("the secret is not at %s: %v", want, err)
	}

	var out struct {
		Projects []struct {
			Repo    string   `json:"repo"`
			Secrets []string `json:"secrets"`
		} `json:"projects"`
		GlobalSecrets []string `json:"global_secrets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Projects) != 1 || len(out.Projects[0].Secrets) != 1 ||
		out.Projects[0].Secrets[0] != "BB_REPO_TOKEN_ONPREM" {
		t.Errorf("the response does not list the secret: %s", rec.Body.String())
	}
	// The server-wide names are reported too. "This project has no secrets" and
	// "this project has none of its own" look identical otherwise, and only one of
	// them means a build is about to fail.
	if len(out.GlobalSecrets) != 1 || out.GlobalSecrets[0] != "GLOBAL_TOKEN" {
		t.Errorf("global secret names missing: %v", out.GlobalSecrets)
	}

	if rec := secretCall(t, h, "DELETE", path, "", localCaller); rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, err := v.Get(want); err == nil {
		t.Error("the secret survived its own deletion")
	}
}

// TestProjectSecretValuesAreNeverReturned. The read path is open from anywhere the
// dashboard is reachable — including the public read-only share — while writing is
// loopback only. A value in this payload would be a credential on the internet.
func TestProjectSecretValuesAreNeverReturned(t *testing.T) {
	h, _, st := projectSecretsFixture(t)
	if err := st.SaveProject(t.Context(), store.Project{
		Platform: "github", Owner: "netfoundry", Repo: "unified-doc", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	const value = "s3cret-token-value"
	secretCall(t, h, "PUT",
		"/api/projects/github/netfoundry/unified-doc/secrets/BB_REPO_TOKEN_ONPREM",
		`{"value":"`+value+`"}`, localCaller)

	rec := secretCall(t, h, "GET", "/api/projects", "", localCaller)
	if strings.Contains(rec.Body.String(), value) {
		t.Fatal("a project secret's value is in the projects payload")
	}
	if !strings.Contains(rec.Body.String(), "BB_REPO_TOKEN_ONPREM") {
		t.Error("the name is missing, so the page cannot show what is set")
	}
}

// TestProjectSecretWritesAreLocalOnly — an environment variable becomes part of a
// process that runs a build script, so this is the same boundary as the credential
// page and the build command, not a lesser one.
func TestProjectSecretWritesAreLocalOnly(t *testing.T) {
	h, v, _ := projectSecretsFixture(t)
	const path = "/api/projects/github/netfoundry/unified-doc/secrets/BB_REPO_TOKEN_ONPREM"

	for _, tc := range []struct {
		name, remote string
		forwarded    bool
	}{
		{name: "from another machine", remote: "10.0.0.7:33333"},
		{name: "forwarded by a proxy", remote: localCaller, forwarded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("PUT", path, strings.NewReader(`{"value":"a-token-value"}`))
			r.RemoteAddr = tc.remote
			if tc.forwarded {
				// A tunnel makes every route reachable while RemoteAddr stays
				// loopback. The forwarding header is the only signal there is.
				r.Header.Set("X-Forwarded-For", "203.0.113.9")
			}
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
		})
	}

	if keys := v.KeysWithPrefix(vault.ProjectPrefix); len(keys) != 0 {
		t.Errorf("a refused request still wrote %v", keys)
	}
}

// TestProjectSecretNamesMustBeShellUsable. A name with a dot or a dash in it is
// settable through exec.Cmd and unreadable from sh, so the operator would store a
// value the build could never see — a failure with nothing to look at.
func TestProjectSecretNamesMustBeShellUsable(t *testing.T) {
	h, _, _ := projectSecretsFixture(t)
	base := "/api/projects/github/netfoundry/unified-doc/secrets/"

	for _, env := range []string{"bb.repo.token", "bb-repo-token", "9LIVES", "bb_repo_token"} {
		rec := secretCall(t, h, "PUT", base+env, `{"value":"a-token-value"}`, localCaller)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", env, rec.Code)
		}
	}
	if rec := secretCall(t, h, "PUT", base+"BB_REPO_TOKEN_2",
		`{"value":"a-token-value"}`, localCaller); rec.Code != http.StatusOK {
		t.Errorf("a legal name was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// TestGlobalSecretKeyCannotReachTheProjectNamespace.
//
// The two scopes are kept apart by the slash: a global key is letters, digits, dot,
// dash and underscore. Relax validKey to allow a slash and the global surface can
// write `project/github/acme/docs/ANYTHING` — or, worse, a project row's namespace
// could be made to shadow `github.private_key`.
func TestGlobalSecretKeyCannotReachTheProjectNamespace(t *testing.T) {
	for _, k := range []string{
		"project/github/acme/docs/BB_TOKEN",
		"project/github/acme/docs/",
		"a/b",
	} {
		if validKey(k) {
			t.Errorf("validKey(%q) = true; the global surface could write into the "+
				"project namespace", k)
		}
	}
	// And the composed key really does contain the character that makes that true,
	// so this test cannot pass because the format changed underneath it.
	key := vault.ProjectSecretKey("github", "acme", "docs", "BB_TOKEN")
	if !strings.Contains(key, "/") {
		t.Errorf("the project key %q has no slash, so nothing separates the scopes", key)
	}
}

// TestProjectSecretsWithALockedVaultSayWhy — a locked vault is the ordinary state of
// a daemon that just booted. Reporting the panel as empty would read as data loss;
// 409 with the place to go is the answer the page can act on.
func TestProjectSecretsWithALockedVaultSayWhy(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}

	h := NewProjectsAdmin(st, cfg, slog.New(slog.DiscardHandler)).
		WithVault(func() *vault.Vault { return nil }).Handler()

	rec := secretCall(t, h, "PUT",
		"/api/projects/github/netfoundry/unified-doc/secrets/BB_REPO_TOKEN_ONPREM",
		`{"value":"a-token-value"}`, localCaller)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/secrets") {
		t.Errorf("the error does not say where to unlock: %s", rec.Body.String())
	}

	var out struct {
		VaultLocked      bool `json:"vault_locked"`
		SecretsAvailable bool `json:"secrets_available"`
	}
	rec = secretCall(t, h, "GET", "/api/projects", "", localCaller)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.VaultLocked || !out.SecretsAvailable {
		t.Errorf("locked = %v, available = %v; the page cannot tell locked from "+
			"not-a-feature", out.VaultLocked, out.SecretsAvailable)
	}
}
