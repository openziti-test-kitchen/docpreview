package pipeline

import (
	"strings"
	"testing"
)

// TestADetectScriptCannotWriteThePullRequestComment: the script is the branch author's, and its
// output becomes the reason posted in a comment under the App's name. What comes out has to be
// one inert line: no mention, no markdown that renders, no way out of the code span, and bounded.
func TestADetectScriptCannotWriteThePullRequestComment(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
	}{
		{"a mention", "cc @everyone @org/team"},
		{"markdown", "# Approved\n\n![x](https://evil.example/pixel.png) [click](https://evil.example)"},
		{"breaking out of the code span", "ok` <img src=x onerror=alert(1)> `"},
		{"a fence", "```\n</details>\n```\n@admin"},
		{"many lines", strings.Repeat("line\n", 5000)},
		{"one long line", strings.Repeat("x", 100000)},
		{"text reordering", "safe ‮gnp.exe"},
		{"control characters", "a\x00b\x1b[31mc\r\nd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scriptSaid(tc.out)

			const lead = "Detect script said: `"
			if !strings.HasPrefix(got, lead) || !strings.HasSuffix(got, "`") {
				t.Fatalf("not quoted as a code span: %q", got)
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(got, lead), "`")
			if strings.ContainsAny(inner, "`@\n\r\t\x00\x1b‮") {
				t.Errorf("the quoted text can leave its span or mention somebody: %q", inner)
			}
			if n := len([]rune(inner)); n > detectReasonLimit+1 {
				t.Errorf("%d characters, want at most %d", n, detectReasonLimit+1)
			}
		})
	}

	if got := scriptSaid("  \n\t "); got != "" {
		t.Errorf("blank output became a reason: %q", got)
	}
	if got := scriptSaid("@`"); got != "" {
		t.Errorf("output with nothing left after stripping became a reason: %q", got)
	}
	if got, want := scriptSaid("only docs/api changed\n"), "Detect script said: `only docs/api changed`"; got != want {
		t.Errorf("scriptSaid = %q, want %q", got, want)
	}
}
