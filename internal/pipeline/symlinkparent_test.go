package pipeline

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// symlinkOrSkip creates a directory symlink, or skips where this host cannot.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		// Unprivileged Windows cannot create symlinks without developer mode.
		if runtime.GOOS == "windows" {
			t.Skipf("cannot create a symlink on this host: %v", err)
		}
		t.Fatal(err)
	}
}

// outsideDir is a directory outside the workspace holding something that must not be published.
func outsideDir(t *testing.T) string {
	t.Helper()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sub", "master.key"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return outside
}

// The branch commits `x -> <a directory outside the
// workspace>` and sets `build.output: x/sub`. The final component is a real directory with no
// links inside it, so the walk that looks for symlinks in the output finds none.
func TestBuildRefusesAnOutputBehindASymlinkedParent(t *testing.T) {
	outside := outsideDir(t)

	ws := &Workspace{Dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(ws.Dir, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(ws.Dir, "x"))

	// The unit under the build first: this is the check that was missing.
	if _, err := resolveInside(ws.Dir, filepath.Join(ws.Dir, "x", "sub"), "build.output"); err == nil {
		t.Fatal("an output directory behind a symlinked parent was accepted")
	}
	if err := OutputInside(ws, filepath.Join(ws.Dir, "x", "sub")); err == nil {
		t.Fatal("OutputInside accepted a directory behind a symlinked parent")
	}
	// The old check alone still passes it, which is why it was not enough.
	if err := rejectSymlinks(filepath.Join(ws.Dir, "x", "sub")); err != nil {
		t.Fatalf("the premise of this test changed: %v", err)
	}

	// And the whole of Build, under the local driver so it runs without docker. `true`
	// succeeds having written nothing, which is all the exploit needs the build to do.
	if runtime.GOOS == "windows" {
		return
	}
	b := NewBuilder(config.BuildDefaults{Driver: "local"}, slog.New(slog.DiscardHandler))
	cfg := config.DefaultRepoConfig()
	cfg.Build.Command = "true"
	cfg.Build.Output = "x/sub"

	result, err := b.Build(t.Context(), ws, cfg, nil)
	if err == nil {
		t.Fatalf("the build published %s", result.OutputDir)
	}
	if !strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "sub", "master.key")); statErr != nil {
		t.Errorf("the directory outside the workspace was disturbed: %v", statErr)
	}
}

// The same for the build directory: a build must not run in a directory the branch pointed
// somewhere else.
func TestBuildRefusesABuildDirBehindASymlink(t *testing.T) {
	outside := outsideDir(t)
	if err := os.WriteFile(filepath.Join(outside, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	ws := &Workspace{Dir: t.TempDir()}
	symlinkOrSkip(t, outside, filepath.Join(ws.Dir, "site"))

	b := NewBuilder(config.BuildDefaults{Driver: "local"}, slog.New(slog.DiscardHandler))
	cfg := config.DefaultRepoConfig()
	cfg.Build.Command = "true"
	cfg.Build.Dir = "site"

	if _, err := b.Build(t.Context(), ws, cfg, nil); err == nil ||
		!strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("got %v, want a refusal", err)
	}
}

// A symlink that stays inside the workspace is not an escape, and an ordinary directory
// resolves to itself.
func TestResolveInsideAcceptsWhatStaysInside(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInside(root, filepath.Join(root, "real", "out"), "build.output"); err != nil {
		t.Errorf("a plain directory was refused: %v", err)
	}
	if _, err := resolveInside(root, root, "build.dir"); err != nil {
		t.Errorf("the workspace root was refused: %v", err)
	}
	// A sibling whose name merely starts with the workspace's is outside it.
	sibling := root + "-other"
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sibling) })
	if _, err := resolveInside(root, sibling, "build.output"); err == nil {
		t.Error("a sibling directory sharing the workspace's prefix was accepted")
	}

	symlinkOrSkip(t, filepath.Join(root, "real"), filepath.Join(root, "alias"))
	got, err := resolveInside(root, filepath.Join(root, "alias", "out"), "build.output")
	if err != nil {
		t.Fatalf("a link that stays inside was refused: %v", err)
	}
	if filepath.Base(filepath.Dir(got)) != "real" {
		t.Errorf("resolved to %s, want the real directory", got)
	}
}
