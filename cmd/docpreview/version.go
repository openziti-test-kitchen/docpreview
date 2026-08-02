package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// The build stamp, set with -ldflags by release.sh.
//
// Left empty in an ordinary `go build`, which is the common case during development and must not
// print something that looks like a release. `versionLine` fills the gap from the module's own
// build info instead, so a binary built from a checkout still says which commit it came from.
var (
	version = ""
	commit  = ""
	date    = ""
)

// versionLine is what `docpreview version` prints, and what a bug report should carry.
//
// The commit matters more than the tag. A tag says which release somebody meant to install; the
// commit says what they are actually running, which is the question when a fix is reported as not
// working. Both are here, with the platform, because "it fails on the VM and not on my laptop" is
// usually those two lines differing.
func versionLine() string {
	v, c, d := version, commit, date

	// From the binary itself when it was not stamped. Go records the VCS revision and whether the
	// tree was dirty for any build from a repository — so a locally built binary is still
	// identifiable, which is the one this question gets asked about most.
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			case "vcs.modified":
				if s.Value == "true" {
					// Said out loud. A dirty build is not the commit it claims, and a bug
					// report that omits this sends somebody reading the wrong source.
					c += "+dirty"
				}
			}
		}
	}

	if v == "" {
		v = "dev"
	}
	if len(c) > 12 {
		c = c[:12]
	}

	parts := []string{"docpreview " + v}
	if c != "" {
		parts = append(parts, c)
	}
	if d != "" {
		parts = append(parts, d)
	}
	parts = append(parts, fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH), runtime.Version())
	return strings.Join(parts, "  ")
}
