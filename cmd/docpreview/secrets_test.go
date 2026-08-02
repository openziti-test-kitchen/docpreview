package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/vault"
)

// buildSecrets is the bridge between `build.secrets` in the config file and the values the builder
// injects, and therefore the values the redactor is built from. If nothing calls it, a configured
// mapping parses and documents fine but never reaches a build: the variable stays unset and the
// redactor scrubs nothing, silently, because an unredacted log looks exactly like a log with no
// secrets in it.
//
// These tests assert that buildSecrets is wired in and resolves what it is given.

func testVault(t *testing.T, entries map[string]string) string {
	t.Helper()

	t.Setenv(vault.MasterKeyEnv, "test-passphrase")
	path := filepath.Join(t.TempDir(), "vault.age")

	v, err := vault.Open(path)
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
	return path
}

func TestBuildSecretsResolvesVaultKeys(t *testing.T) {
	path := testVault(t, map[string]string{"demo.algolia_key": "dpfake_abcdef0123456789"})

	w := &wiring{vaultPath: path}
	w.cfg.Build.Secrets = map[string]string{"ALGOLIA_WRITE_KEY": "demo.algolia_key"}

	got, err := buildSecrets(w)
	if err != nil {
		t.Fatal(err)
	}
	if got["ALGOLIA_WRITE_KEY"] != "dpfake_abcdef0123456789" {
		t.Errorf("ALGOLIA_WRITE_KEY = %q, want the vault value", got["ALGOLIA_WRITE_KEY"])
	}
}

func TestBuildSecretsFailsLoudlyOnAMissingKey(t *testing.T) {
	// The alternative — carrying on with the variable unset — produces a build
	// that succeeds while missing whatever the credential was for, and a
	// redactor built from one fewer value than the operator believes.
	path := testVault(t, map[string]string{"present": "value-long-enough"})

	w := &wiring{vaultPath: path}
	w.cfg.Build.Secrets = map[string]string{"MISSING_ONE": "absent"}

	_, err := buildSecrets(w)
	if err == nil {
		t.Fatal("a missing vault key was accepted")
	}
	// The message has to name both ends of the mapping and the command that
	// fixes it, because the operator's next question is always "which one".
	for _, want := range []string{"MISSING_ONE", "absent", "docpreview vault set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
}

func TestBuildSecretsDoesNotOpenTheVaultWhenNoneAreConfigured(t *testing.T) {
	// A server with the local exposer and no source-control integration needs
	// no secrets. Demanding a passphrase from it would be ceremony, so the
	// vault path here is deliberately nonexistent: touching it would fail.
	w := &wiring{vaultPath: filepath.Join(t.TempDir(), "does-not-exist", "vault.age")}
	w.cfg.Build.Secrets = nil

	got, err := buildSecrets(w)
	if err != nil {
		t.Fatalf("an empty build.secrets should not need the vault: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d secrets, want none", len(got))
	}
}
