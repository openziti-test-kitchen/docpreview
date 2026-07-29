package pipeline

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestDockerStdoutReachesTheLog is the assertion that keeps the docker driver
// useful rather than merely correct.
//
// A build whose output never arrives is worse than no docker driver at all: the
// dashboard tails an empty pane, the failure comment has nothing to point at, and
// the operator cannot tell a hung install from a finished one. The local driver
// gets this for free by running the command itself; the docker driver has to
// attach to a container, and whether that stream arrives depends on the endpoint —
// this daemon is reached over TCP rather than a local socket.
func TestDockerStdoutReachesTheLog(t *testing.T) {
	dockerAvailable(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	created, err := exec.CommandContext(ctx, "docker", "create", "--memory", "256m",
		"alpine:3", "sh", "-lc", "echo line-from-stdout && echo line-from-stderr 1>&2",
	).Output()
	if err != nil {
		t.Fatalf("docker create: %v", err)
	}
	container := strings.TrimSpace(string(created))
	defer exec.Command("docker", "rm", "-f", container).Run()

	var attached strings.Builder
	run := exec.CommandContext(ctx, "docker", "start", "-a", container)
	run.Stdout = &attached
	run.Stderr = &attached
	if err := run.Run(); err != nil {
		t.Fatalf("docker start -a: %v\n%s", err, attached.String())
	}
	t.Logf("start -a delivered %d bytes: %q", attached.Len(), attached.String())

	// docker logs, as the fallback. Whether attach works depends on the endpoint —
	// this daemon is reached over TCP — but the daemon retains the stream either
	// way, so asking for it afterwards is the portable answer.
	logged, err := exec.CommandContext(ctx, "docker", "logs", container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs: %v", err)
	}
	t.Logf("docker logs delivered %d bytes: %q", len(logged), string(logged))

	got := attached.String() + string(logged)
	for _, want := range []string{"line-from-stdout", "line-from-stderr"} {
		if !strings.Contains(got, want) {
			t.Errorf("the container's %s reached neither attach nor logs", want)
		}
	}
}
