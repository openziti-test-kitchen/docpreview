package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// The implicit detect script is absent on most branches, and that is not worth a word in the pull
// request: the globs decide and the reason is theirs alone.
func TestAnAbsentImplicitDetectScriptLeavesItToTheGlobs(t *testing.T) {
	d := testDetector()
	ws := &Workspace{Dir: t.TempDir()}

	cfg := config.DefaultRepoConfig()
	cfg.Build.Dir = "docusaurus"
	cfg.ScopeToBuildDir()
	cfg.Detect.Script = config.DefaultDetectScript(cfg.Build.Dir)
	cfg.Detect.Implicit = true

	got, err := d.Detect(context.Background(), ws, cfg, []string{"docusaurus/docs/intro.md"})
	if err != nil {
		t.Fatalf("an absent implicit script failed the build: %v", err)
	}
	if !got.Build {
		t.Errorf("a change in the build directory was skipped: %s", got.Reason)
	}
	if strings.Contains(got.Reason, "not in this commit") {
		t.Errorf("the reason complains about a script nobody configured: %q", got.Reason)
	}

	got, err = d.Detect(context.Background(), ws, cfg, []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Build {
		t.Errorf("a change outside the build directory was built: %s", got.Reason)
	}
}

func TestTheDefaultDetectScriptIsInTheBuildDirectory(t *testing.T) {
	for dir, want := range map[string]string{
		"docusaurus":    "docusaurus/nfpreview-detect",
		"./docusaurus/": "docusaurus/nfpreview-detect",
		".":             "nfpreview-detect",
		"":              "nfpreview-detect",
	} {
		if got := config.DefaultDetectScript(dir); got != want {
			t.Errorf("DefaultDetectScript(%q) = %q, want %q", dir, got, want)
		}
	}
}

// The detect container is the narrow part of the docker driver: what it must not have is the point.
func TestTheDetectContainerHasNoNetworkAndAReadOnlyCheckout(t *testing.T) {
	args := detectRunArgs(DetectContainer{Image: "node:24-bookworm", CPUs: 2, Memory: "1g"},
		"nfpreview-detect-x", "/var/lib/ws", "docusaurus/nfpreview-detect")

	for _, want := range [][]string{
		{"--network", "none"},
		{"--mount", "type=bind,source=/var/lib/ws,target=/workspace,readonly"},
		{"--entrypoint", "/workspace/docusaurus/nfpreview-detect"},
		{"--pull", "never"},
		{"--cpus", "2"},
		{"--memory", "1g"},
	} {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == want[0] && args[i+1] == want[1] {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s %s in %v", want[0], want[1], args)
		}
	}
	if args[len(args)-1] != "node:24-bookworm" {
		t.Errorf("the image is not last, so docker reads it as something else: %v", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--env-file") {
			t.Errorf("the build's environment reached the detect container: %v", args)
		}
	}
}

// End to end against a real docker: the script runs in the container, reads its stdin, and its 78
// is a skip. It also cannot write to the checkout or reach the network.
func TestADetectScriptRunsInTheContainer(t *testing.T) {
	dockerAvailable(t)
	if runtime.GOOS == "windows" {
		t.Skip("NTFS carries no executable bit, so the mounted script cannot be its own entrypoint")
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docusaurus"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
touch /workspace/written 2>/dev/null && echo "the checkout is writable" && exit 1
wget -q -T 3 -O /dev/null http://example.com 2>/dev/null && echo "the network is reachable" && exit 1
grep -q '^docusaurus/' && { echo "docs changed"; exit 0; }
echo "nothing under docusaurus"; exit 78
`
	if err := os.WriteFile(filepath.Join(dir, "docusaurus", "nfpreview-detect"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultRepoConfig()
	cfg.Build.Dir = "docusaurus"
	cfg.Detect.Script = config.DefaultDetectScript(cfg.Build.Dir)
	cfg.Detect.Implicit = true

	d := testDetector().InContainer(DetectContainer{Image: "alpine:3", Memory: "256m"})
	ws := &Workspace{Dir: dir}

	got, err := d.Detect(context.Background(), ws, cfg, []string{"docusaurus/docs/a.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Build || got.Reason != "docs changed" {
		t.Errorf("got %+v, want a build with the script's own reason", got)
	}

	got, err = d.Detect(context.Background(), ws, cfg, []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Build || got.Reason != "nothing under docusaurus" {
		t.Errorf("got %+v, want the script's skip", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "written")); err == nil {
		t.Error("the detect script wrote to the checkout")
	}
}
