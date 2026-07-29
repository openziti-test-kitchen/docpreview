package main

import (
	"flag"
	"strings"
)

// hoistFlags moves flag arguments ahead of positional ones.
//
// Go's flag package stops parsing at the first non-flag argument, so
//
//	docpreview sim init mydocs -config ./dev.yml
//
// silently ignores -config and writes to the default path instead. No error, no
// warning; the command succeeds and does the wrong thing. That is the worst
// possible handling of a typo people make constantly, because every other CLI
// they use accepts flags in any position.
//
// This rewrites the argument list so the standard parser sees what the operator
// meant. Value-taking flags bring their value along; boolean flags do not, and
// the FlagSet is consulted to tell them apart rather than guessing. A literal
// "--" stops the hoist, so a positional that begins with a dash can still be
// passed deliberately.
func hoistFlags(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}

		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)

		// -name=value carries its own value.
		if strings.Contains(arg, "=") {
			continue
		}

		name := strings.TrimLeft(arg, "-")
		if takesValue(fs, name) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	return append(flags, positional...)
}

// takesValue reports whether a flag consumes the following argument.
//
// Boolean flags do not, which is what makes `-build ./www` work: the path is a
// positional, not the value of -build. The flag package exposes this through an
// unexported interface that bool flags implement, so the check is on the value's
// behaviour rather than on a list of names we would have to keep in sync.
func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		// Unknown flag. Assume it takes no value and let the parser produce the
		// error, which will name the flag properly.
		return false
	}
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !bf.IsBoolFlag()
}
