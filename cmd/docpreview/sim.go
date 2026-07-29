package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/netfoundry/docpreview/internal/config"
)

// cmdSim manages the local source-control stand-in.
//
// The point is to be able to feel the whole flow — change, commit, push, build,
// comment, push again, watch the same comment update — without a GitHub App, an
// account, or an internet connection. Everything downstream of the webhook is
// the real thing: the real queue, the real clone, the real build, the real
// exposer, the real comment renderer.
func cmdSim(args []string) error {
	if len(args) == 0 {
		simUsage()
		return fmt.Errorf("sim: no subcommand")
	}

	switch args[0] {
	case "init":
		return cmdSimInit(args[1:])
	case "list":
		return cmdSimList(args[1:])
	default:
		simUsage()
		return fmt.Errorf("unknown sim subcommand %q", args[0])
	}
}

func simUsage() {
	fmt.Fprint(os.Stderr, `docpreview sim — the local source-control stand-in

  docpreview sim init <name>   Create a bare repo that triggers builds on push
  docpreview sim list          List the repos docpreview will accept pushes for

Then, from a working copy:

  git remote add preview <the path init printed>
  git push preview my-branch

That push runs a build and writes a comment, exactly as a pull request would.
Watch it at http://<listen>/pr
`)
}

func cmdSimInit(args []string) error {
	fs := flag.NewFlagSet("sim init", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	base := fs.String("base", "", "the base branch (default: local.default_base)")
	seed := fs.String("seed", "", "seed the repo by pushing this working copy's current branch")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}

	name := fs.Arg(0)
	if name == "" {
		return fmt.Errorf("usage: docpreview sim init <name>")
	}
	if strings.ContainsAny(name, `/\:`) {
		return fmt.Errorf("repository name %q must not contain a path separator", name)
	}

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		return err
	}
	if *base == "" {
		*base = cfg.Local.DefaultBase
	}

	if err := os.MkdirAll(cfg.Local.ReposDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", cfg.Local.ReposDir, err)
	}

	repoPath := filepath.Join(cfg.Local.ReposDir, name+".git")
	if _, err := os.Stat(repoPath); err == nil {
		return fmt.Errorf("%s already exists", repoPath)
	}

	if err := git("", "init", "--bare", "--initial-branch="+*base, repoPath); err != nil {
		return err
	}

	// Bare repositories reject a push to the branch they have checked out; they
	// have none, but denyCurrentBranch still applies to the symbolic HEAD.
	if err := git(repoPath, "config", "receive.denyCurrentBranch", "ignore"); err != nil {
		return err
	}

	// The hook is a curl to the ingress, so the ingress needs an address on
	// this machine. An overlay-only configuration has none, and a hook written
	// against an empty address would fail on every push with a curl error
	// rather than anything that names the cause.
	if cfg.FirstTCPAddr() == "" {
		return errors.New("the ingress has no TCP listener, so a git hook has nowhere to " +
			"post; add `- tcp: \"127.0.0.1:8471\"` to listeners")
	}

	hookPath := filepath.Join(repoPath, "hooks", "post-receive")
	if err := os.WriteFile(hookPath, []byte(postReceiveHook(cfg, *base)), 0o755); err != nil {
		return fmt.Errorf("writing the post-receive hook: %w", err)
	}

	fmt.Printf("Created %s\n\n", repoPath)
	fmt.Printf("Add it as a remote and push:\n\n")
	fmt.Printf("  git remote add preview %q\n", filepath.ToSlash(repoPath))
	fmt.Printf("  git push preview <branch>\n\n")
	fmt.Printf("Every push triggers a build and updates one comment.\n")
	fmt.Printf("Watch it at http://%s/pr\n", cfg.FirstTCPAddr())

	if *seed != "" {
		fmt.Printf("\nSeeding from %s\n", *seed)
		if err := git(*seed, "push", repoPath, "HEAD:"+*base); err != nil {
			return fmt.Errorf("seeding: %w", err)
		}
		fmt.Printf("Pushed the current branch as %q.\n", *base)
	}

	return nil
}

func cmdSimList(args []string) error {
	fs := flag.NewFlagSet("sim list", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to the server config file")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return err
	}

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(cfg.Local.ReposDir)
	if os.IsNotExist(err) {
		fmt.Printf("No repositories yet. Create one with: docpreview sim init <name>\n")
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", cfg.Local.ReposDir, err)
	}

	found := false
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".git") {
			found = true
			fmt.Printf("%-24s %s\n", strings.TrimSuffix(e.Name(), ".git"),
				filepath.Join(cfg.Local.ReposDir, e.Name()))
		}
	}
	if !found {
		fmt.Printf("No repositories in %s\n", cfg.Local.ReposDir)
	}
	return nil
}

// postReceiveHook is the script that turns `git push` into a webhook delivery.
//
// This is the piece that makes the whole thing feel real rather than simulated:
// you push, and a build starts, because a hook fired — the same causal chain a
// hosted platform gives you, with the hook running locally instead of in
// somebody's datacenter.
//
// The pull request number is derived from the branch name so that pushing the
// same branch twice updates one comment rather than opening a second one, which
// is the behaviour being demonstrated. A real platform assigns the number; here
// the branch is the only stable identifier available.
func postReceiveHook(cfg config.Server, base string) string {
	return fmt.Sprintf(`#!/bin/sh
# Generated by 'docpreview sim init'. Turns a push into a webhook delivery.
set -e

WEBHOOK="http://%s/webhook/local"
REPO="$(basename "$(pwd)" .git)"
BASE="%s"

while read -r _old new ref; do
  branch="${ref#refs/heads/}"

  # Pushing the base branch is not a pull request.
  [ "$branch" = "$BASE" ] && continue

  # A deleted branch closes its "pull request".
  if [ "$new" = "0000000000000000000000000000000000000000" ]; then
    action=closed
    new=""
  else
    action=synchronize
  fi

  # A stable number per branch, so repeated pushes update one comment instead
  # of opening a new one each time. cksum is in every POSIX environment;
  # collisions across branches are possible and harmless here.
  number="$(printf '%%s' "$branch" | cksum | cut -d' ' -f1)"
  number=$((number %% 9000 + 1000))

  printf 'docpreview: %%s %%s -> %%s\n' "$action" "$branch" "$WEBHOOK" >&2

  curl -sS -X POST "$WEBHOOK" \
    -H 'Content-Type: application/json' \
    -d "{\"action\":\"$action\",\"repo\":\"$REPO\",\"number\":$number,\"branch\":\"$branch\",\"sha\":\"$new\",\"base\":\"$BASE\"}" \
    >&2 || printf 'docpreview: webhook unreachable — is the daemon running?\n' >&2
  printf '\n' >&2
done
`, cfg.FirstTCPAddr(), base)
}

// git runs a git command, optionally in a directory.
func git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
