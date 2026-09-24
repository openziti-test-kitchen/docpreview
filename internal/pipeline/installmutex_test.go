package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lockfileTree writes one lockfile into a temp directory and returns it.
func lockfileTree(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestYarnOneInstallTakesTheCacheMutex is the regression guard for a corruption that is
// invisible without concurrency.
//
// One docker volume holds the dependency cache for every preview of a repository, and yarn 1
// does not lock it. Two installs filling a cold cache at once destroy each other's
// half-extracted entries, and the error names a corrupt tarball rather than contention — so
// the symptom reads as a broken lockfile and sends the next person to the wrong repository.
//
// Asserted on the command string rather than by running yarn, because the failure this
// prevents needs two containers and a cold volume to reproduce. A test that cannot run in
// CI is not the guard; this is.
func TestYarnOneInstallTakesTheCacheMutex(t *testing.T) {
	cmd := installCommand(lockfileTree(t, "yarn.lock"))

	first, _, ok := strings.Cut(cmd, "||")
	if !ok {
		t.Fatalf("expected a fallback chain, got %q", cmd)
	}
	if !strings.Contains(first, "--mutex") {
		t.Errorf("the yarn 1 install does not take the cache mutex:\n  %s", first)
	}
}

// TestTheMutexLivesInTheCacheItProtects — the lock has to be scoped to the thing being
// contended, which is the cache volume, not the container and not the workspace.
//
// Hardcoding the docker driver's mount path would also break the local driver, which has no
// such directory: yarn would fail to create the lock and take the whole install down with it.
// So the path is read from the environment with a fallback, and both halves matter.
func TestTheMutexLivesInTheCacheItProtects(t *testing.T) {
	cmd := installCommand(lockfileTree(t, "yarn.lock"))

	if !strings.Contains(cmd, "${YARN_CACHE_FOLDER") {
		t.Errorf("the mutex path is not derived from the cache folder:\n  %s", cmd)
	}
	if strings.Contains(cmd, "--mutex file:/cache/yarn/") {
		t.Errorf("the mutex path is hardcoded to the docker driver's mount:\n  %s", cmd)
	}
	if !strings.Contains(cmd, ":-/tmp}") {
		t.Errorf("no fallback for a build with no shared cache:\n  %s", cmd)
	}
}

// TestTheYarnBerryFallbackTakesNoMutex — yarn 2+ rejects --mutex outright.
//
// Putting it on the fallback would convert a Berry repository from "installs correctly" to
// "fails on an unknown flag", and Berry needs no equivalent: its cache is content-addressed
// and it locks its own installs. The flag belongs on the yarn 1 command alone.
func TestTheYarnBerryFallbackTakesNoMutex(t *testing.T) {
	cmd := installCommand(lockfileTree(t, "yarn.lock"))

	_, rest, ok := strings.Cut(cmd, "||")
	if !ok {
		t.Fatalf("expected a fallback chain, got %q", cmd)
	}
	if strings.Contains(rest, "--mutex") {
		t.Errorf("a fallback carries --mutex, which yarn 2+ refuses:\n  %s", rest)
	}
	if !strings.Contains(rest, "--immutable") {
		t.Errorf("the yarn 2 fallback is gone:\n  %s", rest)
	}
}

// TestTheOtherManagersAreUntouched — npm's cacache and pnpm's store are both built for
// concurrent access, so neither needs this and neither should grow a yarn flag.
func TestTheOtherManagersAreUntouched(t *testing.T) {
	for _, tc := range []struct{ lockfile, want string }{
		{"pnpm-lock.yaml", "pnpm install"},
		{"package-lock.json", "npm ci"},
	} {
		cmd := installCommand(lockfileTree(t, tc.lockfile))
		if !strings.HasPrefix(cmd, tc.want) {
			t.Errorf("%s: got %q, want it to start with %q", tc.lockfile, cmd, tc.want)
		}
		if strings.Contains(cmd, "--mutex") {
			t.Errorf("%s: carries yarn's mutex flag: %q", tc.lockfile, cmd)
		}
	}
}
