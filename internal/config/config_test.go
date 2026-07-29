package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "/"},
		{"/", "/"},
		{"docs", "/docs/"},
		{"/docs", "/docs/"},
		{"docs/", "/docs/"},
		{"/docs/", "/docs/"},
		{"/zrok", "/zrok/"},
		{"//docs//", "/docs/"},
		{"  /docs/  ", "/docs/"},
	}
	for _, tt := range tests {
		got, err := NormalizeBaseURL(tt.in)
		if err != nil {
			t.Errorf("NormalizeBaseURL(%q) errored: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeBaseURLRejectsNonPaths(t *testing.T) {
	for _, in := range []string{"https://example.com/docs/", "/docs/../../etc/"} {
		if _, err := NormalizeBaseURL(in); err == nil {
			t.Errorf("NormalizeBaseURL(%q) should have failed", in)
		}
	}
}

func TestLoadRepoConfigMissingFileYieldsDefaults(t *testing.T) {
	cfg, err := LoadRepoConfig(t.TempDir())
	if err != nil {
		t.Fatalf("LoadRepoConfig on an empty directory: %v", err)
	}
	if cfg.Build.BaseURL != "/" {
		t.Errorf("default base URL = %q, want %q", cfg.Build.BaseURL, "/")
	}
	if len(cfg.Detect.Paths) == 0 {
		t.Error("default config has no detect paths")
	}
}

func TestLoadRepoConfigNormalizesBaseURL(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "build:\n  base_url: zrok\n")

	cfg, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatalf("LoadRepoConfig: %v", err)
	}
	if cfg.Build.BaseURL != "/zrok/" {
		t.Errorf("base URL = %q, want %q", cfg.Build.BaseURL, "/zrok/")
	}
}

func TestLoadRepoConfigRejectsEscapingPaths(t *testing.T) {
	// Repo config comes from a pull request. A build.dir of "../.." would point
	// the build at whatever sits beside the workspace.
	cases := map[string]string{
		"parent traversal": "build:\n  dir: ../..\n",
		"absolute path":    "build:\n  dir: /etc\n",
		"drive qualified":  "build:\n  output: 'C:\\Windows'\n",
		"escaping script":  "detect:\n  script: ../../evil.sh\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, body)
			if _, err := LoadRepoConfig(dir); err == nil {
				t.Fatalf("LoadRepoConfig accepted %q", body)
			}
		})
	}
}

func TestDefaultServerIsValid(t *testing.T) {
	cfg := DefaultServer()
	if err := cfg.validate(); err != nil {
		t.Fatalf("the default server config does not validate: %v", err)
	}
}

func TestLoadServerRejectsUnknownExposer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("exposer:\n  kind: ngrok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServer(path); err == nil {
		t.Fatal("LoadServer accepted an unknown exposer kind")
	}
}

// loadServer writes a config file and loads it, which is the only way to
// exercise the listener spellings: the defaulting, the custom decoders and the
// normalization all run inside LoadServer.
func loadServer(t *testing.T, body string) (Server, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadServer(path)
}

func TestListenStringStillWorks(t *testing.T) {
	// The single-address spelling is in every doc, every example and most of
	// the tests. Breaking it would be churn with no benefit, so it has to keep
	// producing exactly one TCP listener.
	cfg, err := loadServer(t, "listen: \"127.0.0.1:9999\"\n")
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if len(cfg.Listeners) != 1 || cfg.Listeners[0].TCP != "127.0.0.1:9999" {
		t.Fatalf("listeners = %+v, want one tcp 127.0.0.1:9999", cfg.Listeners)
	}
	if cfg.FirstTCPAddr() != "127.0.0.1:9999" {
		t.Errorf("FirstTCPAddr = %q", cfg.FirstTCPAddr())
	}
}

func TestNoConfigFileStillHasAListener(t *testing.T) {
	// A missing file returns early, before validate. If that path skipped
	// normalization, Listeners would be empty and the daemon would start with
	// nothing bound at all.
	cfg, err := LoadServer(filepath.Join(t.TempDir(), "absent.yml"))
	if err != nil {
		t.Fatalf("LoadServer on a missing file: %v", err)
	}
	if len(cfg.Listeners) != 1 || cfg.Listeners[0].TCP != "127.0.0.1:8471" {
		t.Fatalf("listeners = %+v, want the default tcp listener", cfg.Listeners)
	}
}

func TestListenersMixTCPAndZiti(t *testing.T) {
	cfg, err := loadServer(t, `
listeners:
  - tcp: "127.0.0.1:8471"
  - ziti:
      identity_file: "C:\\path\\docpreview-host.json"
      service: "docpreview-admin"
  - "0.0.0.0:9000"
`)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if len(cfg.Listeners) != 3 {
		t.Fatalf("listeners = %+v, want 3", cfg.Listeners)
	}
	if cfg.Listeners[0].TCP != "127.0.0.1:8471" {
		t.Errorf("listeners[0] = %+v", cfg.Listeners[0])
	}
	z := cfg.Listeners[1].Ziti
	if z == nil || z.Service != "docpreview-admin" || z.IdentityFile != `C:\path\docpreview-host.json` {
		t.Errorf("listeners[1].ziti = %+v", z)
	}
	// The bare-string form is shorthand for tcp. Without it a list of plain
	// addresses — the first thing anyone writes — is a parse error.
	if cfg.Listeners[2].TCP != "0.0.0.0:9000" {
		t.Errorf("listeners[2] = %+v, want the bare string read as tcp", cfg.Listeners[2])
	}
}

func TestListenersRejectBadEntries(t *testing.T) {
	// Every one of these otherwise surfaces at bind time or later: an
	// unbindable address after every other component has connected, or a ziti
	// listener that authenticates against nothing on the first request.
	cases := map[string]string{
		"listen and listeners together": "listen: \"127.0.0.1:8471\"\n" +
			"listeners:\n  - tcp: \"127.0.0.1:9000\"\n",
		"ziti with no identity": "listeners:\n  - ziti:\n      service: \"admin\"\n",
		"ziti with no service":  "listeners:\n  - ziti:\n      identity_file: \"id.json\"\n",
		"two kinds in one entry": "listeners:\n  - tcp: \"127.0.0.1:1\"\n" +
			"    ziti:\n      identity_file: \"id.json\"\n      service: \"admin\"\n",
		"unknown kind":      "listeners:\n  - quic: \"127.0.0.1:1\"\n",
		"empty entry":       "listeners:\n  - {}\n",
		"not a host:port":   "listeners:\n  - tcp: \"nonsense\"\n",
		"listen not a port": "listen: \"nonsense\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadServer(t, body); err == nil {
				t.Fatalf("LoadServer accepted:\n%s", body)
			}
		})
	}
}

func TestListenerDescribeNamesTheService(t *testing.T) {
	// doctor and the startup log line are the only places an operator sees
	// which listeners exist, and a ziti listener has no address worth printing
	// — Addr() is an overlay detail that says nothing about the service bound.
	l := Listener{Ziti: &ZitiListener{IdentityFile: "id.json", Service: "docpreview-admin"}}
	if got := l.Describe(); got != "ziti service docpreview-admin (identity id.json)" {
		t.Errorf("Describe = %q", got)
	}
	if got := (Listener{TCP: "127.0.0.1:8471"}).Describe(); got != "tcp 127.0.0.1:8471" {
		t.Errorf("Describe = %q", got)
	}
}

func TestFirstTCPAddrIsEmptyForOverlayOnly(t *testing.T) {
	// `sim` writes a git hook that curls the ingress, and doctor prints a URL.
	// Both need to know that there is no such address rather than printing
	// "http:///pr".
	cfg, err := loadServer(t, "listeners:\n  - ziti:\n"+
		"      identity_file: \"id.json\"\n      service: \"admin\"\n")
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if got := cfg.FirstTCPAddr(); got != "" {
		t.Errorf("FirstTCPAddr = %q, want empty", got)
	}
}

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, RepoConfigName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
