package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildAsUser runs the script the driver runs, as the driver builds it, in image, with ws mounted
// at /workspace. It returns the container's combined output and its exit code.
func buildAsUser(t *testing.T, ctx context.Context, image, ws, cmd string) (string, int) {
	t.Helper()
	source, err := hostMountPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	script := buildScript(asUser(cmd, "/workspace", uid, gid), reownCommand("/workspace", uid, gid))
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--mount", "type=bind,source="+source+",target=/workspace",
		"--workdir", "/workspace", "--memory", "512m",
		image, "sh", "-lc", script,
	).CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	return string(out), 0
}

// dockerAsUser skips a test run as root, where there is no other user to build as. The file is
// Linux-only: Docker Desktop maps ownership through a mount, so elsewhere there is nothing to see.
func dockerAsUser(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("run as root, there is no other user to build as")
	}
	dockerAvailable(t)
}

func ownerOf(t *testing.T, path string) int {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(fi.Sys().(*syscall.Stat_t).Uid)
}

// A build's files are the daemon's, the build can get root with sudo, and the build's own exit
// status comes back. Runs on Debian, which has a setuid su and no sudo, and on Alpine, which has
// neither a usable su nor sudo until the script installs it.
func TestDockerBuildRunsAsTheDaemonsUser(t *testing.T) {
	dockerAsUser(t)

	for _, image := range []string{"node:24-bookworm", "node:24-alpine"} {
		t.Run(image, func(t *testing.T) {
			ws := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			out, code := buildAsUser(t, ctx, image, ws,
				`mkdir -p out && echo hi > out/index.html && `+
					`test -w "$HOME" && `+
					`[ "$(id -u)" = "`+strconv.Itoa(os.Getuid())+`" ] && `+
					`[ "$(sudo id -u)" = 0 ] && `+
					`sudo sh -c 'echo root > out/by-sudo.txt' && `+
					`sudo printf '%s|' "a b" "it's" > out/args.txt`)
			if code != 0 {
				t.Fatalf("the build failed (exit %d):\n%s", code, out)
			}
			if !strings.Contains(out, "$ building as") {
				t.Errorf("the log does not say which user built:\n%s", out)
			}
			for _, f := range []string{"out", "out/index.html", "out/by-sudo.txt"} {
				if got := ownerOf(t, filepath.Join(ws, f)); got != os.Getuid() {
					t.Errorf("%s is owned by uid %d, want %d", f, got, os.Getuid())
				}
			}
			args, err := os.ReadFile(filepath.Join(ws, "out", "args.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(args) != "a b|it's|" {
				t.Errorf("sudo did not pass its arguments through intact: %q", args)
			}
		})
	}
}

// A failed build still fails. The drop to the uid wraps the build in an if, and the status of an
// if is easy to lose.
func TestDockerBuildAsUserKeepsTheExitStatus(t *testing.T) {
	dockerAsUser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out, code := buildAsUser(t, ctx, "node:24-bookworm", t.TempDir(), "echo building; exit 7")
	if code != 7 {
		t.Errorf("exit = %d, want the build's 7:\n%s", code, out)
	}
}

// A build killed part way through never reaches the chown at its end, so what it wrote must
// already be removable by the daemon. If it is not, the next build of that commit fails with
// "clearing workspace: permission denied".
func TestDockerKilledBuildLeavesNothingTheDaemonCannotRemove(t *testing.T) {
	dockerAsUser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ws := t.TempDir()
	source, err := hostMountPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	script := buildScript(
		asUser("mkdir -p node_out/deep && touch node_out/deep/f && touch started && sleep 600",
			"/workspace", uid, gid),
		reownCommand("/workspace", uid, gid))

	name := "nfpreview-asuser-kill-" + strconv.Itoa(os.Getpid())
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name,
		"--mount", "type=bind,source="+source+",target=/workspace",
		"--workdir", "/workspace", "node:24-bookworm", "sh", "-lc", script)
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	defer exec.Command("docker", "rm", "-f", name).Run()

	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
		if _, err := os.Stat(filepath.Join(ws, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the build never started")
		}
	}
	// What a timeout does: the container is removed with the build still running.
	if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
		t.Fatalf("docker rm: %v\n%s", err, out)
	}
	_ = run.Wait()

	if err := os.RemoveAll(filepath.Join(ws, "node_out")); err != nil {
		t.Errorf("the daemon cannot remove what a killed build left: %v", err)
	}
}
