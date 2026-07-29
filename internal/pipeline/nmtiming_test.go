package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWhereNodeModulesShouldLive measures the thing the package cache did not fix.
//
// The cache works — npm writes its _cacache and _logs into the mounted directory, and
// a warm build re-reads them. It bought nothing: a warm `npm ci` of this project's
// own www/ reported "added 1325 packages in 2m", the same as cold. So the two minutes
// was never the download.
//
// The hypothesis this tests is that it is the *write*: `npm ci` extracts 1325
// packages as tens of thousands of small files, and under the docker driver that
// lands on a bind mount crossing WSL into NTFS, where small-file writes are an order
// of magnitude slower than the container's own filesystem.
//
// Two runs, same warm cache, differing only in where node_modules lives. If the
// hypothesis holds, the volume run is much faster and the driver should stop letting
// npm write node_modules onto the bind mount.
//
// Off by default: it pulls a node image and installs a real dependency tree twice.
// Set DOCPREVIEW_NM_TIMING=1 to run it. It logs and does not assert — the numbers are
// host-specific and the point is to decide a design, not to gate a commit.
func TestWhereNodeModulesShouldLive(t *testing.T) {
	if os.Getenv("DOCPREVIEW_NM_TIMING") == "" {
		t.Skip("set DOCPREVIEW_NM_TIMING=1 to measure where node_modules should live")
	}
	dockerAvailable(t)

	// The real lockfile, because the answer depends on the number of files and a
	// synthetic tree would not have it.
	repo, err := filepath.Abs(filepath.Join("..", "..", "www"))
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo, "package-lock.json")
	if _, err := os.Stat(lock); err != nil {
		t.Skipf("no lockfile to install: %v", err)
	}

	cacheHost := t.TempDir()
	cache, err := hostMountPath(cacheHost)
	if err != nil {
		t.Fatal(err)
	}

	// Warm the cache once, so neither timed run pays for the download. Its own
	// workspace, so the install that warms it is not one of the measurements.
	warm := stageLockfile(t, repo)
	t.Log("warming the npm cache…")
	if d, err := timeInstall(t, warm, cache, false); err != nil {
		t.Fatalf("warming failed: %v", err)
	} else {
		t.Logf("cold  (bind mount, empty cache):   %s", d.Round(time.Second))
	}

	onMount := stageLockfile(t, repo)
	d1, err := timeInstall(t, onMount, cache, false)
	if err != nil {
		t.Fatalf("bind-mount run failed: %v", err)
	}
	t.Logf("warm  (node_modules on bind mount): %s", d1.Round(time.Second))

	onVolume := stageLockfile(t, repo)
	d2, err := timeInstall(t, onVolume, cache, true)
	if err != nil {
		t.Fatalf("volume run failed: %v", err)
	}
	t.Logf("warm  (node_modules on a volume):   %s", d2.Round(time.Second))

	if d2 < d1 {
		t.Logf("→ moving node_modules off the bind mount saves %s (%.0f%%)",
			(d1 - d2).Round(time.Second), 100*float64(d1-d2)/float64(d1))
	} else {
		t.Logf("→ no gain from moving node_modules; the cost is elsewhere")
	}
}

// stageLockfile copies just the manifest and lockfile into a fresh directory, which
// is all `npm ci` reads.
func stageLockfile(t *testing.T, repo string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"package.json", "package-lock.json"} {
		body, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// timeInstall runs one `npm ci` and returns how long it took.
//
// With nodeModulesVolume, an anonymous docker volume is mounted over node_modules so
// npm writes to the container's own filesystem rather than through the bind mount.
// Anonymous rather than named: this is measuring the filesystem, not reuse between
// runs, and a named volume left behind would make the next run's numbers a lie.
func timeInstall(t *testing.T, workspace, cache string, nodeModulesVolume bool) (time.Duration, error) {
	t.Helper()

	source, err := hostMountPath(workspace)
	if err != nil {
		return 0, err
	}

	args := []string{"run", "--rm",
		"--mount", "type=bind,source=" + source + ",target=/workspace",
		"--mount", "type=bind,source=" + cache + ",target=/cache/npm",
		"--env", "npm_config_cache=/cache/npm",
		"--workdir", "/workspace",
		"--memory", "4g", "--cpus", "2",
	}
	if nodeModulesVolume {
		args = append(args, "--mount", "type=volume,target=/workspace/node_modules")
	}
	args = append(args, "node:24-bookworm-slim", "npm", "ci", "--no-audit", "--no-fund")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	start := time.Now()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	elapsed := time.Since(start)

	if err != nil {
		return elapsed, err
	}
	// Container stdout does not come back over a TCP endpoint, so this is usually
	// empty. Logged when it is not, because npm's own "in 2m" is worth seeing.
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Logf("npm said: %s", got)
	}
	return elapsed, nil
}
