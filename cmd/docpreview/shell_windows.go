//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func unsafeSizeof(v windows.ProcessEntry32) uintptr { return unsafe.Sizeof(v) }

// parentShell identifies the invoking shell by walking the process tree.
//
// Environment sniffing is not good enough on Windows. PowerShell exports
// PSModulePath, and any Git Bash launched from PowerShell inherits it — while
// also setting SHELL, which PowerShell launched from Git Bash then inherits in
// turn. Whichever variable you check first, there is a common arrangement that
// makes it lie, and the failure is silent: you emit `export FOO=...` into a
// PowerShell prompt and it reports that `export` is not a cmdlet.
//
// The process tree does not lie. Walk up from ourselves and return the first
// ancestor whose image name is a shell we recognize.
//
// A few levels, not one: `docpreview` may be invoked through a wrapper, and in
// a pipeline the immediate parent is sometimes the shell's helper rather than
// the shell. Four is enough for every arrangement seen in practice and bounds
// the walk against a corrupted or cyclic snapshot.
func parentShell() (shellKind, bool) {
	const maxDepth = 4

	pid := uint32(windows.Getpid())
	for range maxDepth {
		parent, name, ok := parentOf(pid)
		if !ok {
			return "", false
		}
		if kind, ok := shellFromImageName(name); ok {
			return kind, true
		}
		pid = parent
	}
	return "", false
}

// parentOf returns the parent PID of pid and the parent's image name.
func parentOf(pid uint32) (parentPID uint32, parentName string, ok bool) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, "", false
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafeSizeof(entry))

	if err := windows.Process32First(snapshot, &entry); err != nil {
		return 0, "", false
	}

	// One pass collects both halves of the answer: the target's parent PID, and
	// every PID's name. Snapshots are point-in-time, so taking a second one to
	// look up the parent risks the parent having exited in between.
	names := map[uint32]string{}
	found := false

	for {
		names[entry.ProcessID] = windows.UTF16ToString(entry.ExeFile[:])
		if entry.ProcessID == pid {
			parentPID = entry.ParentProcessID
			found = true
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}

	if !found {
		return 0, "", false
	}
	name, ok := names[parentPID]
	return parentPID, name, ok
}

// shellFromImageName maps an executable name to a shell.
func shellFromImageName(name string) (shellKind, bool) {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(name), ".exe"))
	switch base {
	case "pwsh", "powershell", "powershell_ise":
		return shellPowerShell, true
	case "bash", "sh", "zsh", "dash", "ksh":
		return shellPosix, true
	case "fish":
		return shellFish, true
	case "cmd":
		return shellCmd, true
	default:
		return "", false
	}
}
