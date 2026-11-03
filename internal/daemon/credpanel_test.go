package daemon

import (
	"regexp"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/scm"
)

// TestTheCredentialPanelRendersEveryOutcomeTheServerSends.
//
// The Test panel settles each row by looking the step's outcome up in two tables in
// credTestHTML — a mark and a word — and the outcomes themselves are constants in the scm
// package, a different language and four thousand lines away. Nothing connects the two: an
// outcome renamed on one side falls through to the page's fallback and a failure is drawn as
// "not attempted", which is the one way this panel could tell somebody a broken token is fine.
//
// Parsed out of the real page rather than restated here, for the reason the preset test gives.
func TestTheCredentialPanelRendersEveryOutcomeTheServerSends(t *testing.T) {
	fn := regexp.MustCompile(`(?s)function credTestHTML\(r\) \{.*?\n\}`).Find(dashboardHTML)
	if fn == nil {
		t.Fatal("credTestHTML is not in the page; this test needs rewriting, not deleting")
	}
	table := func(name string) string {
		m := regexp.MustCompile(`(?s)const ` + name + ` = \{(.*?)\};`).FindSubmatch(fn)
		if m == nil {
			t.Fatalf("no %s table in credTestHTML; the regex has gone stale", name)
		}
		return string(m[1])
	}
	marks, words := table("marks"), table("words")

	for _, outcome := range []string{scm.CheckPass, scm.CheckFail, scm.CheckNotAttempted} {
		key := regexp.MustCompile(`(^|[\s,{])` + regexp.QuoteMeta(outcome) + `:`)
		if !key.MatchString(marks) {
			t.Errorf("the server can send outcome %q but credTestHTML has no mark for it", outcome)
		}
		if !key.MatchString(words) {
			t.Errorf("the server can send outcome %q but credTestHTML has no word for it", outcome)
		}
	}
}

// TestTheCredentialPanelLivesInsideTheCredentialBox. The whole point of the change: the answer
// renders where the button is, and the handler finds its host by walking up to .credbox. A
// host moved outside the box would leave the handler falling back to a toast at the top of the
// window — the bug this replaced.
func TestTheCredentialPanelLivesInsideTheCredentialBox(t *testing.T) {
	page := string(dashboardHTML)
	if !strings.Contains(page, `b.closest(".credbox")?.querySelector("[data-ctest-host]")`) {
		t.Fatal("the Test handler no longer looks for its panel inside .credbox")
	}
	fields := regexp.MustCompile(`(?s)function scmCredentialFields\(s, p, id\) \{.*?\n\}`).
		FindString(page)
	if fields == "" {
		t.Fatal("scmCredentialFields is not in the page; this test needs rewriting")
	}
	box := strings.LastIndex(fields, `<div class="credbox">`)
	test := strings.Index(fields, `data-test-scm=`)
	host := strings.Index(fields, `data-ctest-host`)
	if box < 0 || test < box || host < test {
		t.Error("the panel host is not inside the credential box after the Test button")
	}
}
