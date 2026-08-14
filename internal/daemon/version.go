package daemon

import "sync/atomic"

// buildVersion is what the dashboard shows in its corner, set once at startup.
//
// Package-level rather than a field on Daemon, and set by main, because the stamp lives in
// package main where -ldflags puts it and the daemon cannot import its own command. It is a
// property of the process rather than of one Daemon, so threading it through construction would
// put the same string in every constructor for no gain.
//
// Atomic because `serve` sets it while nothing else runs and `/status` reads it from every
// request goroutine afterwards. A plain string would be a data race the race detector reports on
// the first status poll.
var buildVersion atomic.Value

// SetBuildVersion records the running binary's version for the dashboard to display.
//
// Called from main before the daemon starts serving. Unset, the dashboard shows nothing rather
// than guessing — a corner reading "dev" on a released binary is worse than an empty corner.
func SetBuildVersion(s string) { buildVersion.Store(s) }

// BuildVersion is the recorded version, or empty when nothing set one.
func BuildVersion() string {
	if s, ok := buildVersion.Load().(string); ok {
		return s
	}
	return ""
}
