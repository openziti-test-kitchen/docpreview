package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/vault"
)

// captureStderr runs fn with stderr and stdout redirected, returning what each received.
//
// The command's guidance goes to stderr and only a key ever goes to stdout, so both have to be
// readable to assert on either.
func captureOutput(t *testing.T, fn func() error) (stdout, stderr string, err error) {
	t.Helper()

	outR, outW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	errR, errW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}

	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	err = fn()

	os.Stdout, os.Stderr = savedOut, savedErr
	outW.Close()
	errW.Close()

	outBytes := readAll(t, outR)
	errBytes := readAll(t, errR)
	return string(outBytes), string(errBytes), err
}

func readAll(t *testing.T, f *os.File) []byte {
	t.Helper()
	defer f.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := f.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf
		}
	}
}

// A vault unlocked from the environment has no key source to write to, and the guidance must name
// what will actually read the key back.
//
// `KeySource.Describe()` answers "none" for an unconfigured source, so the not-a-file branch told the
// operator "the vault now needs the key above, and none cannot be written by this command. Store it
// wherever that source reads from" — there is no source, and nothing to store it in. This is a
// supported configuration: the resolution order is key_source, then $DOCPREVIEW_MASTER_KEY, then a
// prompt.
func TestVaultRekeyWithNoKeySourceNamesTheEnvironmentRatherThanNone(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yml")
	if err := os.WriteFile(configPath,
		[]byte("data_dir: "+filepath.ToSlash(dataDir)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Unlocked the way an operator with no key_source does it.
	t.Setenv(vault.MasterKeyEnv, "8086")
	vaultPath := filepath.Join(dataDir, "vault.age")
	v, err := vault.OpenWithKey(vaultPath, "8086")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("github.private_key", vault.NewSecretString("pem")); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := captureOutput(t, func() error {
		return cmdVaultRekey([]string{"-config", configPath, "-generate", "-yes"})
	})
	if err != nil {
		t.Fatal(err)
	}

	newKey := strings.TrimSpace(stdout)
	if !strings.HasPrefix(newKey, "AGE-SECRET-KEY-") {
		t.Fatalf("the new key was not printed to stdout: %q", stdout)
	}
	if strings.Contains(stderr, "none cannot be written") || strings.Contains(stderr, "that source") {
		t.Errorf("the guidance still refers to a source that does not exist:\n%s", stderr)
	}
	if !strings.Contains(stderr, vault.MasterKeyEnv) {
		t.Errorf("the guidance does not name %s, which is what will read this key back:\n%s",
			vault.MasterKeyEnv, stderr)
	}

	// And the vault really was re-encrypted, whatever the message said.
	if _, err := vault.OpenWithKey(vaultPath, newKey); err != nil {
		t.Errorf("the printed key does not open the vault: %v", err)
	}
	if _, err := vault.OpenWithKey(vaultPath, "8086"); err == nil {
		t.Error("the old passphrase still opens the vault")
	}
}

// With no vault yet there is nothing to back up, and the output must not claim otherwise.
//
// The sentinel for "no backup taken" was printed where a path goes, producing "backup (no existing
// vault) (opens with the previous key)" — a filename that does not exist, and a promise about a key
// that was never used. Minting a strong key before storing anything is a reasonable first step on a
// fresh install, so this path is reachable on day one.
func TestVaultRekeyWithNoVaultYetClaimsNoBackup(t *testing.T) {
	configPath, keyPath, vaultPath := rekeyFixture(t, "8086")

	// Remove the vault the fixture created, keeping the config and the key file.
	if err := os.Remove(vaultPath); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := captureOutput(t, func() error {
		return cmdVaultRekey([]string{"-config", configPath, "-generate", "-yes"})
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(stderr, "(no existing vault)") {
		t.Errorf("a sentinel was printed as a file path:\n%s", stderr)
	}
	if !strings.Contains(stderr, "No backup") {
		t.Errorf("the output does not say that no backup was taken:\n%s", stderr)
	}
	if b := backupsIn(t, filepath.Dir(vaultPath)); len(b) != 0 {
		t.Errorf("a backup was written for a vault that did not exist: %v", b)
	}

	// The new key is installed and the vault it now describes is openable — empty, but real.
	installed, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	newKey := strings.TrimSpace(string(installed))
	v, err := vault.OpenWithKey(vaultPath, newKey)
	if err != nil {
		t.Fatalf("the vault is not openable with the installed key: %v", err)
	}
	if n := len(v.Keys()); n != 0 {
		t.Errorf("%d secrets in a vault that did not exist", n)
	}
}

// A vault with no secrets rekeys, and reports zero rather than failing.
func TestVaultRekeyWithAnEmptyVault(t *testing.T) {
	configPath, keyPath, vaultPath := rekeyFixture(t, "8086")

	// Empty it, keeping the file.
	v, err := vault.OpenWithKey(vaultPath, "8086")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Delete("github.private_key"); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := captureOutput(t, func() error {
		return cmdVaultRekey([]string{"-config", configPath, "-generate", "-yes"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "0 secrets") {
		t.Errorf("the count is wrong for an empty vault:\n%s", stderr)
	}

	installed, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.OpenWithKey(vaultPath, strings.TrimSpace(string(installed))); err != nil {
		t.Errorf("an emptied vault did not survive the rekey: %v", err)
	}
}

// Whitespace-only stdin is refused, and refused before anything is written.
//
// `trimTrailingNewline` strips newlines and not spaces, so " \n" passed the command's own emptiness
// check, wrote a backup, and was then refused two steps later by Rekey's TrimSpace — an error naming
// no fix, plus a stray backup file. The other two refusal paths are asserted to leave none.
func TestVaultRekeyRefusesWhitespaceStdinWithoutWritingABackup(t *testing.T) {
	configPath, keyPath, vaultPath := rekeyFixture(t, "8086")

	stdin := pipeStdin(t, "   \n\t\n")
	defer stdin()

	err := cmdVaultRekey([]string{"-config", configPath, "-yes"})
	if err == nil {
		t.Fatal("whitespace was accepted as a master key")
	}
	if !strings.Contains(err.Error(), "-generate") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
	if b := backupsIn(t, filepath.Dir(vaultPath)); len(b) != 0 {
		t.Errorf("a refused rekey left %d backup(s)", len(b))
	}
	if raw, _ := os.ReadFile(keyPath); strings.TrimSpace(string(raw)) != "8086" {
		t.Error("the key file was changed by a refused rekey")
	}
	if _, err := vault.OpenWithKey(vaultPath, "8086"); err != nil {
		t.Errorf("the vault was changed by a refused rekey: %v", err)
	}
}

// A passphrase piped in is accepted and trimmed, so the surrounding whitespace a shell adds does not
// become part of the key — which would be undiscoverable later, since the only symptom is a vault
// that no longer opens with what was typed.
func TestVaultRekeyAcceptsAPassphraseAndTrimsIt(t *testing.T) {
	configPath, keyPath, vaultPath := rekeyFixture(t, "8086")

	stdin := pipeStdin(t, "  a-much-longer-passphrase-than-8086  \n")
	defer stdin()

	if err := cmdVaultRekey([]string{"-config", configPath, "-yes"}); err != nil {
		t.Fatal(err)
	}

	installed, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(installed)); got != "a-much-longer-passphrase-than-8086" {
		t.Errorf("the installed key is %q, want the trimmed passphrase", got)
	}
	if _, err := vault.OpenWithKey(vaultPath, "a-much-longer-passphrase-than-8086"); err != nil {
		t.Errorf("the trimmed passphrase does not open the vault: %v", err)
	}
}

// Two rekeys in the same second each keep their own backup.
//
// The name carries a second-resolution stamp, and the write used to truncate whatever was there — so
// the first rekey's backup, the only thing that opens the vault under the original key, could be
// replaced by the second's with no error.
func TestTwoRekeysInOneSecondKeepBothBackups(t *testing.T) {
	configPath, keyPath, vaultPath := rekeyFixture(t, "8086")

	for i := 0; i < 2; i++ {
		if err := cmdVaultRekey([]string{"-config", configPath, "-generate", "-yes"}); err != nil {
			t.Fatalf("rekey %d: %v", i+1, err)
		}
	}

	backups := backupsIn(t, filepath.Dir(vaultPath))
	if len(backups) != 2 {
		t.Fatalf("%d backups after two rekeys, want 2: %v", len(backups), backups)
	}

	// The first backup is the original vault, which only the original passphrase opens.
	opened := 0
	for _, b := range backups {
		if _, err := vault.OpenWithKey(b, "8086"); err == nil {
			opened++
		}
	}
	if opened != 1 {
		t.Errorf("%d of %d backups open with the original key, want exactly 1 — "+
			"the first rekey's backup was overwritten", opened, len(backups))
	}

	installed, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.OpenWithKey(vaultPath, strings.TrimSpace(string(installed))); err != nil {
		t.Errorf("the vault does not open with the last installed key: %v", err)
	}
}

// pipeStdin replaces os.Stdin with the given text, returning a restore func.
func pipeStdin(t *testing.T, text string) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(text); err != nil {
		t.Fatal(err)
	}
	w.Close()

	saved := os.Stdin
	os.Stdin = r
	return func() {
		os.Stdin = saved
		r.Close()
	}
}
