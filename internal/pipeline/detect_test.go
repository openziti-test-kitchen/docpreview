package pipeline

import (
	"context"
	"log/slog"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

func testDetector() *Detector {
	return NewDetector(slog.New(slog.DiscardHandler))
}

func TestDetectDefaultGlobsMatchDocusaurusLayout(t *testing.T) {
	d := testDetector()
	cfg := config.DefaultRepoConfig()

	shouldBuild := [][]string{
		{"docs/intro.md"},
		{"docs/guides/deep/nested/page.mdx"},
		{"blog/2026-01-01-post.md"},
		{"docusaurus.config.ts"},
		{"sidebars.ts"},
		{"static/img/logo.svg"},
		{"src/css/custom.css"},
		{"README.md"},
		{"package.json"},
		{"src/main.go", "docs/intro.md"},
	}
	for _, changed := range shouldBuild {
		got, err := d.Detect(context.Background(), nil, cfg, changed)
		if err != nil {
			t.Fatalf("Detect(%v): %v", changed, err)
		}
		if !got.Build {
			t.Errorf("Detect(%v) said skip, want build (%s)", changed, got.Reason)
		}
	}
}

func TestDetectDefaultGlobsSkipCodeOnlyChanges(t *testing.T) {
	d := testDetector()
	cfg := config.DefaultRepoConfig()

	changed := []string{"internal/server/handler.go", "cmd/app/main.go", ".github/workflows/ci.yml"}
	got, err := d.Detect(context.Background(), nil, cfg, changed)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Build {
		t.Errorf("Detect(%v) said build, want skip", changed)
	}
	if got.Reason == "" {
		t.Error("a skip decision must explain itself; the reason goes in the PR comment")
	}
}

func TestDetectNormalizesWindowsSeparators(t *testing.T) {
	// The platform reports forward slashes, but a locally-computed path might
	// not, and a glob like docs/** never matches docs\intro.md.
	d := testDetector()
	cfg := config.DefaultRepoConfig()

	got, err := d.Detect(context.Background(), nil, cfg, []string{`docs\intro.md`})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !got.Build {
		t.Error("a backslash-separated docs path was not recognized")
	}
}

func TestDetectEmptyChangeSetSkips(t *testing.T) {
	d := testDetector()
	got, err := d.Detect(context.Background(), nil, config.DefaultRepoConfig(), nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Build {
		t.Error("an empty change set should not trigger a build")
	}
}

func TestDetectEmptyPatternListBuildsRatherThanGoingSilent(t *testing.T) {
	// An empty pattern list is a configuration mistake. Treating it as "never
	// build" makes a repository silently dead with nothing in the logs.
	d := testDetector()
	cfg := config.DefaultRepoConfig()
	cfg.Detect.Paths = nil

	got, err := d.Detect(context.Background(), nil, cfg, []string{"main.go"})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !got.Build {
		t.Error("an empty pattern list should fail open, not closed")
	}
}

func TestDetectIgnoresMalformedGlob(t *testing.T) {
	d := testDetector()
	cfg := config.DefaultRepoConfig()
	cfg.Detect.Paths = []string{"[", "docs/**"}

	got, err := d.Detect(context.Background(), nil, cfg, []string{"docs/intro.md"})
	if err != nil {
		t.Fatalf("a malformed glob should not fail the build: %v", err)
	}
	if !got.Build {
		t.Error("a valid glob after a malformed one was not evaluated")
	}
}
