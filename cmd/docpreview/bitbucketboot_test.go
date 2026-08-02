package main

import (
	"log/slog"
	"os"
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
// The daemon must not read the vault during wiring: a failed New() must be survivable, leaving a daemon
// with no Bitbucket client so /webhook/bitbucket answers 501 rather than pretending to verify.
//
// The webhook secret is one the daemon generates, via a Generate button on /secrets, so a daemon that
// refuses to boot without it cannot be used to create it — the operator's only way out would be
// `docpreview vault set` in a terminal, which the setup page exists to avoid.
//
// This test asserts the shape rather than calling setup().
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

	// The webhook secret alone is enough to build a client, deliberately: a Bitbucket access token is
	// scoped to one repository unless a workspace administrator permits wider kinds, and many do not.
	// An operator with only per-project tokens and no workspace-wide one still needs a Bitbucket client
	// and a working webhook endpoint to supply those tokens through.
	if err := v.Set(vault.KeyBitbucketHookSec, vault.NewSecretString("generated")); err != nil {
		t.Fatal(err)
	}
	bb, err := bitbucket.New(cfg, v, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("a client with per-project credentials only was refused: %v", err)
	}
	if bb.Platform() != model.PlatformBitbucket {
		t.Errorf("platform = %q", bb.Platform())
	}

	// A global token is still honoured, as the fallback for repositories with none of
	// their own — which is what a workspace token is for.
	if err := v.Set(vault.KeyBitbucketAccessToken, vault.NewSecretString("a-token")); err != nil {
		t.Fatal(err)
	}
	if _, err := bitbucket.New(cfg, v, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("a client with a global token was refused: %v", err)
	}
}

// TestBothConstructionPathsResolvePerProjectCredentials.
//
// A Bitbucket client is built in two places — setup(), for a daemon whose vault is already unlocked, and
// rewireBitbucket, for one unlocked later from the dashboard — and both must attach the per-project
// credential resolver. Without it on the setup() path, a project's own token in the vault is unreachable
// and "Test credential" reports none, even with vault.key_source set and the token present.
//
// Asserted against the *source*, because the resolver is a function field with no exported reader.
func TestBothConstructionPathsResolvePerProjectCredentials(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	// Every place the client is handed to something must attach the resolver first.
	for _, want := range []string{
		"w.clients[model.PlatformBitbucket] = bb.WithProjectCredentials(",
		"bb = bb.WithProjectCredentials(projectBitbucketCredentials(w))",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("a Bitbucket client is installed without the per-project resolver; "+
				"expected to find %q in main.go", want)
		}
	}

	// And the plain assignment, without it, must not come back.
	if strings.Contains(src, "w.clients[model.PlatformBitbucket] = bb\n") {
		t.Error("setup() installs the Bitbucket client without WithProjectCredentials, " +
			"so a per-project token is invisible until the vault is written to")
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
