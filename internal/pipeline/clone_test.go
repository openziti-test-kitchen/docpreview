package pipeline

import (
	"testing"
	"time"
)

func TestScrubRemovesCloneCredentials(t *testing.T) {
	// Git echoes the remote URL in its errors, and that URL carries a live
	// installation token. Build output is attached to the pull request comment,
	// so a leak here publishes a GitHub credential to the pull request.
	const token = "ghs_supersecrettokenvalue"

	tests := []struct {
		name string
		in   string
	}{
		{
			"fatal clone error",
			"fatal: could not read from 'https://x-access-token:" + token + "@github.com/acme/docs.git'",
		},
		{
			"remote line",
			"remote: repository not found\nfatal: https://x-access-token:" + token + "@github.com/a/b.git not found",
		},
		{
			"enterprise host",
			"error: https://x-access-token:" + token + "@ghe.internal:8443/a/b.git",
		},
		{
			// The one that leaked. A Bitbucket credential is an email address, so
			// the userinfo contains an unescaped "@" and the first one is inside
			// the username. Splitting on the first "@" redacted the username and
			// published the token.
			"username containing an at sign",
			"fatal: https://someone@example.com:" + token + "@bitbucket.org/ws/docs.git not found",
		},
		{
			// Two of them on one line, the second with an "@" in the username.
			// Scrubbing has to survive the first replacement and keep going.
			"two urls, second has an at sign in userinfo",
			"tried https://x-access-token:" + token + "@github.com/a/b.git then " +
				"https://someone@example.com:" + token + "@bitbucket.org/c/d.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scrub(tt.in)
			if contains(got, token) {
				t.Fatalf("scrub leaked the token: %q", got)
			}
			if !contains(got, "REDACTED") {
				t.Fatalf("scrub did not mark the redaction: %q", got)
			}
		})
	}
}

func TestScrubLeavesOrdinaryOutputAlone(t *testing.T) {
	for _, in := range []string{
		"Cloning into 'docs'...",
		"warning: redirecting to https://github.com/acme/docs.git/",
		"see https://docs.example.com/help@v2 for details",
		"",
	} {
		if got := scrub(in); got != trimSpace(in) {
			t.Errorf("scrub(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestScrubTerminates(t *testing.T) {
	// The scrubber walks a line replacing userinfo sections. An earlier version
	// re-scanned from the start after each replacement and spun forever on a
	// line it had already redacted.
	done := make(chan string, 1)
	go func() {
		done <- scrub("https://a:b@x/ https://c:d@y/ https://***REDACTED***@z/")
	}()

	select {
	case got := <-done:
		if contains(got, "a:b") || contains(got, "c:d") {
			t.Fatalf("scrub missed a credential: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scrub did not terminate")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
