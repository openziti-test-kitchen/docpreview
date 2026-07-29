package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDurationFieldsActuallyLoad guards a silent-failure risk in the config
// format.
//
// gopkg.in/yaml.v3 has no special handling for time.Duration — it is an int64,
// so a YAML value of `15m` is a string being decoded into an integer field.
// Whether that errors, silently yields zero, or works at all is not obvious
// from reading the struct, and every duration in the config is a timeout or a
// TTL where "silently zero" is a bad outcome: a zero build timeout kills every
// build instantly, and a zero preview TTL reaps every preview on the next
// sweep.
func TestDurationFieldsActuallyLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	body := "" +
		"build:\n" +
		"  timeout: 30m\n" +
		"preview:\n" +
		"  ttl: 24h\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer rejected duration values: %v", err)
	}

	if cfg.Build.Timeout != 30*time.Minute {
		t.Errorf("build.timeout = %v, want 30m", cfg.Build.Timeout)
	}
	if cfg.Preview.TTL != 24*time.Hour {
		t.Errorf("preview.ttl = %v, want 24h", cfg.Preview.TTL)
	}
}

// TestDurationRoundTripsThroughGeneratedForm checks the exact spelling
// `docpreview init` writes, which is time.Duration's own String form.
func TestDurationRoundTripsThroughGeneratedForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	body := "" +
		"build:\n" +
		"  timeout: " + (15 * time.Minute).String() + "\n" +
		"preview:\n" +
		"  ttl: " + (72 * time.Hour).String() + "\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer rejected generated duration values (%q): %v", body, err)
	}
	if cfg.Build.Timeout != 15*time.Minute {
		t.Errorf("build.timeout = %v, want 15m", cfg.Build.Timeout)
	}
	if cfg.Preview.TTL != 72*time.Hour {
		t.Errorf("preview.ttl = %v, want 72h", cfg.Preview.TTL)
	}
}

// TestZeroDurationsAreRejected: a timeout of zero kills every build instantly
// and a TTL of zero reaps every preview on the next sweep. Both are more likely
// to be a typo than an intention.
func TestZeroDurationsAreRejected(t *testing.T) {
	for name, body := range map[string]string{
		"zero timeout": "build:\n  timeout: 0s\n",
		"zero ttl":     "preview:\n  ttl: 0s\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadServer(path); err == nil {
				t.Fatalf("LoadServer accepted %q", body)
			}
		})
	}
}
