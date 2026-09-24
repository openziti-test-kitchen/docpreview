package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// A configured detect script that is not in the commit falls back to the path globs.
//
// A project row names a path and the script lives in the branch, so the two disagree on every branch
// that predates the file or removed it. Failing there is wrong twice over: nothing is wrong with the
// branch, and the pull request gets a red comment naming a path its author has never seen.
//
// Live on 2026-08-27: `netfoundry/docusaurus-shared` had `.docpreview/detect` set on the project
// before the file was committed, and every open pull request failed with
// `lstat .../.docpreview: no such file or directory`.
func TestAMissingDetectScriptFallsBackToTheGlobs(t *testing.T) {
	d := testDetector()
	ws := &Workspace{Dir: t.TempDir()}

	cfg := config.DefaultRepoConfig()
	cfg.Detect.Script = ".docpreview/detect"

	// A documentation change, which the default globs match.
	got, err := d.Detect(context.Background(), ws, cfg, []string{"unified-doc/docs/intro.md"})
	if err != nil {
		t.Fatalf("a missing script failed the build: %v", err)
	}
	if !got.Build {
		t.Errorf("the globs did not decide: %s", got.Reason)
	}
	// The reason has to say why, or the operator sees a build with no explanation of which rule ran.
	if !strings.Contains(got.Reason, ".docpreview/detect") {
		t.Errorf("the reason does not name the missing script: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "not in this commit") {
		t.Errorf("the reason does not say the script was absent: %q", got.Reason)
	}

	// And the globs really are deciding, not a blanket "build". A change the globs do not match must
	// still skip, or the fallback would silently build everything on a repository that configured a
	// script precisely to avoid that.
	got, err = d.Detect(context.Background(), ws, cfg, []string{"cmd/tool/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Build {
		t.Errorf("a non-documentation change was built anyway: %s", got.Reason)
	}
}

// A script that exists and is unreadable is still a failure. Only absence falls back: a symlink
// resolving outside the repository, a permission error, or a script that exits 3 are all real
// failures, and none of them may quietly become "match the globs".
func TestAnUnusableDetectScriptStillFails(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("mode bits do not describe access on Windows, so an unreadable file cannot be staged")
	}

	dir := t.TempDir()
	ws := &Workspace{Dir: dir}

	// Present, and not executable.
	script := filepath.Join(dir, "detect")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultRepoConfig()
	cfg.Detect.Script = "detect"

	d := testDetector()
	if _, err := d.Detect(context.Background(), ws, cfg, []string{"docs/intro.md"}); err == nil {
		t.Error("a script that cannot be executed was treated as absent")
	}
}

// A script that resolves outside the repository is refused rather than fallen back from. The working
// tree is attacker-controlled, so a symlink there pointing at something on the host is the case this
// check exists for, and turning it into a silent fallback would hide it.
// Not skipped by platform: symlink creation on Windows needs privilege or developer mode, and which
// of those is true is a property of the machine rather than of the operating system. So it is
// attempted, and only a machine that refuses skips.
func TestADetectScriptOutsideTheRepositoryIsRefused(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "detect")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	cfg := config.DefaultRepoConfig()
	cfg.Detect.Script = "detect"

	d := testDetector()
	_, err := d.Detect(context.Background(), &Workspace{Dir: repo}, cfg, []string{"docs/intro.md"})
	if err == nil {
		t.Fatal("a script resolving outside the repository was accepted")
	}
	if !strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("the error does not say why it was refused: %v", err)
	}
}
