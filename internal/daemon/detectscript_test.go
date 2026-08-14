package daemon

import (
	"strings"
	"testing"
)

// The detect script field is a path, and the refusals say so while somebody is looking at the form.
//
// Live on 2026-08-13: a project was saved with `../scripts/vercel-ignore.sh unified-doc/`, which is
// Vercel's ignore *command*. It saved cleanly, and every build then failed with
// `lstat /var/lib/docpreview/workspaces/<id>/scripts: no such file or directory` — a path nobody
// typed, produced by joining a repository-relative `..` onto a workspace directory the operator has
// never seen. The value was wrong in two ways at once, and the error named neither.
func TestValidDetectScript(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   string // a phrase the refusal must contain, "" to accept
	}{
		{"empty defers to the globs", "", ""},
		{"a path in the repository", ".docpreview/detect", ""},
		{"a path with a dot slash", "./scripts/detect.sh", ""},
		{"a windows-style path", `scripts\detect.bat`, ""},
		{"a dotted filename", "scripts/detect.v2.sh", ""},

		// The failure that prompted this: a command, not a path.
		{"vercel's ignore command", "../scripts/vercel-ignore.sh unified-doc/", "not a command"},
		{"a path with an argument", "scripts/detect.sh --verbose", "not a command"},
		{"a tab before the argument", "scripts/detect.sh\tunified-doc/", "not a command"},

		// Leaving the repository. Under the local driver this runs on the build host.
		{"a parent directory", "../scripts/detect.sh", `".."`},
		{"a parent in the middle", "scripts/../../detect.sh", `".."`},
		{"a windows parent", `..\scripts\detect.sh`, `".."`},

		{"an absolute unix path", "/usr/local/bin/detect", "relative"},
		{"an absolute windows path", `C:\tools\detect.bat`, "relative"},
		{"a rooted path", `\tools\detect.bat`, "relative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := validDetectScript(tc.script)
			if tc.want == "" {
				if got != "" {
					t.Errorf("validDetectScript(%q) refused it: %s", tc.script, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("validDetectScript(%q) accepted it", tc.script)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("validDetectScript(%q) = %q, want it to mention %q", tc.script, got, tc.want)
			}
		})
	}
}

// Every refusal names what to do instead, which is the rule for errors here: the operator's next
// question is always "so what do I put".
func TestEveryDetectScriptRefusalNamesTheFix(t *testing.T) {
	for _, bad := range []string{
		"../scripts/vercel-ignore.sh unified-doc/",
		"scripts/detect.sh --verbose",
		"../detect.sh",
		"/usr/local/bin/detect",
	} {
		why := validDetectScript(bad)
		if why == "" {
			t.Fatalf("%q was accepted", bad)
		}
		if !strings.Contains(why, "repository") {
			t.Errorf("the refusal for %q does not say where the path must point: %s", bad, why)
		}
	}
}
