package main

import (
	"testing"

	"github.com/netfoundry/docpreview/internal/vault"
)

// TestAGlobalSecretReachesEveryBuild.
//
// A credential stored from the dashboard must reach every build, not only builds whose config
// file has an explicit `build.secrets` mapping — the dashboard cannot edit that file.
//
// So a vault key shaped like an environment variable is injected under its own name. The
// shape is the discriminator, which is what keeps github.private_key out of every build's
// environment while BB_REPO_TOKEN_ONPREM goes in.
func TestAGlobalSecretReachesEveryBuild(t *testing.T) {
	path := testVault(t, map[string]string{
		"BB_REPO_TOKEN_ONPREM":  "ATCTT3xFfGN0-not-a-real-token",
		"GH_ZITI_CI_REPO_TOKEN": "github_pat_not-a-real-token",
		// Infrastructure. The daemon uses these itself and no build may see them.
		vault.KeyGitHubPrivateKey: "-----BEGIN RSA PRIVATE KEY-----",
		vault.KeyGitHubWebhookSec: "a-webhook-secret",
		vault.KeyFrontdoorToken:   "a-frontdoor-token",
	})

	w := &wiring{vaultPath: path}
	got, err := buildSecrets(w)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"BB_REPO_TOKEN_ONPREM", "GH_ZITI_CI_REPO_TOKEN"} {
		if got[want] == "" {
			t.Errorf("%s is not injected, so a build script looking for it finds nothing", want)
		}
	}
	// The discriminator, and the reason it is the key's shape: these are the daemon's own
	// credentials, they are not shell-shaped, and a build that could read them could clone
	// every repository the App is installed on.
	for _, never := range []string{
		vault.KeyGitHubPrivateKey, vault.KeyGitHubWebhookSec, vault.KeyFrontdoorToken,
	} {
		if _, ok := got[never]; ok {
			t.Errorf("%s is in the build environment", never)
		}
	}
	if len(got) != 2 {
		t.Errorf("injected %d variables, want exactly the two shell-shaped ones: %v",
			len(got), keysOf(got))
	}
}

// TestAnExplicitMappingWinsOverABareKey — build.secrets is a deliberate statement in a
// file only the operator can edit, so it outranks the convention.
func TestAnExplicitMappingWinsOverABareKey(t *testing.T) {
	path := testVault(t, map[string]string{
		"MY_TOKEN":        "from-the-bare-key",
		"elsewhere.token": "from-the-mapping",
	})

	w := &wiring{vaultPath: path}
	w.cfg.Build.Secrets = map[string]string{"MY_TOKEN": "elsewhere.token"}

	got, err := buildSecrets(w)
	if err != nil {
		t.Fatal(err)
	}
	if got["MY_TOKEN"] != "from-the-mapping" {
		t.Errorf("MY_TOKEN = %q, want the configured mapping to win", got["MY_TOKEN"])
	}
}

// TestIsBuildEnvKey pins the rule both the injector and the credential page read, so they
// cannot disagree about which entries do anything.
func TestIsBuildEnvKey(t *testing.T) {
	for _, k := range []string{"BB_REPO_TOKEN_ONPREM", "A", "TOKEN_2", "X_1_Y"} {
		if !vault.IsBuildEnvKey(k) {
			t.Errorf("IsBuildEnvKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{
		"", "github.private_key", "lower_case", "2FA_TOKEN", "has-dash", "project/x/y/Z",
	} {
		if vault.IsBuildEnvKey(k) {
			t.Errorf("IsBuildEnvKey(%q) = true; it would be handed to every build", k)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
