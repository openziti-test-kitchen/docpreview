package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canary = "ghs_thisisaverysecrettokenvalue"

func TestSecretNeverRendersItsValue(t *testing.T) {
	// The hazard is mundane: a struct holding a private key reaches a
	// log.Printf("%+v") or a debug endpoint. Every formatting path has to be
	// closed, not just String().
	s := NewSecretString(canary)

	for _, rendered := range []string{
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%q", s),
		fmt.Sprintf("%x", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprintf("%+v", s),
		fmt.Sprint(s),
		s.String(),
	} {
		if strings.Contains(rendered, canary) {
			t.Errorf("a formatting verb leaked the secret: %q", rendered)
		}
	}
}

func TestSecretInsideAStructDoesNotLeak(t *testing.T) {
	// The realistic case: the secret is a field, and the struct is what gets
	// logged.
	type creds struct {
		AppID int
		Key   Secret
	}
	c := creds{AppID: 7, Key: NewSecretString(canary)}

	if out := fmt.Sprintf("%+v", c); strings.Contains(out, canary) {
		t.Errorf("struct formatting leaked the secret: %q", out)
	}
}

func TestSecretDoesNotMarshalToJSON(t *testing.T) {
	raw, err := json.Marshal(map[string]Secret{"key": NewSecretString(canary)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(canary)) {
		t.Errorf("JSON encoding leaked the secret: %s", raw)
	}
}

func TestSecretLeaksThroughSlog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	log.Info("configured", "key", NewSecretString(canary))

	if strings.Contains(buf.String(), canary) {
		t.Errorf("slog leaked the secret: %s", buf.String())
	}
}

func TestSecretRevealReturnsTheValue(t *testing.T) {
	s := NewSecretString(canary)
	if got := s.RevealString(); got != canary {
		t.Errorf("Reveal returned %q, want the original value", got)
	}
}

func TestVaultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault.age")

	key, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(MasterKeyEnv, key)

	v, err := Open(path)
	if err != nil {
		t.Fatalf("opening a fresh vault: %v", err)
	}
	if err := v.Set(KeyGitHubWebhookSec, NewSecretString(canary)); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening the vault: %v", err)
	}
	got, err := reopened.Get(KeyGitHubWebhookSec)
	if err != nil {
		t.Fatal(err)
	}
	if got.RevealString() != canary {
		t.Errorf("round trip returned %q", got.RevealString())
	}
}

func TestVaultFileIsActuallyEncrypted(t *testing.T) {
	// The whole point. If this passes trivially because the file is JSON, the
	// vault is a filename and nothing more.
	dir := t.TempDir()
	path := filepath.Join(dir, "vault.age")

	key, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(MasterKeyEnv, key)

	v, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set(KeyGitHubPrivateKey, NewSecretString(canary)); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(canary)) {
		t.Fatal("the vault file contains the plaintext secret")
	}
	if bytes.Contains(raw, []byte(KeyGitHubPrivateKey)) {
		t.Fatal("the vault file leaks secret names")
	}
}

func TestVaultRejectsTheWrongKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault.age")

	first, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(MasterKeyEnv, first)

	v, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("k", NewSecretString("v")); err != nil {
		t.Fatal(err)
	}

	second, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(MasterKeyEnv, second)

	if _, err := Open(path); err == nil {
		t.Fatal("the vault opened with the wrong master key")
	}
}

func TestVaultAcceptsAPassphrase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault.age")

	t.Setenv(MasterKeyEnv, "correct horse battery staple")

	v, err := Open(path)
	if err != nil {
		t.Fatalf("opening with a passphrase: %v", err)
	}
	if err := v.Set("k", NewSecretString(canary)); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening with a passphrase: %v", err)
	}
	got, err := reopened.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if got.RevealString() != canary {
		t.Error("passphrase round trip failed")
	}
}

func TestMustGetNamesTheFix(t *testing.T) {
	// A fresh install should be told what to run, not handed a bare "not
	// found" three layers down the stack.
	t.Setenv(MasterKeyEnv, "passphrase")
	v, err := Open(filepath.Join(t.TempDir(), "vault.age"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = v.MustGet(KeyGitHubPrivateKey)
	if err == nil {
		t.Fatal("MustGet succeeded on an empty vault")
	}
	if !strings.Contains(err.Error(), "docpreview vault set") {
		t.Errorf("error does not name the fix: %v", err)
	}
}

func TestKeysListsNamesNotValues(t *testing.T) {
	t.Setenv(MasterKeyEnv, "passphrase")
	v, err := Open(filepath.Join(t.TempDir(), "vault.age"))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("b", NewSecretString(canary)); err != nil {
		t.Fatal(err)
	}
	if err := v.Set("a", NewSecretString(canary)); err != nil {
		t.Fatal(err)
	}

	keys := v.Keys()
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Fatalf("Keys() = %v, want sorted [a b]", keys)
	}
	for _, k := range keys {
		if strings.Contains(k, canary) {
			t.Error("Keys() returned a value")
		}
	}
}
