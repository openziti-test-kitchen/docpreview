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

// wslPath rewrites D:/x as /mnt/d/x, which is where a dockerd running inside a
// WSL distribution sees Windows drives.
func wslPath(slashed string) string {
	if len(slashed) > 2 && slashed[1] == ':' {
		return "/mnt/" + strings.ToLower(slashed[:1]) + slashed[2:]
	}
	return slashed
}

// desktopPath rewrites D:/x as /run/desktop/mnt/host/d/x, which is where Docker
// Desktop's own engine mounts the host's drives.
func desktopPath(slashed string) string {
	if len(slashed) > 2 && slashed[1] == ':' {
		return "/run/desktop/mnt/host/" + strings.ToLower(slashed[:1]) + slashed[2:]
	}
	return slashed
}

// TestWhichMountFormDockerAccepts is a diagnostic, not an assertion.
//
// Which spellings of a host path a Windows docker CLI accepts is not documented
// anywhere matching observed behaviour, and the answer moves with the daemon: a
// dockerd inside WSL2 wants /mnt/<letter>, Docker Desktop's own engine wants
// /run/desktop/mnt/host/<letter>, and a remote daemon accepts none of them.
// hostMountPath encodes one answer; this asks the daemon in front of it.
//
// Success is judged by exit status, never by output: container stdout does not come back
// over a TCP endpoint, so a probe that looked for the canary's contents in the command's
// output would report every form as broken, including the ones that work.
//
// It logs and never fails, so it is safe to leave in the tree.
func TestWhichMountFormDockerAccepts(t *testing.T) {
	// Off by default: it starts six containers to answer a question nobody is
	// asking on an ordinary test run. Set DOCPREVIEW_DOCKER_DIAG=1 when a new
	// Docker version makes the answer worth rechecking.
	if os.Getenv("DOCPREVIEW_DOCKER_DIAG") == "" {
		t.Skip("set DOCPREVIEW_DOCKER_DIAG=1 to probe which host-path spellings docker accepts")
	}
	dockerAvailable(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "canary.txt"), []byte("mounted"), 0o600); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	slashed := filepath.ToSlash(abs)
	// The MSYS/Docker Toolbox spelling: /d/path, no colon at all.
	msys := slashed
	if len(msys) > 2 && msys[1] == ':' {
		msys = "/" + strings.ToLower(msys[:1]) + msys[2:]
	}

	forms := []struct {
		name, arg string
		flag      string
	}{
		{"volume backslash", abs + ":/workspace", "--volume"},
		{"volume forward", slashed + ":/workspace", "--volume"},
		{"volume msys", msys + ":/workspace", "--volume"},
		{"mount backslash", "type=bind,source=" + abs + ",target=/workspace", "--mount"},
		{"mount forward", "type=bind,source=" + slashed + ",target=/workspace", "--mount"},
		{"mount msys", "type=bind,source=" + msys + ",target=/workspace", "--mount"},
		// WSL2 spellings. A dockerd inside a WSL distro sees Windows drives at
		// /mnt/<letter>; Docker Desktop's own engine exposes them at
		// /run/desktop/mnt/host/<letter>.
		{"volume wsl", wslPath(slashed) + ":/workspace", "--volume"},
		{"volume desktop", desktopPath(slashed) + ":/workspace", "--volume"},
		{"mount wsl", "type=bind,source=" + wslPath(slashed) + ",target=/workspace", "--mount"},
		{"mount desktop", "type=bind,source=" + desktopPath(slashed) + ",target=/workspace", "--mount"},
	}

	for _, f := range forms {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		// test -f, so the exit code is the whole answer and no output is needed.
		out, err := exec.CommandContext(ctx, "docker", "run", "--rm",
			f.flag, f.arg, "--workdir", "/workspace", "alpine:3", "test", "-f", "canary.txt",
		).CombinedOutput()
		cancel()

		if err == nil {
			t.Logf("WORKS  %-18s %s %s", f.name, f.flag, f.arg)
			continue
		}
		got := strings.TrimSpace(string(out))
		if len(got) > 160 {
			got = got[:160]
		}
		t.Logf("fails  %-18s %v — %s", f.name, err, got)
	}
}
