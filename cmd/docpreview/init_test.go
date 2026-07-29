package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

func TestYamlStringEscapesControlCharacters(t *testing.T) {
	// Every free-form answer in `init -advanced` is operator-typed, so a pasted
	// value with a stray newline or tab is ordinary, not exotic. An earlier
	// version escaped only backslash and quote and emitted unparseable YAML.
	tests := []struct {
		name  string
		value string
	}{
		{"newline", "line one\nline two"},
		{"carriage return", "one\rtwo"},
		{"tab", "one\ttwo"},
		{"quote", `say "hello"`},
		{"backslash", `C:\Users\you\.docpreview`},
		{"both", `C:\path\"quoted"` + "\n"},
		{"empty", ""},
		{"looks like a bool", "yes"},
		{"looks like a number", "8471"},
		{"leading asterisk", "*.example.com"},
		{"unicode", "café ✓"},
		{"null byte-ish control", "a\x01b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered := yamlString(tt.value)

			if strings.ContainsAny(rendered, "\n\r") {
				t.Fatalf("yamlString(%q) = %q, which spans lines and would break the template",
					tt.value, rendered)
			}

			// Round trip through the loader to prove it is both valid YAML and
			// the same value. The zrok namespace is the carrier because blank
			// is a meaningful value there, so the empty case exercises the
			// escaper rather than tripping a required-field check.
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yml")
			body := "exposer:\n  zrok2:\n    namespace: " + rendered + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := config.LoadServer(path)
			if err != nil {
				t.Fatalf("yamlString(%q) = %q produced unparseable YAML: %v", tt.value, rendered, err)
			}
			if got := cfg.Exposer.Zrok.Namespace; got != tt.value {
				t.Errorf("round trip changed the value: got %q, want %q", got, tt.value)
			}
		})
	}
}

func TestRenderedConfigAlwaysLoads(t *testing.T) {
	// The generated file must satisfy the loader that the daemon uses, for
	// every exposer branch.
	for _, kind := range []string{"zrok2", "frontdoor", "local"} {
		t.Run(kind, func(t *testing.T) {
			cfg := config.DefaultServer()
			cfg.Exposer.Kind = kind
			cfg.DataDir = `C:\Users\someone\.docpreview`
			cfg.Exposer.Zrok.NameTemplate = "{{.Repo.Name}}-{{.Name}}"
			cfg.Exposer.Zrok.AccessGrants = []string{"a@example.com", "b@example.com"}
			cfg.Exposer.Zrok.OauthEmailDomains = []string{"*@example.com"}

			path := filepath.Join(t.TempDir(), "config.yml")
			if err := writeConfig(path, renderConfig(cfg)); err != nil {
				t.Fatalf("writeConfig: %v", err)
			}

			got, err := config.LoadServer(path)
			if err != nil {
				t.Fatalf("the generated config does not load: %v", err)
			}
			if got.Exposer.Kind != kind {
				t.Errorf("exposer kind = %q, want %q", got.Exposer.Kind, kind)
			}
			if got.DataDir != cfg.DataDir {
				t.Errorf("data_dir = %q, want %q", got.DataDir, cfg.DataDir)
			}
			if len(got.Exposer.Zrok.AccessGrants) != 2 {
				t.Errorf("access grants = %v", got.Exposer.Zrok.AccessGrants)
			}
		})
	}
}

func TestWriteConfigLeavesTheTargetAloneWhenInvalid(t *testing.T) {
	// The promise: write a valid config, or fail without writing. An earlier
	// version wrote first and validated second, so a bad value cost you the
	// config you already had.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	const existing = "listen: \"127.0.0.1:9999\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	err := writeConfig(path, "exposer:\n  kind: \"nonsense\"\n")
	if err == nil {
		t.Fatal("writeConfig accepted a config the loader rejects")
	}
	if !strings.Contains(err.Error(), "left alone") {
		t.Errorf("error does not say the target was preserved: %v", err)
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != existing {
		t.Errorf("the existing config was modified:\n%s", after)
	}
}

func TestWriteConfigLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	// Once succeeding, once failing.
	if err := writeConfig(path, renderConfig(config.DefaultServer())); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(path, "workers: -1\n"); err == nil {
		t.Fatal("writeConfig accepted an invalid worker count")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left a temporary file behind: %s", e.Name())
		}
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a, b ,,c , ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if splitList("") != nil {
		t.Error("splitList on an empty string should yield nothing")
	}
}
