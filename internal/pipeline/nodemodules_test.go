package pipeline

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
)

// TestNodeModulesGetsAVolume pins the single largest performance decision in the
// docker driver.
//
// Measured with an identical warm package cache, installing this project's own www/
// lockfile: 5m46s with node_modules on the bind mount, 14s with it on a volume. 1325
// packages is tens of thousands of small files, and every one of them was crossing
// WSL into NTFS. See TestWhereNodeModulesShouldLive for the harness.
//
// So this is not a preference. Removing the mount makes every build twenty times
// slower with nothing in any log to say why, which is the kind of change a test has
// to stand in the way of. No docker needed — the argument list is the assertion.
func TestNodeModulesGetsAVolume(t *testing.T) {
	args := createArgsFor(t, "www")
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "type=volume,target=/workspace/www/node_modules") {
		t.Errorf("node_modules is not on a volume, so it writes through the bind mount:\n%s", joined)
	}

	// npm resolves node_modules from the directory it runs in, so a volume at the
	// workspace root is mounted where npm never looks and the install goes through
	// the mount anyway. That failure costs five minutes and looks like nothing.
	if strings.Contains(joined, "type=volume,target=/workspace/node_modules ") {
		t.Error("the volume is at the workspace root, where npm building in www/ will not use it")
	}
}

// TestNodeModulesVolumeFollowsTheBuildDir — the target is derived from the build
// directory, so a wrong derivation is the way this silently stops working.
func TestNodeModulesVolumeFollowsTheBuildDir(t *testing.T) {
	for _, c := range []struct{ buildDir, want string }{
		{"", "type=volume,target=/workspace/node_modules"},
		{"www", "type=volume,target=/workspace/www/node_modules"},
		{"docs/site", "type=volume,target=/workspace/docs/site/node_modules"},
	} {
		got := strings.Join(createArgsFor(t, c.buildDir), " ")
		if !strings.Contains(got, c.want) {
			t.Errorf("build dir %q: no %s in\n%s", c.buildDir, c.want, got)
		}
	}
}

// TestWorkspaceIsStillBindMounted — the volume is an addition, not a replacement.
// The built site has to land on the host, which is what the bind mount is for.
func TestWorkspaceIsStillBindMounted(t *testing.T) {
	got := strings.Join(createArgsFor(t, "www"), " ")
	if !strings.Contains(got, "type=bind,source=/mnt/") && !strings.Contains(got, "type=bind,source=/") {
		t.Errorf("the workspace is not bind-mounted, so the build output cannot reach the host:\n%s", got)
	}
	if !strings.Contains(got, ",target=/workspace ") {
		t.Errorf("the workspace bind mount is not at /workspace:\n%s", got)
	}
}

// createArgsFor builds the create arguments for one build directory.
//
// containerDir is computed the way buildDocker computes it, which is the one piece of
// duplication here — the alternative is a test that cannot see the path it is
// asserting about.
func createArgsFor(t *testing.T, buildDir string) []string {
	t.Helper()

	containerDir := "/workspace"
	if buildDir != "" {
		containerDir += "/" + buildDir
	}

	b := &Builder{
		defaults: config.BuildDefaults{Driver: "docker", Image: "node:24-bookworm-slim",
			CacheDir: t.TempDir()},
		log: slog.New(slog.DiscardHandler),
	}
	cfg := config.RepoConfig{}
	cfg.Build.Dir = buildDir
	cfg.Build.Output = "build"
	cfg.Build.Command = "npm run build"

	args, err := b.createArgs(
		model.PullRequest{Repo: model.Repo{Platform: model.PlatformGitHub,
			Owner: "acme", Name: "docs"}, Number: 1},
		// No env file: these tests assert the two mount arguments, and the environment
		// has its own tests. An empty path means no --env-file argument at all.
		cfg, "/mnt/d/ws", containerDir, t.TempDir(), "node:24-bookworm-slim", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return args
}
