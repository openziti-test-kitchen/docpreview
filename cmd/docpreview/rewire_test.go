package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/daemon"
	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
	"github.com/netfoundry/docpreview/internal/vault"
)

// The GitHub client is built from two values that live in the vault, so with
// github.app_id set the daemon used to refuse to start until the vault was
// open — while the page that opens it was served by that same daemon. The fix
// is to boot without a client and install one afterwards.
//
// These tests cover the sequence a person performs rather than the request
// shape: start locked, unlock, watch the webhook endpoint come alive. Every bug
// in this area so far was found by doing that by hand.

func testAppKeyPEM(t *testing.T) string {
	t.Helper()
	// 1024 bits: this key signs a JWT that never leaves the process in these
	// tests, and 2048 costs noticeably more per run.
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// rewireFixture assembles the pieces cmdServe wires together, minus the
// listeners: a daemon and an ingress over a temp store, and the vault path they
// share.
type rewireFixture struct {
	w         *wiring
	d         *daemon.Daemon
	ingress   *daemon.Ingress
	vaultPath string
}

func newRewireFixture(t *testing.T, appID int64) *rewireFixture {
	t.Helper()

	dir := t.TempDir()
	cfg := config.Server{
		DataDir:   dir,
		Listeners: []config.Listener{{TCP: "127.0.0.1:0"}},
	}
	cfg.GitHub.AppID = appID
	cfg.GitHub.APIBase = "https://api.github.com"

	st, err := store.Open(filepath.Join(dir, "docpreview.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.DiscardHandler)
	w := &wiring{
		cfg:       cfg,
		log:       log,
		store:     st,
		clients:   map[model.Platform]scm.Client{},
		vaultPath: filepath.Join(dir, "vault.age"),
	}
	ex := expose.NewLocal(log, "http://127.0.0.1:0")
	t.Cleanup(func() { ex.Close() })

	d := daemon.New(cfg, st, ex, w.clients, log)
	return &rewireFixture{
		w:         w,
		d:         d,
		ingress:   daemon.NewIngress(d, w.clients, st, log),
		vaultPath: w.vaultPath,
	}
}

// webhookStatus posts an unsigned delivery. 501 means no client is installed;
// anything else means one is, and rejected the signature.
func (f *rewireFixture) webhookStatus(t *testing.T) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader("{}"))
	f.ingress.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func (f *rewireFixture) vaultWith(t *testing.T, entries map[string]string) *vault.Vault {
	t.Helper()
	v, err := vault.OpenWithKey(f.vaultPath, "test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	for k, val := range entries {
		if err := v.Set(k, vault.NewSecretString(val)); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Save(); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRewireGitHubInstallsTheClientAfterUnlock(t *testing.T) {
	f := newRewireFixture(t, 4420399)

	if got := f.webhookStatus(t); got != http.StatusNotImplemented {
		t.Fatalf("webhook before unlock = %d, want %d — a daemon with no client "+
			"must say so rather than pretend to verify", got, http.StatusNotImplemented)
	}

	v := f.vaultWith(t, map[string]string{
		vault.KeyGitHubPrivateKey: testAppKeyPEM(t),
		vault.KeyGitHubWebhookSec: "a-webhook-secret",
	})

	// Empty key: this is the unlock case, where every value became readable at
	// once rather than one of them changing.
	rewireGitHub(f.w, f.d, f.ingress, v, "")

	if got := f.webhookStatus(t); got == http.StatusNotImplemented {
		t.Fatalf("webhook after unlock = %d, want anything but %d — the client "+
			"was not installed", got, http.StatusNotImplemented)
	}
}

func TestRewireGitHubWaitsForBothCredentials(t *testing.T) {
	// Storing one of the two and not yet the other is the normal middle of
	// setup. It must leave the daemon serving and the endpoint honest, not
	// install a half-built client.
	f := newRewireFixture(t, 4420399)

	v := f.vaultWith(t, map[string]string{
		vault.KeyGitHubPrivateKey: testAppKeyPEM(t),
	})
	rewireGitHub(f.w, f.d, f.ingress, v, vault.KeyGitHubPrivateKey)

	if got := f.webhookStatus(t); got != http.StatusNotImplemented {
		t.Fatalf("webhook with no webhook secret = %d, want %d", got, http.StatusNotImplemented)
	}

	// The second credential arrives and the client appears.
	if err := v.Set(vault.KeyGitHubWebhookSec, vault.NewSecretString("a-webhook-secret")); err != nil {
		t.Fatal(err)
	}
	rewireGitHub(f.w, f.d, f.ingress, v, vault.KeyGitHubWebhookSec)

	if got := f.webhookStatus(t); got == http.StatusNotImplemented {
		t.Fatalf("webhook after both credentials = %d, want anything but %d",
			got, http.StatusNotImplemented)
	}
}

func TestRewireGitHubIgnoresUnrelatedKeys(t *testing.T) {
	// rearm fires on every vault write, including build secrets. Rebuilding the
	// client then would throw away a cached installation token for nothing.
	f := newRewireFixture(t, 4420399)

	v := f.vaultWith(t, map[string]string{
		vault.KeyGitHubPrivateKey: testAppKeyPEM(t),
		vault.KeyGitHubWebhookSec: "a-webhook-secret",
		"demo.algolia_key":        "dpfake_abcdef0123456789",
	})
	rewireGitHub(f.w, f.d, f.ingress, v, "demo.algolia_key")

	if got := f.webhookStatus(t); got != http.StatusNotImplemented {
		t.Fatalf("an unrelated key built a client: webhook = %d, want %d",
			got, http.StatusNotImplemented)
	}
}

func TestRewireGitHubDoesNothingWithNoAppID(t *testing.T) {
	// app_id zero is "GitHub is not wired up", the permanent state of anyone on
	// Bitbucket. A stray private key in the vault must not conjure a client.
	f := newRewireFixture(t, 0)

	v := f.vaultWith(t, map[string]string{
		vault.KeyGitHubPrivateKey: testAppKeyPEM(t),
		vault.KeyGitHubWebhookSec: "a-webhook-secret",
	})
	rewireGitHub(f.w, f.d, f.ingress, v, "")

	if got := f.webhookStatus(t); got != http.StatusNotImplemented {
		t.Fatalf("webhook with app_id 0 = %d, want %d", got, http.StatusNotImplemented)
	}
}
