// Package redact removes known secret values from text before it is shown to
// anyone.
//
// Build logs are the hazard. A documentation build may legitimately need a
// secret — a search-index write key, a private registry token — and build tools
// are careless with them: npm echoes its config, a failing HTTP call prints the
// request it made, a shell script runs with `set -x`. Any of those puts the
// value into output that docpreview then writes into a pull request comment,
// which is about the most public place it could land.
//
// The guarantee here is narrow and worth stating exactly. Every value the
// Redactor is told about is replaced with a fixed five asterisks wherever it
// appears in the text, including in several encodings a build tool might apply
// on the way. It is a last line of defence over output docpreview does not
// control, not a substitute for not logging secrets.
package redact

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

// Mask is what every secret becomes.
//
// Fixed width, always five, regardless of the secret's actual length. A mask
// that mirrored the length would leak it, and the length of a credential is a
// real clue: it distinguishes a 40-character GitHub token from a 4-character
// PIN, and it tells anyone brute-forcing exactly how much work they face.
const Mask = "*****"

// minLength is the shortest value worth redacting.
//
// A secret of one or two characters would match constantly — every "a" in the
// log becomes an asterisk run and the output is destroyed while telling an
// attacker the secret is one character. Values this short are rejected at
// registration and reported, so the operator finds out rather than quietly
// getting no protection.
const minLength = 4

// Redactor replaces known secret values with Mask.
//
// Safe for concurrent use once built; it is never mutated after construction.
type Redactor struct {
	// replacer does the work in one pass. strings.Replacer builds a trie, so
	// scrubbing is linear in the input regardless of how many secrets are
	// registered — which matters when it runs over every line of a build log.
	replacer *strings.Replacer

	// count is how many distinct strings are being matched, including the
	// encoded variants. Exposed for logging so an operator can see that
	// redaction is actually armed.
	count int
}

// New builds a Redactor for the given secret values.
//
// Values shorter than minLength are skipped and returned in `tooShort`, because
// silently declining to protect something is worse than saying so.
func New(values []string) (r *Redactor, tooShort []string) {
	seen := map[string]bool{}
	var pairs []string

	// Longest first. Otherwise a secret that is a prefix of another would be
	// replaced inside it, leaving the tail of the longer one exposed:
	// registering both "abcd" and "abcd1234" must not turn the latter into
	// "*****1234".
	var kept []string
	for _, v := range values {
		if len(v) < minLength {
			if v != "" {
				tooShort = append(tooShort, v)
			}
			continue
		}
		kept = append(kept, v)
	}
	sort.Slice(kept, func(i, j int) bool { return len(kept[i]) > len(kept[j]) })

	for _, v := range kept {
		for _, variant := range variants(v) {
			if len(variant) < minLength || seen[variant] {
				continue
			}
			seen[variant] = true
			pairs = append(pairs, variant, Mask)
		}
	}

	if len(pairs) == 0 {
		return &Redactor{}, tooShort
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...), count: len(pairs) / 2}, tooShort
}

// variants returns the forms a secret might appear in.
//
// A build tool rarely prints a credential exactly as given. It may percent-encode
// it into a URL, escape it into a JSON body it logs, or base64 it into an
// Authorization header that an error message then echoes. Each of those is a
// different byte sequence carrying the same secret, so each is matched.
//
// This is not exhaustive and cannot be: a tool that hashes or encrypts the value
// before logging defeats it, as does one that prints it in fragments. The set
// covers the transformations that actually show up in build output.
func variants(v string) []string {
	out := []string{v}

	add := func(s string) {
		if s != "" && s != v {
			out = append(out, s)
		}
	}

	add(url.QueryEscape(v))
	add(url.PathEscape(v))

	// JSON escaping, minus the surrounding quotes. Catches a secret logged
	// inside a request body, where a quote or backslash in it would be escaped.
	if b, err := json.Marshal(v); err == nil && len(b) >= 2 {
		add(string(b[1 : len(b)-1]))
	}

	// Base64, as it would appear in an Authorization header.
	add(base64.StdEncoding.EncodeToString([]byte(v)))
	add(base64.RawStdEncoding.EncodeToString([]byte(v)))
	add(base64.URLEncoding.EncodeToString([]byte(v)))

	return out
}

// Scrub replaces every known secret in s.
func (r *Redactor) Scrub(s string) string {
	if r == nil || r.replacer == nil || s == "" {
		return s
	}
	return r.replacer.Replace(s)
}

// ScrubBytes replaces every known secret in b, returning a new slice.
func (r *Redactor) ScrubBytes(b []byte) []byte {
	if r == nil || r.replacer == nil || len(b) == 0 {
		return b
	}
	return []byte(r.replacer.Replace(string(b)))
}

// ScrubError wraps an error so its message is scrubbed.
//
// Errors are the sharpest edge here. A failing command's message routinely
// contains the command line, and the command line contains the environment it
// was given; that error then travels up through several %w wraps and into a
// pull request comment. Scrubbing at the point the error leaves the build is
// the only place with both the secret list and the message.
func (r *Redactor) ScrubError(err error) error {
	if err == nil || r == nil || r.replacer == nil {
		return err
	}
	return &scrubbedError{msg: r.Scrub(err.Error()), cause: err}
}

// Count reports how many strings are being matched, counting encoded variants.
func (r *Redactor) Count() int {
	if r == nil {
		return 0
	}
	return r.count
}

// Active reports whether anything will be redacted.
func (r *Redactor) Active() bool { return r != nil && r.replacer != nil }

// scrubbedError presents a scrubbed message while keeping the original chain
// reachable by errors.Is and errors.As.
//
// Unwrap deliberately returns the unscrubbed cause: comparisons like
// errors.Is(err, context.DeadlineExceeded) must keep working, and a sentinel
// error's identity is not a secret. Only the rendered message is scrubbed,
// because the message is the only part that gets shown to anyone.
type scrubbedError struct {
	msg   string
	cause error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.cause }
