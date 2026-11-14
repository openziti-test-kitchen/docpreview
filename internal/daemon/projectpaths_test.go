package daemon

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A project row is held to the same path allowlist as .nfpreview.yml:
// these values reach a docker --mount option string.
func TestSavingAProjectRefusesPathsOutsideTheAllowlist(t *testing.T) {
	h, st := projectFormFixture(t)

	for _, body := range []map[string]string{
		{"build_dir": "a,type=bind,source=/,target=/h"},
		{"build_output": "out,readonly"},
		{"build_dir": "a;reboot"},
		{"build_output": "-v"},
		{"detect_script": "x$(id)"},
		{"build_dir": "../up"},
	} {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := secretCall(t, h, "PUT", "/api/projects/github/acme/docs", string(raw), localCaller)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s was accepted: %d %s", raw, rec.Code, rec.Body)
		}
		if _, err := st.ProjectFor(t.Context(), "github", "acme", "docs"); err == nil {
			t.Fatalf("the project was saved despite the refused path %s", raw)
		}
	}

	// An ordinary row saves, and a Windows separator is stored as a forward slash.
	rec := secretCall(t, h, "PUT", "/api/projects/github/acme/docs",
		`{"build_dir":"docs\\site","build_output":".vitepress/dist","detect_script":"scripts/detect.sh"}`,
		localCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("an ordinary row was refused: %d %s", rec.Code, rec.Body)
	}
	p, err := st.ProjectFor(t.Context(), "github", "acme", "docs")
	if err != nil {
		t.Fatal(err)
	}
	if p.BuildDir != "docs/site" || p.BuildOutput != ".vitepress/dist" {
		t.Errorf("saved dir %q and output %q", p.BuildDir, p.BuildOutput)
	}
}
