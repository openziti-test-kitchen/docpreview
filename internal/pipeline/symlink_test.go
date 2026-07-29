package pipeline

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRejectSymlinksCatchesAnEscape covers what the bind mount gave away.
//
// The preview server serves the output directory through http.Dir, which blocks a
// URL that climbs out of the root but follows a symlink that leaves it. Under the
// docker driver the build is untrusted code, so a link planted in the output is a
// way to publish a host file. The copy driver dropped symlinks in transit and never
// had to think about it.
func TestRejectSymlinksCatchesAnEscape(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "index.html"), []byte("<h1>ok</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Clean output passes.
	if err := rejectSymlinks(out); err != nil {
		t.Fatalf("a plain directory was rejected: %v", err)
	}

	target := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(target, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(out, "assets", "leak")
	if err := os.Symlink(target, link); err != nil {
		// Unprivileged Windows cannot create symlinks without developer mode.
		if runtime.GOOS == "windows" {
			t.Skipf("cannot create a symlink on this host: %v", err)
		}
		t.Fatal(err)
	}

	err := rejectSymlinks(out)
	if err == nil {
		t.Fatal("a symlink in the build output was accepted")
	}
	// The message has to name the file, or the operator sees "the build failed" with
	// nothing to look at.
	if !strings.Contains(err.Error(), "leak") {
		t.Errorf("the error does not name the symlink: %v", err)
	}
}
