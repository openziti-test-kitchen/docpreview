package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm/bitbucket"
	"github.com/netfoundry/docpreview/internal/vault"
)

// TestBitbucketWithNoCredentialsMustNotStopTheDaemon.
//
// The deadlock this guards against has now been created three times in this codebase,
// twice for GitHub and once here: something reads the vault during wiring and refuses
// to start, while the page that fixes the vault is served by the process that will not
// start.
//
// Bitbucket's version is worse than GitHub's, because the missing credential is one
// the daemon *generates*: /secrets has a Generate button for the webhook secret, so a
// daemon that will not boot without that secret cannot be used to create it. The
// operator's only way out is `docpreview vault set` in a terminal — which is exactly
// what the setup page exists to avoid.
//
// This test asserts the shape rather than calling setup(): what must hold is that a
// New() failure is survivable, and that the surviving daemon has no Bitbucket client
// so /webhook/bitbucket answers 501 rather than pretending to verify.
func TestBitbucketWithNoCredentialsMustNotStopTheDaemon(t *testing.T) {
	dir := t.TempDir()
	v, err := vault.OpenWithKey(filepath.Join(dir, "vault.age"), "a-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.BitbucketConfig{
		Enabled: true,
		APIBase: config.BitbucketAPIBase,
		Auth:    config.BitbucketAuthAccessToken,
	}

	// An empty vault: the state of every machine on the morning Bitbucket is turned on.
	_, err = bitbucket.New(cfg, v, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a client was built with no credentials at all")
	}
	// The error has to name the key, because the next thing the operator does is set it.
	if !strings.Contains(err.Error(), vault.KeyBitbucketHookSec) {
		t.Errorf("the error does not name the missing key: %v", err)
	}

	// The webhook secret alone is still not enough, and this is the ordinary middle of
	// setup: it is generated first, pasted into Bitbucket's form, and the access token
	// arrives afterwards.
	if err := v.Set(vault.KeyBitbucketHookSec, vault.NewSecretString("generated")); err != nil {
		t.Fatal(err)
	}
	if _, err := bitbucket.New(cfg, v, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a client was built with a webhook secret and no access token")
	}

	// Both present: the client appears, which is what rewireBitbucket installs.
	if err := v.Set(vault.KeyBitbucketAccessToken, vault.NewSecretString("a-token")); err != nil {
		t.Fatal(err)
	}
	bb, err := bitbucket.New(cfg, v, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("both credentials stored and still no client: %v", err)
	}
	if bb.Platform() != model.PlatformBitbucket {
		t.Errorf("platform = %q", bb.Platform())
	}
}

// TestHasSCMCountsBitbucket. Without this the daemon warns "no source control is
// configured, so no webhooks can arrive" on a Bitbucket-only install, which sends the
// operator to fix something that is not broken.
func TestHasSCMCountsBitbucket(t *testing.T) {
	w := &wiring{
		cfg:     config.Server{Bitbucket: config.BitbucketConfig{Enabled: true}},
		clients: nil,
	}
	if !w.hasSCM() {
		t.Error("a Bitbucket-only install reports no source control")
	}
}
