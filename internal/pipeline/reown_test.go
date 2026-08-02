package pipeline

import (
	"strings"
	"testing"
)

// The container's exit status must be the build's, not the chown's.
//
// The chown runs after the build so that root-owned output can be read, copied and eventually
// removed by the unprivileged account the daemon runs as. Appending it naively breaks one of two
// things, and the wrong one is silent:
//
//   - `build && chown` skips the chown when the build fails, leaving root-owned files in the
//     workspace. The daemon cannot then remove that workspace, which breaks the *next* build of
//     the same preview rather than this one.
//   - `build ; chown` makes the shell's status the chown's, so **every failed build reports
//     success** — a green comment on a pull request whose site did not build.
//
// The second is what this test exists for. It was written, reviewed and nearly shipped.
func TestTheBuildsExitStatusSurvivesTheChown(t *testing.T) {
	script := buildScript("npm ci", "npm run build", "chown -R 1000:1000 /workspace")

	if !strings.Contains(script, "rc=$?") {
		t.Fatalf("the script does not capture the build's exit status:\n%s", script)
	}
	if !strings.HasSuffix(strings.TrimSpace(script), "exit $rc") {
		t.Fatalf("the script does not end by re-raising the build's status:\n%s", script)
	}

	// Capture, then chown, then re-raise. Any other order makes the capture pointless.
	rc := strings.Index(script, "rc=$?")
	chown := strings.Index(script, "chown")
	exit := strings.LastIndex(script, "exit $rc")
	if rc > chown || chown > exit {
		t.Errorf("capture, chown and exit are out of order (%d, %d, %d):\n%s",
			rc, chown, exit, script)
	}

	// And the chown is not conditional on the build succeeding — that is the other half.
	if strings.Contains(script, "build && chown") {
		t.Error("the chown only runs on success, so a failed build leaves files nothing can remove")
	}
}

// With nothing to append the script is exactly what it always was. Asserted so the Windows and
// macOS path cannot acquire a stray `; exit $rc` that changes how a build reports.
func TestWithNoChownTheScriptIsUnchanged(t *testing.T) {
	script := buildScript("npm ci", "npm run build", "")
	if script != "npm ci && npm run build" {
		t.Errorf("script = %q", script)
	}
}

// The chown names this host's user, prunes the node_modules volume, and tolerates failure.
func TestTheChownCommand(t *testing.T) {
	cmd := reownCommand("/workspace/www", 1000, 1001)

	if !strings.Contains(cmd, "chown 1000:1001") {
		t.Errorf("the chown does not name the uid and gid it was given:\n%s", cmd)
	}
	// The build directory's node_modules, not the workspace root's: npm resolves from the
	// directory it runs in, and the volume is mounted where npm looks. Pruned because it is
	// anonymous, discarded with the container, and holds tens of thousands of files.
	if !strings.Contains(cmd, "-path /workspace/www/node_modules -prune") {
		t.Errorf("the chown does not prune the node_modules volume:\n%s", cmd)
	}
	// On a platform where the ids mean nothing the chown is pointless but must not fail the
	// build.
	if !strings.HasSuffix(cmd, "|| true") {
		t.Errorf("a failing chown would fail the build:\n%s", cmd)
	}
}

// A platform with no meaningful uid appends nothing. Docker Desktop maps ownership on the way
// through, so there is nothing to correct and no reason to run a command over the workspace.
func TestNoChownWhereThereAreNoUnixIds(t *testing.T) {
	if got := reownCommand("/workspace", -1, -1); got != "" {
		t.Errorf("got %q, want nothing appended", got)
	}
}
