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

	// The webhook secret alone is enough to build a client, and that is deliberate.
	//
	// A Bitbucket access token is scoped to one repository unless a workspace
	// administrator permits the wider kinds, and plenty do not — so an operator may have
	// no workspace-wide token to store at all, only one per project. Requiring a global
	// credential here meant those workspaces got no Bitbucket client, which meant
	// /webhook/bitbucket answered 501, which meant the projects page could not be used to
	// supply the per-project tokens that would have made it work.
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
// There are two places a Bitbucket client is built — setup(), for a daemon whose vault is
// already unlocked, and rewireBitbucket, for one unlocked afterwards from the dashboard —
// and only the second attached the per-project credential resolver. The consequence was
// invisible until somebody restarted: with vault.key_source set, the client came from
// setup(), so every project's own token was unreachable and Test credential answered "no
// Bitbucket credential for owner/repo" about a token sitting in the vault. It worked after
// a vault write and stopped working after a restart, which is the hardest shape of bug to
// believe a report of.
//
// Asserted against the *source*, because the resolver is a function field with no exported
// reader: a wiring bug in a path nobody tests is exactly what this is.
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
