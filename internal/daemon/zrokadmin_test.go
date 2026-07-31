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
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/store"
	"github.com/netfoundry/docpreview/internal/vault"
)

// zrokAdminFixture is a ZrokAdmin on a loopback daemon with a locked vault.
//
// A locked vault is the interesting default here: enrolment has to work without one — the account
// token simply cannot be stored — and a fixture with an open vault would hide that.
func zrokAdminFixture(t *testing.T, scope expose.ZrokScope) (*ZrokAdmin, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Server{
		DataDir:   dir,
		Listeners: []config.Listener{{TCP: "127.0.0.1:8471"}},
	}
	cfg.Exposer.Kind = "zrok2"

	a := NewZrokAdmin(cfg, st, slog.New(slog.DiscardHandler), func() (*vault.Vault, error) {
		return nil, vault.ErrLocked
	})
	// The seam. Which zrok directory the process loaded is a process-wide global that is
	// deliberately one-way, so it is injected here rather than set — see the field's comment.
	a.scopeOf = func() expose.ZrokScope { return scope }
	return a, a.Handler()
}

func zrokPost(h http.Handler, path string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// docpreview must not delete the machine-wide zrok environment.
//
// `~/.zrok2` belongs to the zrok CLI and to whatever else that account is used for — a share
// somebody left running, another tool, a colleague's scripts. Deleting it from a
// documentation-preview dashboard takes those with it, and nobody would think to look here for the
// cause. `zrok2 disable` is the tool for that, run by the person who knows what depends on it.
//
// Enforced rather than only hidden on the page: a hidden button is a preference, and an endpoint
// that would still do it is the thing an errant script finds.
func TestDisableRefusesTheMachineWideZrokEnvironment(t *testing.T) {
	_, h := zrokAdminFixture(t, expose.ZrokSystem)
	w := zrokPost(h, "/api/zrok/disable", `{}`)

	if w.Code != http.StatusConflict {
		t.Fatalf("POST /api/zrok/disable = %d, want 409 while using the machine's environment",
			w.Code)
	}
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// The refusal has to say where the control is, or it reads as a broken feature.
	if !strings.Contains(out.Error, "zrok2 disable") {
		t.Errorf("the refusal does not name the tool that can do it: %q", out.Error)
	}
}

// The same request against this installation's own environment is not refused for that reason.
//
// It fails, because nothing is enrolled in a temporary directory — but with the enrolment error
// rather than the ownership one, which is what shows the check is scoped rather than blanket.
func TestDisableIsAllowedForThisInstallationsOwnEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	_, h := zrokAdminFixture(t, expose.ZrokProject)
	w := zrokPost(h, "/api/zrok/disable", `{}`)

	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if strings.Contains(out.Error, "zrok2 disable") {
		t.Errorf("this installation's own environment was refused as the machine's: %q", out.Error)
	}
	if !strings.Contains(out.Error, "not enabled") {
		t.Errorf("expected the not-enrolled error, got %q", out.Error)
	}
}

// The state payload never carries the account token. It is the credential that can create and
// delete every share on the account, and this is the one endpoint a browser reads.
func TestTheZrokStateNeverCarriesTheAccountToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	_, h := zrokAdminFixture(t, expose.ZrokProject)
	r := httptest.NewRequest(http.MethodGet, "/api/zrok", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/zrok = %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"account_token", "token", "password"} {
		if _, ok := got[k]; ok {
			t.Errorf("the zrok state carried %q", k)
		}
	}
	// A locked vault reports that it is locked rather than claiming no token is stored, which
	// would be a different and unknowable statement.
	if locked, _ := got["vault_locked"].(bool); !locked {
		t.Error("a locked vault was not reported as locked")
	}
}
