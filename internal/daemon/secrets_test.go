package daemon

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

func testAdmin(t *testing.T, listen string) (*SecretsAdmin, http.Handler) {
	t.Helper()

	cfg := config.DefaultServer()
	cfg.DataDir = t.TempDir()
	cfg.Listeners = []config.Listener{{TCP: listen}}

	a := NewSecretsAdmin(cfg, slog.New(slog.DiscardHandler), func(string) {})
	return a, a.Handler()
}

// do issues a request that looks like it came from this machine.
//
// The RemoteAddr override is load-bearing. httptest.NewRequest defaults it to
// 192.0.2.1:1234, which the locality gate correctly refuses — so without this
// every write test would assert the 403 path and none would cover the behaviour
// they were written for.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doFrom(t, h, method, path, body, "127.0.0.1:54321", nil)
}

// doFrom issues a request from a chosen address, optionally with headers, for the
// tests that are about the locality gate itself.
func doFrom(t *testing.T, h http.Handler, method, path, body, remote string,
	headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestSecretsRefusedOnANonLoopbackListener(t *testing.T) {
	// The gate this whole surface depends on. A credential-write endpoint on an
	// address other people can reach has no authentication in front of it, and
	// that is worse than having no UI at all.
	_, h := testAdmin(t, "0.0.0.0:8471")

	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/secrets/unlock", `{"key":"hunter2"}`},
		{"PUT", "/api/secrets/github.webhook_secret", `{"value":"x"}`},
		{"DELETE", "/api/secrets/github.webhook_secret", ""},
	} {
		rec := do(t, h, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}

	// The read path still answers, and explains itself. A feature that vanishes
	// with no reason reads as broken.
	rec := do(t, h, "GET", "/api/secrets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	var st secretsState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Available {
		t.Error("available on a wildcard listener")
	}
	if !strings.Contains(st.Reason, "0.0.0.0:8471") {
		t.Errorf("the reason does not name the listener: %q", st.Reason)
	}
}

func TestSecretsRefusedWithAZitiListener(t *testing.T) {
	cfg := config.DefaultServer()
	cfg.DataDir = t.TempDir()
	cfg.Listeners = []config.Listener{{Ziti: &config.ZitiListener{
		IdentityFile: "id.json", Service: "admin",
	}}}

	a := NewSecretsAdmin(cfg, slog.New(slog.DiscardHandler), func(string) {})
	if ok, why := a.Available(); ok {
		t.Error("available behind a ziti listener with no identity check")
	} else if !strings.Contains(why, "identity") {
		t.Errorf("the reason does not say why: %q", why)
	}
}

func TestSecretsRoundTripNeverReturnsAValue(t *testing.T) {
	// The invariant that matters most: there is no path that reads a secret
	// back out. The operator does not need one; an attacker does.
	const canary = "dpfake_9f2a1c4b7e01d3f5a8c6b2e4"

	a, h := testAdmin(t, "127.0.0.1:8471")

	if rec := do(t, h, "POST", "/api/secrets/unlock", `{"key":"a-test-passphrase"}`); rec.Code != http.StatusOK {
		t.Fatalf("unlock = %d: %s", rec.Code, rec.Body.String())
	}
	rec := do(t, h, "PUT", "/api/secrets/demo.key", `{"value":"`+canary+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), canary) {
		t.Error("the write response echoed the value back")
	}

	body := do(t, h, "GET", "/api/secrets", "").Body.String()
	if strings.Contains(body, canary) {
		t.Errorf("the listing contains the value:\n%s", body)
	}
	if !strings.Contains(body, "demo.key") {
		t.Errorf("the listing does not mention the key:\n%s", body)
	}

	// And it really was stored — the absence above is not just an empty vault.
	v := a.Vault()
	if v == nil {
		t.Fatal("the vault is not open after unlock")
	}
	got, err := v.Get("demo.key")
	if err != nil {
		t.Fatal(err)
	}
	if got.RevealString() != canary {
		t.Errorf("stored value = %q", got.RevealString())
	}

	// Nor is it on disk in the clear.
	raw, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatalf("reading the vault file: %v", err)
	}
	if strings.Contains(string(raw), canary) {
		t.Error("the value is readable in the vault file")
	}
}

func TestSecretsGenerateShowsTheValueExactlyOnce(t *testing.T) {
	// The one endpoint that emits a secret. It has to, because a webhook secret
	// must be identical in GitHub's form and in this vault, and a UI that only
	// accepts values leaves the operator with nowhere to get one from.
	//
	// What must hold: the value comes back from the call that created it, it is
	// really stored, and no later request can retrieve it.
	_, h := testAdmin(t, "127.0.0.1:8471")
	do(t, h, "POST", "/api/secrets/unlock", `{"key":"pass"}`)

	rec := do(t, h, "POST", "/api/secrets/github.webhook_secret/generate", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("generate = %d: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		ShownOnce string       `json:"shown_once"`
		Entries   []secretView `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.ShownOnce) < 40 {
		t.Fatalf("shown_once = %q, want a 32-byte base64 value", out.ShownOnce)
	}

	var found bool
	for _, e := range out.Entries {
		if e.Key == "github.webhook_secret" {
			found = e.Set
		}
	}
	if !found {
		t.Error("the generated secret is not marked as set")
	}

	// And it is gone from every subsequent response.
	again := do(t, h, "GET", "/api/secrets", "").Body.String()
	if strings.Contains(again, out.ShownOnce) {
		t.Error("the listing returns a value that was supposed to be shown once")
	}
	if strings.Contains(again, "shown_once") {
		t.Error("the listing carries a shown_once field")
	}
}

func TestSecretsGenerateProducesADifferentValueEachTime(t *testing.T) {
	_, h := testAdmin(t, "127.0.0.1:8471")
	do(t, h, "POST", "/api/secrets/unlock", `{"key":"pass"}`)

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		var out struct {
			ShownOnce string `json:"shown_once"`
		}
		rec := do(t, h, "POST", "/api/secrets/demo.key/generate", "")
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if seen[out.ShownOnce] {
			t.Fatalf("generate returned a repeated value: %q", out.ShownOnce)
		}
		seen[out.ShownOnce] = true
	}
}

func TestSecretsCreatingAVaultPersistsIt(t *testing.T) {
	// "Create" has to create something. Open on a missing file returns an empty vault in memory and writes
	// nothing on its own, so unlock must persist the vault immediately: otherwise a restart before the first
	// secret is stored discards the passphrase and the page offers to create the vault again, indistinguishable
	// from the vault having been wiped.
	a, h := testAdmin(t, "127.0.0.1:8471")

	if vaultExists(a.path) {
		t.Fatal("a vault exists before anything created one")
	}
	if rec := do(t, h, "POST", "/api/secrets/unlock", `{"key":"correct horse battery staple"}`); rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if !vaultExists(a.path) {
		t.Fatal("the vault was not written to disk")
	}

	// And a fresh process opens it with the same passphrase — which is the
	// property the file existing is supposed to guarantee.
	b := NewSecretsAdmin(a.cfg, slog.New(slog.DiscardHandler), func(string) {})
	b.v = nil
	if rec := do(t, b.Handler(), "POST", "/api/secrets/unlock",
		`{"key":"correct horse battery staple"}`); rec.Code != http.StatusOK {
		t.Errorf("reopening with the same passphrase = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSecretsWrongKeyIsRefusedWithoutDetail(t *testing.T) {
	a, h := testAdmin(t, "127.0.0.1:8471")

	if rec := do(t, h, "POST", "/api/secrets/unlock", `{"key":"first-passphrase"}`); rec.Code != http.StatusOK {
		t.Fatalf("create = %d", rec.Code)
	}
	if rec := do(t, h, "PUT", "/api/secrets/demo.key", `{"value":"whatever"}`); rec.Code != http.StatusOK {
		t.Fatalf("put = %d", rec.Code)
	}

	// A second admin against the same file, with the wrong key.
	b := NewSecretsAdmin(a.cfg, slog.New(slog.DiscardHandler), func(string) {})
	b.v = nil
	rec := do(t, b.Handler(), "POST", "/api/secrets/unlock", `{"key":"wrong-passphrase"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	// age cannot tell a wrong key from a corrupt file, and neither should the
	// response — both mean "you cannot get in".
	if strings.Contains(strings.ToLower(rec.Body.String()), "corrupt") {
		t.Errorf("the refusal speculates about the cause: %s", rec.Body.String())
	}
}

func TestSecretsRejectsABadKeyName(t *testing.T) {
	// The key becomes a map entry and is echoed into a JSON document; a path
	// separator in it has no legitimate use.
	_, h := testAdmin(t, "127.0.0.1:8471")
	do(t, h, "POST", "/api/secrets/unlock", `{"key":"pass"}`)

	rec := do(t, h, "PUT", "/api/secrets/..%2Fescape", `{"value":"x"}`)
	if rec.Code == http.StatusOK {
		t.Errorf("a traversing key was accepted: %s", rec.Body.String())
	}
}

func TestSecretsWriteRefusedWhileLocked(t *testing.T) {
	cfg := config.DefaultServer()
	cfg.DataDir = t.TempDir()
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}

	a := NewSecretsAdmin(cfg, slog.New(slog.DiscardHandler), func(string) {})
	a.v = nil // whatever the environment held, this test is about the locked path

	rec := do(t, a.Handler(), "PUT", "/api/secrets/demo.key", `{"value":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

// The locality gate. Available() asks whether the daemon's *listeners* are
// loopback; these ask whether a *request* came from this machine. The gap between
// the two is a tunnel: `zrok2 share public http://127.0.0.1:8471` publishes every
// route on the internet while the listener is still loopback, so Available says
// yes and the credential API is exposed. RemoteAddr does not close that gap on
// its own — under a tunnel the daemon sees the connection from the local tunnel
// process — which is why the forwarding headers are checked too.

func TestSecretsWriteRefusedFromARemoteAddress(t *testing.T) {
	a, h := testAdmin(t, "127.0.0.1:8471")
	a.v = nil

	for _, remote := range []string{"192.0.2.1:1234", "10.0.0.5:443", "[2001:db8::1]:80"} {
		rec := doFrom(t, h, "PUT", "/api/secrets/demo.key", `{"value":"x"}`, remote, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", remote, rec.Code)
		}
	}
}

func TestSecretsWriteRefusedWhenForwarded(t *testing.T) {
	// The tunnel case, and the reason RemoteAddr alone is not enough. Anything
	// proxying to the daemon sets one of these, including docpreview's own
	// webhook-only proxy.
	a, h := testAdmin(t, "127.0.0.1:8471")
	a.v = nil

	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"} {
		rec := doFrom(t, h, "PUT", "/api/secrets/demo.key", `{"value":"x"}`,
			"127.0.0.1:54321", map[string]string{header: "203.0.113.7"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 — a forwarded request did not originate here",
				header, rec.Code)
		}
	}
}

func TestSecretsUnlockRefusedFromARemoteAddress(t *testing.T) {
	// The one that matters most: the endpoint that would otherwise accept the
	// passphrase to everything.
	_, h := testAdmin(t, "127.0.0.1:8471")

	rec := doFrom(t, h, "POST", "/api/secrets/unlock", `{"key":"a-passphrase"}`, "192.0.2.1:1234", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestSecretsReadIsAllowedRemotelyAndReportsReadOnly(t *testing.T) {
	// Deliberately not gated. It returns no values, and a panel that cannot
	// explain why it is read-only reads as a broken feature. can_write is what
	// stops the page offering buttons that would 403.
	_, h := testAdmin(t, "127.0.0.1:8471")

	rec := doFrom(t, h, "GET", "/api/secrets", "", "192.0.2.1:1234", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("remote read = %d, want 200", rec.Code)
	}
	var st secretsState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.CanWrite {
		t.Error("can_write is true for a remote caller")
	}
	if st.ReadOnlyWhy == "" {
		t.Error("read_only_why is empty, so the page cannot say why it is read-only")
	}
}

func TestSecretsLocalReadReportsWritable(t *testing.T) {
	_, h := testAdmin(t, "127.0.0.1:8471")

	rec := do(t, h, "GET", "/api/secrets", "")
	var st secretsState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.CanWrite {
		t.Errorf("can_write is false from loopback: %s", st.ReadOnlyWhy)
	}
	if st.ReadOnlyWhy != "" {
		t.Errorf("read_only_why is set for a writable caller: %q", st.ReadOnlyWhy)
	}
}

func TestIsLocalRequestAcceptsEveryLoopbackForm(t *testing.T) {
	// 127.0.0.53 is loopback and is what systemd-resolved uses, so a check that
	// only accepted 127.0.0.1 would refuse a legitimate local caller.
	for _, remote := range []string{"127.0.0.1:54321", "[::1]:54321", "127.0.0.53:9"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if ok, why := isLocalRequest(r); !ok {
			t.Errorf("%s was refused: %s", remote, why)
		}
	}
}
