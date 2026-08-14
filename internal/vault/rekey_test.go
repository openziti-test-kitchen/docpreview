package vault

import (
	"errors"
	"path/filepath"
	"strings"
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
