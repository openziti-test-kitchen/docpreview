package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/vault"
)

// vault.key_source is the one setting whose value has to be checked against
// another setting: a key file inside data_dir sits beside the vault it decrypts,
// which is not encryption at rest. That is a startup error rather than a caveat
// in the docs, and these tests are what keep it one.

func keySourceConfig(t *testing.T, dataDir, source string) (Server, error) {
	t.Helper()
	// YAML-quoted, because a Windows path is full of backslashes and an
	// unquoted one is a parse error before it is ever validated.
	return loadServer(t, "data_dir: \""+yamlEscape(dataDir)+"\"\n"+
		"vault:\n  key_source: \""+yamlEscape(source)+"\"\n")
}

func yamlEscape(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }

func TestKeySourceInsideDataDirIsRefused(t *testing.T) {
	dataDir := t.TempDir()

	for _, name := range []string{"master.key", filepath.Join("keys", "master.key")} {
		inside := filepath.Join(dataDir, name)
		_, err := keySourceConfig(t, dataDir, "file:"+inside)
		if err == nil {
			t.Fatalf("a key file at %s was accepted", inside)
		}
		// The message has to say why, because the obvious reading of the refusal
		// is that the path is wrong.
		for _, want := range []string{"data_dir", "beside the vault"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q:\n%s", want, err)
			}
		}
	}
}

func TestKeySourceOutsideDataDirIsAccepted(t *testing.T) {
	dataDir := t.TempDir()
	// A sibling directory, and a sibling whose name is a prefix of data_dir's —
	// the second is the case a naive string-prefix check gets wrong.
	for _, outside := range []string{
		filepath.Join(t.TempDir(), "master.key"),
		dataDir + "-keys" + string(filepath.Separator) + "master.key",
	} {
		cfg, err := keySourceConfig(t, dataDir, "file:"+outside)
		if err != nil {
			t.Fatalf("a key file at %s was refused: %v", outside, err)
		}
		if got := cfg.KeySource().Kind(); got != vault.SourceKindFile {
			t.Errorf("KeySource().Kind() = %q, want %q", got, vault.SourceKindFile)
		}
	}
}

func TestExecKeySourceSkipsThePlacementRule(t *testing.T) {
	// There is no file to place. The command string can name a path inside
	// data_dir — `cat` of something in there would be silly, but it is the
	// operator's silliness and not a containment violation this can detect.
	dataDir := t.TempDir()
	cfg, err := keySourceConfig(t, dataDir, "exec:op read op://ops/docpreview")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.KeySource().Kind(); got != vault.SourceKindExec {
		t.Errorf("KeySource().Kind() = %q, want %q", got, vault.SourceKindExec)
	}
}

func TestNoKeySourceIsTheDefault(t *testing.T) {
	// Unset means the daemon starts locked and waits for a person. That is the
	// safe default and it must not require saying anything in the config.
	cfg := DefaultServer()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.KeySource().IsZero() {
		t.Errorf("KeySource() = %q by default, want none", cfg.KeySource().Describe())
	}
}

func TestUnparseableKeySourceIsRefusedAtLoad(t *testing.T) {
	if _, err := keySourceConfig(t, t.TempDir(), "exec:"); err == nil {
		t.Fatal("an exec: source with no command was accepted")
	}
}
