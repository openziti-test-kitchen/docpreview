package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/vault"
)

// rekeyFixture writes a config, a master key file and a vault holding one secret, and returns the
// three paths. The layout matches an installed host: the key file outside data_dir, because a key
// beside the vault means one directory read yields both halves and config refuses it.
func rekeyFixture(t *testing.T, key string) (configPath, keyPath, vaultPath string) {
	t.Helper()

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	keyPath = filepath.Join(root, "master.key")
	configPath = filepath.Join(root, "config.yml")

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	yaml := "data_dir: " + filepath.ToSlash(dataDir) + "\n" +
		"vault:\n  key_source: \"file:" + filepath.ToSlash(keyPath) + "\"\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	vaultPath = filepath.Join(dataDir, "vault.age")
	v, err := vault.OpenWithKey(vaultPath, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("github.private_key", vault.NewSecretString("pem-body")); err != nil {
		t.Fatal(err)
	}
	return configPath, keyPath, vaultPath
}

// The command re-encrypts the vault, installs the new key, and leaves a backup that the old key
// still opens.
//
// The backup is the recovery path for the one window this cannot make atomic: between re-encrypting
// the vault and writing the key file, the two disagree.
func TestVaultRekeyReplacesTheKeyAndKeepsABackup(t *testing.T) {
	configPath, keyPath, vaultPath := rekeyFixture(t, "8086")

	if err := cmdVaultRekey([]string{"-config", configPath, "-generate", "-yes"}); err != nil {
		t.Fatal(err)
	}

	installed, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	newKey := strings.TrimSpace(string(installed))
	if newKey == "8086" {
		t.Fatal("the key file still holds the old key")
	}
	if !strings.HasPrefix(newKey, "AGE-SECRET-KEY-") {
		t.Errorf("the installed key is not an age identity: %q", newKey)
	}

	// The vault opens with what is now on disk, which is the property that matters: a daemon
	// restarting reads the key file and must be able to unlock.
	v, err := vault.OpenWithKey(vaultPath, newKey)
	if err != nil {
		t.Fatalf("the installed key does not open the vault: %v", err)
	}
	if got, err := v.Get("github.private_key"); err != nil || got.RevealString() != "pem-body" {
		t.Errorf("the secret did not survive the rekey: %v", err)
	}

	// The old key does not, or nothing was gained.
	if _, err := vault.OpenWithKey(vaultPath, "8086"); err == nil {
		t.Error("the old passphrase still opens the vault")
	}

	// And exactly one backup, which the old key does open.
	backups := backupsIn(t, filepath.Dir(vaultPath))
	if len(backups) != 1 {
		t.Fatalf("%d backups, want 1: %v", len(backups), backups)
	}
	old, err := vault.OpenWithKey(backups[0], "8086")
	if err != nil {
		t.Fatalf("the backup does not open with the previous key: %v", err)
	}
	if got, err := old.Get("github.private_key"); err != nil || got.RevealString() != "pem-body" {
		t.Errorf("the backup does not hold the secret: %v", err)
	}
}

// Without -yes it does nothing, and says why.
//
// A running daemon holds the old recipient in memory and its next write would re-encrypt under the
// old key, silently undoing the rekey. Nothing here can detect that, so the confirmation is the
// only place it can be said.
func TestVaultRekeyRefusesWithoutConfirmation(t *testing.T) {
	configPath, keyPath, vaultPath := rekeyFixture(t, "8086")

	err := cmdVaultRekey([]string{"-config", configPath, "-generate"})
	if err == nil {
		t.Fatal("the rekey ran without -yes")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Errorf("the refusal does not mention the daemon: %v", err)
	}

	if raw, _ := os.ReadFile(keyPath); strings.TrimSpace(string(raw)) != "8086" {
		t.Error("the key file was changed by a refused rekey")
	}
	if _, err := vault.OpenWithKey(vaultPath, "8086"); err != nil {
		t.Errorf("the vault was changed by a refused rekey: %v", err)
	}
	if b := backupsIn(t, filepath.Dir(vaultPath)); len(b) != 0 {
		t.Errorf("a refused rekey left %d backups", len(b))
	}
}

// A wrong current key stops the rekey before it writes anything.
//
// This is the failure worth being careful about: a rekey that could not read the vault but wrote
// anyway would replace a vault full of credentials with an empty one, and report success.
func TestVaultRekeyStopsWhenTheCurrentKeyIsWrong(t *testing.T) {
	configPath, keyPath, vaultPath := rekeyFixture(t, "8086")

	// The key file now disagrees with the vault, which is what a half-finished manual rotation
	// looks like.
	if err := os.WriteFile(keyPath, []byte("not-the-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := cmdVaultRekey([]string{"-config", configPath, "-generate", "-yes"})
	if err == nil {
		t.Fatal("the rekey proceeded with a key that cannot open the vault")
	}
	if !strings.Contains(err.Error(), "current key") {
		t.Errorf("the error does not say which key failed: %v", err)
	}

	// The vault is untouched: the real key still opens it and still holds the secret.
	v, err := vault.OpenWithKey(vaultPath, "8086")
	if err != nil {
		t.Fatalf("the vault was damaged: %v", err)
	}
	if got, err := v.Get("github.private_key"); err != nil || got.RevealString() != "pem-body" {
		t.Errorf("the secret is gone: %v", err)
	}
	if b := backupsIn(t, filepath.Dir(vaultPath)); len(b) != 0 {
		t.Errorf("a failed rekey left %d backups", len(b))
	}
}

// With no key on stdin and no -generate it refuses, rather than rekeying to an empty passphrase —
// which is valid age and would produce a vault anybody can open.
func TestVaultRekeyRefusesAnEmptyStdin(t *testing.T) {
	configPath, _, vaultPath := rekeyFixture(t, "8086")

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	saved := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = saved }()

	if err := cmdVaultRekey([]string{"-config", configPath, "-yes"}); err == nil {
		t.Fatal("an empty stdin was accepted as the new key")
	}
	if _, err := vault.OpenWithKey(vaultPath, "8086"); err != nil {
		t.Errorf("the vault was changed: %v", err)
	}
}

// backupsIn lists the vault backups in a directory.
func backupsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "vault.age.bak-") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}
