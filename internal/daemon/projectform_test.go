package daemon

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/store"
)

// projectFormFixture is a projects admin with no vault: these tests are about the
// build fields, and a locked or absent vault must not stop one being saved.
func projectFormFixture(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}

	return NewProjectsAdmin(st, cfg, slog.New(slog.DiscardHandler)).Handler(), st
}

// TestABaseURLIsCorrectedRatherThanRefused. The slashes are mandatory and this code knows where they go,
// so "/docs" saves as "/docs/" instead of coming back as an error somebody has to read and retype.
func TestABaseURLIsCorrectedRatherThanRefused(t *testing.T) {
	h, st := projectFormFixture(t)

	for _, typed := range []string{"/docs", "docs", "docs/", "/docs/", " /docs "} {
		rec := secretCall(t, h, "PUT", "/api/projects/github/acme/docs",
			`{"base_url":`+quote(typed)+`}`, localCaller)
		if rec.Code != http.StatusOK {
			t.Fatalf("saving base_url %q: %d %s", typed, rec.Code, rec.Body)
		}

		p, err := st.ProjectFor(t.Context(), "github", "acme", "docs")
		if err != nil {
			t.Fatal(err)
		}
		if p.BaseURL != "/docs/" {
			t.Errorf("base_url %q saved as %q, want %q", typed, p.BaseURL, "/docs/")
		}
	}
}

// TestBaseURLNormalizationEdges covers the forms that are not simply missing a
// slash: an empty value must stay empty, because "" means "defer to the repository"
// and "/" does not.
func TestBaseURLNormalizationEdges(t *testing.T) {
	cases := map[string]string{
		"":                               "",
		"   ":                            "",
		"/":                              "/",
		"//docs//guide//":                "/docs/guide/",
		"https://docs.example.com/docs/": "/docs/",
		"https://example.com":            "/",
	}
	for in, want := range cases {
		if got := normalizeBaseURL(in); got != want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAnUnusableBaseURLIsStillRefused. Correcting punctuation is one thing; a query
// string is not a path prefix and there is nothing to guess at.
func TestAnUnusableBaseURLIsStillRefused(t *testing.T) {
	h, _ := projectFormFixture(t)
	rec := secretCall(t, h, "PUT", "/api/projects/github/acme/docs",
		`{"base_url":"/docs?v=2"}`, localCaller)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a base URL with a query was accepted: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "query") {
		t.Errorf("the error does not say what is wrong: %s", rec.Body)
	}
}

// TestAProjectBuildTimeoutRoundTrips — saved as typed, and handed back as typed. It
// is redisplayed in the form it was entered, which is why it is stored as text.
func TestAProjectBuildTimeoutRoundTrips(t *testing.T) {
	h, st := projectFormFixture(t)

	rec := secretCall(t, h, "PUT", "/api/projects/github/netfoundry/docusaurus-shared",
		`{"timeout":"45m"}`, localCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("saving a timeout: %d %s", rec.Code, rec.Body)
	}

	p, err := st.ProjectFor(t.Context(), "github", "netfoundry", "docusaurus-shared")
	if err != nil {
		t.Fatal(err)
	}
	if p.Timeout != "45m" {
		t.Fatalf("timeout saved as %q, want %q", p.Timeout, "45m")
	}
}

// TestABadBuildTimeoutIsRefusedWithTheUnit. "45" parses as 45 nanoseconds, so every
// build of that project would die before docker was invoked. The error has to name
// the unit, because the number typed was not the number meant.
func TestABadBuildTimeoutIsRefusedWithTheUnit(t *testing.T) {
	for _, bad := range []string{"45", "soon", "0s", "-5m", "30s", "9h"} {
		if why := validTimeout(bad); why == "" {
			t.Errorf("validTimeout(%q) accepted it", bad)
		}
	}
	for _, good := range []string{"", "1m", "45m", "2h", "6h"} {
		if why := validTimeout(good); why != "" {
			t.Errorf("validTimeout(%q) = %q, want accepted", good, why)
		}
	}
}

// TestABadBuildTimeoutDoesNotSaveTheRest. The whole row is one upsert, so a refused
// field must not leave half a project behind — otherwise a rejected save has silently
// changed the build command.
func TestABadBuildTimeoutDoesNotSaveTheRest(t *testing.T) {
	h, st := projectFormFixture(t)

	rec := secretCall(t, h, "PUT", "/api/projects/github/acme/docs",
		`{"build_command":"npm run build","timeout":"45"}`, localCaller)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a bare number was accepted as a timeout: %d %s", rec.Code, rec.Body)
	}
	if _, err := st.ProjectFor(t.Context(), "github", "acme", "docs"); err == nil {
		t.Fatal("the project was saved despite the refused timeout")
	}
}

// TestTheProjectFormReportsTheServerTimeout — the empty field's placeholder is the
// server-wide value, so "blank" says what it will do rather than nothing at all.
func TestTheProjectFormReportsTheServerTimeout(t *testing.T) {
	h, _ := projectFormFixture(t)

	rec := secretCall(t, h, "GET", "/api/projects", "", localCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/projects: %d %s", rec.Code, rec.Body)
	}

	var got struct {
		Defaults struct {
			Timeout string `json:"timeout"`
		} `json:"defaults"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Defaults.Timeout != config.DefaultServer().Build.Timeout.String() {
		t.Errorf("defaults.timeout = %q, want the server's build.timeout",
			got.Defaults.Timeout)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
