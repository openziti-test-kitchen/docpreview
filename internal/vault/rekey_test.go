package vault

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Rekey keeps every secret and hands the vault to the new key.
//
// Without this the master key was the one credential that could not be changed: rotating it meant
// re-storing each secret by hand, which needs every plaintext, which is what the vault exists so
// that nobody keeps. A key typed hastily during setup was therefore permanent.
func TestRekeyKeepsEverySecretAndSwitchesTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.age")

	old, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenWithKey(path, old)
	if err != nil {
		t.Fatal(err)
	}

	stored := map[string]string{
		"github.private_key":      "-----BEGIN RSA PRIVATE KEY-----\nnot really\n",
		"github.webhook_secret":   "s3cret",
		"BB_REPO_TOKEN_FRONTDOOR": "bb-token",
	}
	for k, val := range stored {
		if err := v.Set(k, NewSecretString(val)); err != nil {
			t.Fatal(err)
		}
	}

	fresh, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Rekey(fresh); err != nil {
		t.Fatal(err)
	}

	// The new key opens it, with every value intact. A rekey that dropped a secret would be
	// indistinguishable from a successful one until the next build needed that secret.
	reopened, err := OpenWithKey(path, fresh)
	if err != nil {
		t.Fatalf("the new key does not open the vault: %v", err)
	}
	for k, want := range stored {
		got, err := reopened.Get(k)
		if err != nil {
			t.Errorf("%s is gone after the rekey: %v", k, err)
			continue
		}
		if got.RevealString() != want {
			t.Errorf("%s changed value across the rekey", k)
		}
	}
	if n := len(reopened.Keys()); n != len(stored) {
		t.Errorf("%d secrets after the rekey, want %d", n, len(stored))
	}

	// And the old key does not. Otherwise the old key file, or a copy of it, still opens the
	// vault — which is the entire thing being fixed.
	if _, err := OpenWithKey(path, old); err == nil {
		t.Error("the previous key still opens the vault")
	}
}

// Rekey and Save may run against one Vault without racing.
//
// Vault documents itself as safe for concurrent use, and Save read v.recipient with no lock held —
// which was harmless only because the recipient was write-once at construction. Rekey is the first
// thing that mutates it, so the unsynchronized read became a genuine race, and the value it reads
// decides which key the file on disk can be opened with.
//
// Run this with -race; without it, the test passes against the broken code.
func TestRekeyDoesNotRaceWithSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.age")
	first, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenWithKey(path, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("token", NewSecretString("value")); err != nil {
		t.Fatal(err)
	}

	second, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			// Set saves, so this is the ordinary write path a dashboard action takes.
			if err := v.Set("token", NewSecretString("value")); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := v.Rekey(second); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()

	// Whatever the interleaving, the file has to be readable by the key the vault now holds —
	// a torn pair of "these secrets, that key" would be a vault nothing opens.
	reopened, err := OpenWithKey(path, second)
	if err != nil {
		t.Fatalf("the vault does not open with the key it was rekeyed to: %v", err)
	}
	if got, err := reopened.Get("token"); err != nil || got.RevealString() != "value" {
		t.Errorf("the secret did not survive concurrent writes: %v", err)
	}
}

// A failed Save leaves the object on the key the file still has.
//
// Rekey swaps identity and recipient and then writes. If the write fails, an object left holding the
// new key would encrypt its *next* write to a key the caller believes was never installed — and the
// file on disk is still the old key's, since Save renames over the target rather than truncating it.
func TestAFailedSaveLeavesTheVaultOnItsOldKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "vault.age")

	key, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("token", NewSecretString("value")); err != nil {
		t.Fatal(err)
	}

	fresh, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	// A directory where the vault file should be: MkdirAll succeeds, CreateTemp succeeds, and the
	// rename onto a non-empty directory fails. Reached without depending on filesystem permissions,
	// which do not behave the same way on Windows.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := v.Rekey(fresh); err == nil {
		t.Fatal("Rekey reported success with nowhere to write")
	}

	// The object must still be on the old key. Asserted by writing somewhere that works and seeing
	// which key opens the result.
	moved := filepath.Join(dir, "moved.age")
	v.mu.Lock()
	v.path = moved
	v.mu.Unlock()

	if err := v.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithKey(moved, key); err != nil {
		t.Errorf("the vault is no longer on its old key after a failed rekey: %v", err)
	}
	if _, err := OpenWithKey(moved, fresh); err == nil {
		t.Error("a failed rekey left the vault encrypting to the new key")
	}
}

// An empty key is refused rather than accepted as a passphrase. An empty scrypt passphrase is valid
// age and would produce a vault anybody can open — the same reason Open refuses one.
func TestRekeyRefusesAnEmptyKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.age")
	key, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("token", NewSecretString("value")); err != nil {
		t.Fatal(err)
	}

	for _, empty := range []string{"", "   ", "\n", "\t\n "} {
		if err := v.Rekey(empty); !errors.Is(err, ErrLocked) {
			t.Errorf("Rekey(%q) = %v, want ErrLocked", empty, err)
		}
	}

	// And the vault still opens with the key it had, so a refused rekey changed nothing.
	if _, err := OpenWithKey(path, key); err != nil {
		t.Errorf("a refused rekey damaged the vault: %v", err)
	}
}

// An unparsable key is refused before anything is written.
//
// The failure that matters is not the error, it is what the vault is afterwards: a Rekey that
// installed a broken recipient and then saved would leave a file nothing can read.
func TestRekeyRefusesAnUnusableKeyWithoutTouchingTheVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.age")
	key, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	v, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("token", NewSecretString("value")); err != nil {
		t.Fatal(err)
	}

	// An age identity that is the right shape and not valid. A passphrase would be accepted —
	// deliberately, since a passphrase is a legitimate key — so this has to look like an identity.
	if err := v.Rekey("AGE-SECRET-KEY-1NOTAREALKEYATALL"); err == nil {
		t.Fatal("an unusable age key was accepted")
	}

	reopened, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatalf("the vault no longer opens with its own key: %v", err)
	}
	if got, err := reopened.Get("token"); err != nil || got.RevealString() != "value" {
		t.Errorf("the secret did not survive a refused rekey: %v", err)
	}
}

// A passphrase is a valid master key, so rekeying from a weak one to a strong one is the case this
// was built for: `8086` to something that cannot be guessed.
func TestRekeyFromAPassphraseToAnIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.age")

	v, err := OpenWithKey(path, "8086")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("github.private_key", NewSecretString("pem")); err != nil {
		t.Fatal(err)
	}

	strong, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Rekey(strong); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenWithKey(path, "8086"); err == nil {
		t.Error("the four-character passphrase still opens the vault")
	}
	reopened, err := OpenWithKey(path, strong)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Get("github.private_key"); err != nil || got.RevealString() != "pem" {
		t.Errorf("the secret did not survive: %v", err)
	}
}

// Nothing in a rekey's error text carries the key. Both keys pass through this call, and an error
// that quoted either would put a master key in a terminal, a log file, or a support ticket.
func TestRekeyErrorsNeverCarryAKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.age")
	v, err := OpenWithKey(path, "the-current-master-key")
	if err != nil {
		t.Fatal(err)
	}

	const attempted = "AGE-SECRET-KEY-1THISISNOTVALID"
	err = v.Rekey(attempted)
	if err == nil {
		t.Fatal("expected a failure")
	}
	for _, secret := range []string{"the-current-master-key", attempted} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error quotes a key: %v", err)
		}
	}
}
