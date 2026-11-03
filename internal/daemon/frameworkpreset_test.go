package daemon

import (
	"regexp"
	"testing"
)

// TestEveryPresetFieldSyncFrameworkReadsIsEmitted.
//
// The framework select carries each preset's answers as data- attributes, and syncFramework
// copies them into the placeholders of the fields they fill. The two halves live four thousand
// lines apart, and nothing connects them: a preset field that is read but never emitted reads
// back undefined, so the placeholder silently falls through to its hardcoded default.
//
// That is not a cosmetic failure. It happened to `dir`, and the form then advertised `www` under
// the Docusaurus preset while the build used `docusaurus` — the placeholder told the operator the
// opposite of what the server would do, which is worse than showing nothing at all.
//
// Parsed out of the real page rather than restated here. A list of expected attributes in this
// file would agree with the test and disagree with the page, which is the failure it is meant to
// catch.
func TestEveryPresetFieldSyncFrameworkReadsIsEmitted(t *testing.T) {
	sync := regexp.MustCompile(`(?s)function syncFramework\(\) \{.*?\n\}`).Find(dashboardHTML)
	if sync == nil {
		t.Fatal("syncFramework is not in the page; this test needs rewriting, not deleting")
	}

	opt := regexp.MustCompile(`(?s)<option value="\$\{esc\(f\.id\)\}.*?</option>`).Find(dashboardHTML)
	if opt == nil {
		t.Fatal("the framework <option> template is not in the page; this test needs rewriting")
	}

	// Two ways the function reaches a preset field: the put() helper, whose first argument
	// names the dataset key, and a direct read for the ones that are not placeholders.
	want := map[string]bool{}
	for _, m := range regexp.MustCompile(`put\("([a-z_]+)"`).FindAllSubmatch(sync, -1) {
		want[string(m[1])] = true
	}
	for _, m := range regexp.MustCompile(`opt\.dataset\.([a-z_]+)`).FindAllSubmatch(sync, -1) {
		want[string(m[1])] = true
	}
	if len(want) == 0 {
		t.Fatal("found no preset fields read by syncFramework; the regexes have gone stale")
	}

	for field := range want {
		if !regexp.MustCompile(`data-` + field + `=`).Match(opt) {
			t.Errorf("syncFramework reads the %q preset field but the <option> template "+
				"never emits data-%s, so its placeholder always falls back to the hardcoded "+
				"default", field, field)
		}
	}
}
