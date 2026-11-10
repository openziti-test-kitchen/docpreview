package config

import "testing"

func TestListenersReadAZrokPrivateShare(t *testing.T) {
	cfg, err := loadServer(t, `
listeners:
  - tcp: "127.0.0.1:8471"
  - zrok_private:
      token: "nfpadmin"
      access_grants: ["ops@example.com"]
`)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	z := cfg.Listeners[1].ZrokPrivate
	if z == nil || z.Token != "nfpadmin" || len(z.AccessGrants) != 1 || z.AccessGrants[0] != "ops@example.com" {
		t.Fatalf("listeners[1].zrok_private = %+v", z)
	}
	if got := cfg.Listeners[1].Describe(); got != "zrok private share nfpadmin" {
		t.Errorf("Describe = %q", got)
	}
	// The first TCP address is still the one a URL is built from.
	if got := cfg.FirstTCPAddr(); got != "127.0.0.1:8471" {
		t.Errorf("FirstTCPAddr = %q", got)
	}
}

func TestListenersRejectABadZrokPrivateToken(t *testing.T) {
	for _, tok := range []string{"", "abc", "Has-Caps", "has space", "waytoolongtobeazroktokenatallreally"} {
		body := "listeners:\n  - zrok_private:\n      token: \"" + tok + "\"\n"
		if _, err := loadServer(t, body); err == nil {
			t.Errorf("LoadServer accepted token %q", tok)
		}
	}
}
