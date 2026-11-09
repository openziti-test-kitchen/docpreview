package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// shellKind names a shell whose syntax we can emit an assignment in.
type shellKind string

const (
	shellPowerShell shellKind = "powershell"
	shellPosix      shellKind = "sh"
	shellCmd        shellKind = "cmd"
	shellFish       shellKind = "fish"
)

// detectShell works out which shell invoked us.
//
// The primary source is the process tree, via parentShell, because environment
// sniffing is not good enough. PowerShell exports PSModulePath, and a Git Bash
// launched from PowerShell inherits it while also setting SHELL — which a
// PowerShell launched from Git Bash then inherits in turn. Whichever variable
// you check first, there is a common nesting that makes it lie, and the failure
// is silent and confusing: `export FOO=...` arrives at a PowerShell prompt,
// which reports that `export` is not a cmdlet.
//
// The environment is still consulted, but only as a fallback for when the
// process tree is unreadable.
func detectShell() shellKind {
	if kind, ok := parentShell(); ok {
		return kind
	}

	if sh := os.Getenv("SHELL"); sh != "" {
		switch {
		case strings.Contains(sh, "fish"):
			return shellFish
		case strings.Contains(sh, "pwsh"), strings.Contains(sh, "powershell"):
			return shellPowerShell
		default:
			return shellPosix
		}
	}

	// PowerShell always exports this; cmd.exe does not.
	if os.Getenv("PSModulePath") != "" {
		return shellPowerShell
	}

	if runtime.GOOS == "windows" {
		return shellCmd
	}
	return shellPosix
}

// normalizeShellFlag rewrites a bare -shell into -shell=auto.
//
// Go's flag package has no notion of an optional value: a string flag always
// consumes the next token, so "keygen -shell" fails with "flag needs an
// argument" and "keygen -shell -quiet" silently takes "-quiet" as the shell
// name. Both are the forms people will actually type, since the whole appeal of
// the feature is that it guesses.
//
// Rewriting up front is a smaller price than a bool flag plus a separate string
// flag, which is the usual workaround and reads badly at the command line.
func normalizeShellFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for i, arg := range args {
		if arg != "-shell" && arg != "--shell" {
			out = append(out, arg)
			continue
		}
		// A value follows only if the next token is not itself a flag.
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		if next == "" || strings.HasPrefix(next, "-") {
			out = append(out, "-shell=auto")
			continue
		}
		out = append(out, arg)
	}
	return out
}

// parseShell resolves a -shell flag value.
func parseShell(value string) (shellKind, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return detectShell(), nil
	case "powershell", "pwsh", "ps", "ps1":
		return shellPowerShell, nil
	case "sh", "bash", "zsh", "posix":
		return shellPosix, nil
	case "fish":
		return shellFish, nil
	case "cmd", "bat", "batch":
		return shellCmd, nil
	default:
		return "", fmt.Errorf("unknown shell %q (use auto, powershell, sh, fish, or cmd)", value)
	}
}

// exportStatement renders "set this variable to this value" in a shell's own
// syntax, ready to be evaluated by that shell.
//
// The value is single-quoted wherever the shell supports it, because an age
// key is base32 and a passphrase might be anything. In POSIX shells and
// PowerShell a single-quoted string interpolates nothing, so a value containing
// $, backtick, or a space cannot become part of the command. cmd.exe has no
// quoting that survives `set`, which is one of several reasons it is the last
// resort here.
func exportStatement(kind shellKind, name, value string) string {
	switch kind {
	case shellPowerShell:
		// PowerShell escapes a literal single quote by doubling it.
		return fmt.Sprintf("$env:%s = '%s'", name, strings.ReplaceAll(value, "'", "''"))
	case shellFish:
		return fmt.Sprintf("set -gx %s '%s'", name, strings.ReplaceAll(value, "'", `\'`))
	case shellCmd:
		return fmt.Sprintf("set %s=%s", name, value)
	default:
		// POSIX has no escape inside single quotes; close, emit an escaped
		// quote, reopen.
		return fmt.Sprintf("export %s='%s'", name, strings.ReplaceAll(value, "'", `'\''`))
	}
}

// evalHint tells the operator how to feed the emitted statement to their shell.
func evalHint(kind shellKind, command string) string {
	switch kind {
	case shellPowerShell:
		return command + " | Invoke-Expression"
	case shellFish:
		return command + " | source"
	case shellCmd:
		return `for /f "delims=" %i in ('` + command + `') do @%i`
	default:
		return `eval "$(` + command + `)"`
	}
}
