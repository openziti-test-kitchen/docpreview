package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// prompter reads answers from a terminal.
//
// Every prompt shows its default in brackets and accepts an empty line to take
// it, so the whole questionnaire can be completed by holding Enter and the
// result is a working configuration. That property is worth protecting: a setup
// wizard that cannot be skipped is a setup wizard people avoid.
//
// Questions go to stderr, not stdout. It costs nothing and it means
// `docpreview init -config -` could stream a config to a pipe later without the
// prose landing in it.
type prompter struct {
	in  *bufio.Scanner
	out io.Writer
}

func newPrompter() *prompter {
	s := bufio.NewScanner(os.Stdin)
	// Config values are short, but a Frontdoor API base plus a long comment is
	// not impossible; the default 64 KiB token limit is plenty and explicit.
	s.Buffer(make([]byte, 0, 4096), 64*1024)
	return &prompter{in: s, out: os.Stderr}
}

// section prints a heading.
func (p *prompter) section(title string) {
	fmt.Fprintf(p.out, "\n%s\n%s\n", title, strings.Repeat("-", len(title)))
}

// note prints explanatory text under a heading.
func (p *prompter) note(format string, args ...any) {
	fmt.Fprintf(p.out, format+"\n", args...)
}

// read returns the next line, or "" at EOF.
//
// EOF is treated as "take every remaining default" rather than as an error, so
// piping /dev/null at init produces the default configuration instead of a
// half-written file.
func (p *prompter) read() string {
	if !p.in.Scan() {
		return ""
	}
	return strings.TrimSpace(p.in.Text())
}

// text asks for a free-form string.
func (p *prompter) text(question, def string) string {
	return p.validated(question, def, nil)
}

// validated asks for a string and re-asks until check accepts it.
//
// Validating at the prompt rather than at write time is the difference between
// "that is not a host:port, try again" next to the question, and a wall of
// answers followed by one error about a field you filled in nine questions ago.
// The default is never validated: it is ours, and re-asking forever because a
// default is empty would trap the caller.
func (p *prompter) validated(question, def string, check func(string) error) string {
	for {
		if def == "" {
			fmt.Fprintf(p.out, "%s []: ", question)
		} else {
			fmt.Fprintf(p.out, "%s [%s]: ", question, def)
		}

		answer := p.read()
		if answer == "" {
			return def
		}
		if check == nil {
			return answer
		}
		if err := check(answer); err != nil {
			fmt.Fprintf(p.out, "  %v\n", err)
			continue
		}
		return answer
	}
}

// choice asks for one of a fixed set of options.
func (p *prompter) choice(question string, options []string, def string) string {
	for {
		fmt.Fprintf(p.out, "%s (%s) [%s]: ", question, strings.Join(options, "/"), def)
		answer := p.read()
		if answer == "" {
			return def
		}
		for _, opt := range options {
			if strings.EqualFold(answer, opt) {
				return opt
			}
		}
		fmt.Fprintf(p.out, "  %q is not one of %s\n", answer, strings.Join(options, ", "))
	}
}

// yesNo asks a yes/no question.
func (p *prompter) yesNo(question string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		fmt.Fprintf(p.out, "%s [%s]: ", question, hint)
		switch strings.ToLower(p.read()) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		default:
			fmt.Fprintln(p.out, "  answer y or n")
		}
	}
}

// number asks for an integer. An empty answer takes the default; anything
// unparseable is re-asked rather than silently becoming zero, because a zero
// app_id is a valid "fill this in later" and must not be produced by a typo.
func (p *prompter) number(question string, def int64) int64 {
	for {
		fmt.Fprintf(p.out, "%s [%d]: ", question, def)
		answer := p.read()
		if answer == "" {
			return def
		}
		n, err := strconv.ParseInt(answer, 10, 64)
		if err != nil {
			fmt.Fprintf(p.out, "  %q is not a number\n", answer)
			continue
		}
		return n
	}
}

// duration asks for a Go duration such as 72h or 15m.
func (p *prompter) duration(question string, def time.Duration) time.Duration {
	for {
		fmt.Fprintf(p.out, "%s [%s]: ", question, def)
		answer := p.read()
		if answer == "" {
			return def
		}
		d, err := time.ParseDuration(answer)
		if err != nil {
			fmt.Fprintf(p.out, "  %q is not a duration (try 72h, 30m, 15m)\n", answer)
			continue
		}
		if d <= 0 {
			fmt.Fprintln(p.out, "  must be positive")
			continue
		}
		return d
	}
}
