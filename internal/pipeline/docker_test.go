package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The docker driver had no test, which is how it shipped unable to reach the
// workspace at all: it handed the daemon a Windows path, and the daemon is Linux
// and has no drive letters. See hostMountPath.
//
// These run docker for real, and they cannot be shell-driven: Git Bash rewrites
// container paths on the way through, which is a property of the shell rather than
// of the driver. Driving them through exec is the only way to see what the daemon
// sees.

func dockerAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("the docker daemon is not reachable")
	}
}

func TestHostMountPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		got, err := hostMountPath("/srv/docpreview/ws")
		if err != nil {
			t.Fatal(err)
		}
		if got != "/srv/docpreview/ws" {
			t.Errorf("hostMountPath = %q, want the path unchanged", got)
		}
		return
	}

	// The translation, and the two spellings that must not survive it: a backslash
	// path is what filepath hands out, and a colon is what --volume would split on.
	got, err := hostMountPath(`D:\worktrees\tangents\ws`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/mnt/d/worktrees/tangents/ws" {
		t.Errorf("hostMountPath = %q, want /mnt/d/worktrees/tangents/ws", got)
	}
	if strings.ContainsAny(got, `:\`) {
		t.Errorf("the translated path still has a colon or a backslash: %q", got)
	}

	// A UNC path has no drive letter to translate, and mounting the wrong thing is
	// worse than refusing: an empty mount fails later as a missing package.json.
	if _, err := hostMountPath(`\\fileserver\share\ws`); err == nil {
		t.Error("a UNC path was accepted; it cannot be mounted")
	}
}

// TestDockerMountRoundTrip is the driver's whole contract with docker: the source
// is visible inside the container, and what the container writes is on the host
// when it exits.
func TestDockerMountRoundTrip(t *testing.T) {
	dockerAvailable(t)

	ws := t.TempDir()
	site := filepath.Join(ws, "site")
	if err := os.MkdirAll(filepath.Join(site, "deep", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "input.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "deep", "deeper", "nested.txt"),
		[]byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}

	source, err := hostMountPath(ws)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--mount", "type=bind,source="+source+",target=/workspace",
		"--workdir", "/workspace/site", "--memory", "512m",
		"alpine:3", "sh", "-lc",
		"mkdir -p out/deep/deeper && cp input.txt out/copied.txt && "+
			"cp deep/deeper/nested.txt out/deep/deeper/nested.txt",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("the container failed: %v\n%s", err, out)
	}

	// Read from the host, not from inside the container: the mount is only useful
	// if the exposer can serve what the build wrote.
	body, err := os.ReadFile(filepath.Join(site, "out", "copied.txt"))
	if err != nil {
		t.Fatalf("the build output is not on the host: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("contents = %q, want %q", body, "hello")
	}
	nested, err := os.ReadFile(filepath.Join(site, "out", "deep", "deeper", "nested.txt"))
	if err != nil {
		t.Fatalf("the nested output is not on the host: %v", err)
	}
	if string(nested) != "nested" {
		t.Errorf("nested contents = %q, want %q", nested, "nested")
	}
}

// TestDockerMountSeesTheExecutableBit — a repository may ship an executable build
// script (ziti-doc runs gendoc.sh), and a mount that loses the bit turns it into
// "permission denied" inside the container.
//
// `test -x` and an exit code, not `ls -l` and a string: container stdout does not
// come back over a TCP endpoint, so a test that reads output cannot tell a lost
// bit from a lost stream.
func TestDockerMountSeesTheExecutableBit(t *testing.T) {
	dockerAvailable(t)

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "build.sh"), []byte("#!/bin/sh\necho ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := hostMountPath(ws)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--mount", "type=bind,source="+source+",target=/workspace",
		"--workdir", "/workspace", "--memory", "512m",
		"alpine:3", "test", "-x", "build.sh",
	).CombinedOutput()
	if err != nil {
		// Not fatal on Windows: NTFS carries no executable bit, so the mount has
		// nothing to preserve and a repo script must be invoked as `sh script.sh`.
		if runtime.GOOS == "windows" {
			t.Skipf("no executable bit on this host to preserve: %v %s", err, out)
		}
		t.Errorf("the mount lost the executable bit: %v\n%s", err, out)
	}
}
