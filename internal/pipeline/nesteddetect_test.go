package pipeline

import (
	"context"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// A site that does not live at the repository root is still a documentation change.
//
// The default globs were written for a stock Docusaurus layout, where `docs/`, `src/` and
// `static/` sit beside `package.json` at the top. A repository that assembles its site in a
// subdirectory — `unified-doc/` here — matches none of them, so every change to it was reported as
// "no documentation changes" and skipped.
//
// Live on 6 August 2026: netfoundry/docusaurus-shared#146 changed
// `unified-doc/src/pages/what-is-netfoundry.tsx` and was skipped.
func TestANestedSiteIsStillADocumentationChange(t *testing.T) {
	d := testDetector()
	cfg := config.DefaultRepoConfig()

	for _, tc := range []struct {
		name    string
		changed []string
		build   bool
	}{
		{"a page in a nested site", []string{"unified-doc/src/pages/what-is-netfoundry.tsx"}, true},
		{"its stylesheet", []string{"unified-doc/src/pages/what-is-netfoundry.module.css"}, true},
		{"nested docs", []string{"unified-doc/docs/intro.md"}, true},
		{"a nested static asset", []string{"unified-doc/static/img/logo.svg"}, true},
		{"a nested config", []string{"unified-doc/docusaurus.config.ts"}, true},
		{"nested sidebars", []string{"unified-doc/sidebars.ts"}, true},
		{"a nested manifest", []string{"unified-doc/package.json"}, true},

		// The root layout has to keep working, since it is what the defaults were written for.
		{"docs at the root", []string{"docs/intro.md"}, true},
		{"src at the root", []string{"src/pages/index.tsx"}, true},
		{"a config at the root", []string{"docusaurus.config.js"}, true},

		// And a change that is genuinely not documentation still skips. Defaults that matched
		// everything would make the whole detection step a no-op.
		{"a workflow", []string{".github/workflows/ci.yml"}, false},
		{"a Go file", []string{"cmd/tool/main.go"}, false},
		{"a licence", []string{"LICENSE"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.Detect(context.Background(), &Workspace{}, cfg, tc.changed)
			if err != nil {
				t.Fatal(err)
			}
			if got.Build != tc.build {
				t.Errorf("Detect(%v).Build = %v, want %v — %s",
					tc.changed, got.Build, tc.build, got.Reason)
			}
		})
	}
}
