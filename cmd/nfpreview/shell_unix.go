//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// parentShell identifies the invoking shell.
//
// On Unix, $SHELL is set by the login shell and is not routinely inherited
// across shell-in-shell nesting the way the Windows variables are, so the
// ambiguity that forces process-tree walking on Windows does not arise here.
// /proc is consulted first where it exists, because it answers the question
// exactly rather than by convention.
func parentShell() (shellKind, bool) {
	if kind, ok := shellFromProc(); ok {
		return kind, true
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return shellFromImageName(sh)
	}
	return "", false
}

// shellFromProc reads the parent's executable name from /proc, where available.
func shellFromProc() (shellKind, bool) {
	comm, err := os.ReadFile("/proc/" + itoa(os.Getppid()) + "/comm")
	if err != nil {
		return "", false
	}
	return shellFromImageName(strings.TrimSpace(string(comm)))
}

// shellFromImageName maps an executable name or path to a shell.
func shellFromImageName(name string) (shellKind, bool) {
	// Login shells appear as "-bash"; strip the leading hyphen.
	base := strings.TrimPrefix(filepath.Base(strings.TrimSpace(name)), "-")
	switch strings.ToLower(base) {
	case "pwsh", "powershell":
		return shellPowerShell, true
	case "bash", "sh", "zsh", "dash", "ksh":
		return shellPosix, true
	case "fish":
		return shellFish, true
	default:
		return "", false
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
