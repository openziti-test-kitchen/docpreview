package pipeline

import (
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
)

// TestNoSecretReachesTheCommandLine.
//
// `docker create --env NAME=value` publishes every value in the process's command line,
// which any local account can read while the process runs — `ps` on Linux,
// `Get-CimInstance Win32_Process` on Windows. Redaction cannot help: it scrubs what this
// program writes, and this was the operating system handing the value out.
//
// So the environment goes in a 0600 file that exists only for the moment `docker create`
// reads it. This test is the reason that cannot quietly regress: an --env argument
// carrying a token is invisible in a screenshot, in a log, and in a passing build.
func TestNoSecretReachesTheCommandLine(t *testing.T) {
	const token = "ATCTT3xFfGN0-pretend-this-is-a-real-bitbucket-token"

	b := NewBuilder(config.BuildDefaults{Driver: "docker"}, slog.New(slog.DiscardHandler)).
		WithSecrets(map[string]string{"BB_REPO_TOKEN_ONPREM": token})

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
	}
	cfg := config.RepoConfig{}
	cfg.Build.Output = "build"
	cfg.Build.Command = "npm run build"

	path, extra, cleanup, err := b.writeEnvFile(pr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if path == "" {
		t.Fatal("no env file was written, so the values went to the command line")
	}
	if len(extra) != 0 {
		t.Errorf("%d values still go through --env: %v", len(extra)/2, extra)
	}

	args, err := b.createArgs(pr, cfg, "/mnt/d/ws", "/workspace", t.TempDir(),
		"node:24-bookworm-slim", path, extra)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range args {
		if strings.Contains(a, token) {
			t.Fatalf("the token is in the docker command line: %q", a)
		}
	}
	// And the file docker is pointed at does carry it, or the build would run without the
	// variable and this test would pass for the wrong reason.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "BB_REPO_TOKEN_ONPREM="+token) {
		t.Error("the env file does not carry the variable, so the build would not get it")
	}

	// 0600. A file that is briefly readable is a file that was readable. Unix only: on
	// Windows the mode bits are advisory and os.Stat reports 0666 regardless of the ACL,
	// which is the same carve-out the vault documents for its own permission check.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != fs.FileMode(0o600) {
			t.Errorf("env file mode = %v, want 0600", perm)
		}
	}
}

// TestTheEnvFileIsRemoved — it holds every injected credential in plaintext, and docker
// reads it once, at create. Left behind, one accumulates per build in a directory that is
// world-readable on most systems.
func TestTheEnvFileIsRemoved(t *testing.T) {
	b := NewBuilder(config.BuildDefaults{Driver: "docker"}, slog.New(slog.DiscardHandler)).
		WithSecrets(map[string]string{"TOKEN": "a-value-long-enough-to-redact"})

	pr := model.PullRequest{
		Repo: model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}, Number: 1,
	}
	path, _, cleanup, err := b.writeEnvFile(pr, config.RepoConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the env file was not created: %v", err)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the env file survived cleanup: %v", err)
	}
	// Twice, because cleanup runs from a defer on paths that may already have removed it.
	cleanup()
}

// TestAMultiLineValueIsReportedRatherThanMangled.
//
// docker's env-file format takes a value literally to the end of the line, so a PEM
// injected as a build secret cannot go in the file. It falls back to --env, where it *is*
// exposed in the process list — which is why the fallback logs the variable's name. Silent
// truncation to the first line would be worse: the build would run with a corrupt
// credential and fail somewhere unrelated.
func TestAMultiLineValueIsReportedRatherThanMangled(t *testing.T) {
	const pem = "-----BEGIN KEY-----\nabcdef\n-----END KEY-----"

	b := NewBuilder(config.BuildDefaults{Driver: "docker"}, slog.New(slog.DiscardHandler)).
		WithSecrets(map[string]string{
			"A_PEM":     pem,
			"ONE_LINER": "a-single-line-value-long-enough",
		})

	pr := model.PullRequest{
		Repo: model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"}, Number: 1,
	}
	path, extra, cleanup, err := b.writeEnvFile(pr, config.RepoConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if !strings.Contains(strings.Join(extra, " "), "A_PEM=") {
		t.Errorf("the multi-line value did not fall back to --env: %v", extra)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "A_PEM") {
		t.Error("a multi-line value was written into the env file, where it would be truncated")
	}
	if !strings.Contains(string(body), "ONE_LINER=") {
		t.Error("the single-line value did not go in the file")
	}
}
