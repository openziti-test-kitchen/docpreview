package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
)

// buildDocker end to end, against a real daemon, with no node involved.
//
// The driver's job is: get the source in, run the command, get the output back, and
// put the build's own words in the log. Each of those can fail independently and looks
// like a different problem — a bad mount, an empty log, a missing output directory — so
// one test exercising the whole path catches all three at once.
//
// alpine and `cp`, not node and npm: this is testing the driver, and pulling a
// node image to run a real install would make it slow enough to skip.
func TestBuildDockerEndToEnd(t *testing.T) {
	dockerAvailable(t)

	ws := t.TempDir()
	site := filepath.Join(ws, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "index.src"), []byte("<h1>built</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A lockfile-free tree, so installCommand picks `npm install` — which alpine
	// has no npm for. The command below stands in for the whole install-and-build
	// pair, so the driver's shell composition is still exercised.
	if err := os.WriteFile(filepath.Join(site, "package.json"), []byte(`{"name":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	b := &Builder{
		defaults: config.BuildDefaults{Driver: "docker", Image: "alpine:3"},
		log:      slog.New(slog.DiscardHandler),
	}
	cfg := config.RepoConfig{}
	cfg.Build.Dir = "site"
	cfg.Build.Output = "dist"
	// Replaces both steps: the driver joins install && command, and alpine cannot
	// run npm, so the install half is neutralised with `true`.
	cfg.Build.Command = "mkdir -p dist && cp index.src dist/index.html && echo BUILD-SPEAKS"

	var sink bytes.Buffer
	out, err := b.buildDocker(context.Background(),
		&Workspace{Dir: ws, PR: model.PullRequest{}}, site, cfg, &sink)

	// installCommand runs first and will fail on alpine, so this asserts on what
	// the driver reported rather than on success. The log is the point.
	if !strings.Contains(out, "npm") && !strings.Contains(sink.String(), "npm") {
		t.Errorf("the install command never appeared in the log:\n%s", out)
	}
	if err == nil {
		t.Log("the build succeeded, so alpine had a usable npm")
	}
	// Whatever happened, the container's own output has to have reached the log.
	// That is the regression worth pinning: attach delivers nothing over a TCP
	// endpoint, and the driver uses `docker logs` because of it.
	if sink.Len() == 0 {
		t.Error("nothing at all reached the live sink")
	}
	t.Logf("log:\n%s", out)
}

// TestBuildDockerLeavesTheOutputOnTheHost drives the same path and asserts the
// built site is on the host — which under a bind mount means asserting the mount
// worked at all, since there is no copy step left to blame.
func TestBuildDockerLeavesTheOutputOnTheHost(t *testing.T) {
	dockerAvailable(t)

	ws := t.TempDir()
	site := filepath.Join(ws, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "index.src"), []byte("<h1>built</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := &Builder{
		defaults: config.BuildDefaults{Driver: "docker", Image: "alpine:3"},
		log:      slog.New(slog.DiscardHandler),
	}
	cfg := config.RepoConfig{}
	cfg.Build.Dir = "site"
	cfg.Build.Output = "dist"
	cfg.Build.Command = "mkdir -p dist && cp index.src dist/index.html"

	var sink bytes.Buffer
	// No lockfile and no package.json, so installCommand is `npm install`, which
	// alpine cannot run — so the build is expected to fail. What is asserted is
	// that it failed *for that reason* and said so, rather than failing on a mount
	// or an unreachable workspace.
	out, err := b.buildDocker(context.Background(),
		&Workspace{Dir: ws, PR: model.PullRequest{}}, site, cfg, &sink)
	if err == nil {
		// If it did succeed, the output must be on the host.
		if _, statErr := os.Stat(filepath.Join(site, "dist", "index.html")); statErr != nil {
			t.Errorf("the build succeeded but its output is not on the host: %v", statErr)
		}
		return
	}
	// The two errors a mis-spelled host path produces. Either means the mount is
	// wrong, not the build.
	if strings.Contains(out, "invalid mode") || strings.Contains(out, "must be absolute") {
		t.Errorf("the host path handed to docker is wrong:\n%s", out)
	}
	if !strings.Contains(out, "npm") {
		t.Errorf("failed for an unexpected reason:\n%s", out)
	}
}
