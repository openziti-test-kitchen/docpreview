package pipeline

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
)

// The commit in a build event names the workspace directory and is an argument to git, and on
// the local platform it could arrive in a delivery nobody signed. `../../../../` is exactly
// twelve characters, which is what the directory name keeps: Clone deleted the directory four
// levels above the preview's workspace.
func TestCloneRefusesACommitThatIsNotAnObjectName(t *testing.T) {
	// A layout deep enough that the old behaviour would have deleted `outer`.
	outer := t.TempDir()
	root := filepath.Join(outer, "a", "b", "workspaces")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outer, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("still here"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewCloner(root, slog.New(slog.DiscardHandler))
	for _, sha := range []string{
		"../../../../",
		"..",
		"--upload-pack=touch pwned",
		"-q",
		"",
		"abc123",
		"refs/heads/main",
		strings.Repeat("A", 40),
	} {
		pr := model.PullRequest{
			Repo:    model.Repo{Platform: model.PlatformLocal, Owner: "local", Name: "docs"},
			Number:  1,
			Branch:  "feature",
			HeadSHA: sha,
		}
		ws, err := c.Clone(t.Context(), pr, outer)
		if err == nil {
			t.Errorf("sha %q was cloned into %s", sha, ws.Dir)
		}
		if _, statErr := os.Stat(sentinel); statErr != nil {
			t.Fatalf("sha %q deleted a directory outside the workspace root: %v", sha, statErr)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("a refused clone left %d entries in the workspace root", len(entries))
	}
}

// The other half: a real commit still clones, with `--` in the fetch.
func TestCloneFetchesARealCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	src := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", src,
			"-c", "user.name=test", "-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	if err := os.WriteFile(filepath.Join(src, "page.md"), []byte("# hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plumbing rather than `git commit`, so the test does not run whatever commit hooks the
	// machine it is on has configured.
	git("add", "page.md")
	sha := git("commit-tree", git("write-tree"), "-m", "one")
	git("update-ref", "refs/heads/main", sha)

	c := NewCloner(t.TempDir(), slog.New(slog.DiscardHandler))
	ws, err := c.Clone(t.Context(), model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformLocal, Owner: "local", Name: "docs"},
		Number:  1,
		Branch:  "main",
		HeadSHA: sha,
	}, src)
	if err != nil {
		t.Fatalf("a real commit was not cloned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "page.md")); err != nil {
		t.Errorf("the checkout is missing its file: %v", err)
	}
}
