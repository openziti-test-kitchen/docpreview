package scm

import (
	"strings"
	"testing"
)

// TestHasMarkerMatchesEveryStyleEverWritten is the test that makes adding a second
// marker style safe, and it is written to fail loudly if a style is ever dropped.
//
// The comments are on somebody else's server. A daemon upgraded across a style change
// finds comments it wrote in the old style, and a matcher that knew only the new one
// would decide there was no comment and post a second — on every open pull request at
// once, which is the exact failure the marker exists to prevent. So the styles are
// enumerated here, and every one of them must be recognised forever.
func TestHasMarkerMatchesEveryStyleEverWritten(t *testing.T) {
	// Every style docpreview has ever written, oldest first. Append; never remove.
	styles := []MarkerStyle{MarkerHTMLComment, MarkerLinkRef}

	for _, s := range styles {
		body := MarkerFor("abc123", s) + "\n\n**Documentation preview**\n"
		if !HasMarker(body, "abc123") {
			t.Errorf("style %d is not recognised; every open pull request would get a "+
				"second comment on upgrade", s)
		}
		if HasMarker(body, "def456") {
			t.Errorf("style %d matched another preview's id, so two instances watching "+
				"one repository would fight over one comment", s)
		}
	}
}

// TestMarkerStyleRenderings pins the two syntaxes, because each is chosen for what a
// specific renderer does with it and a plausible-looking edit breaks that silently —
// the marker still matches, and only the rendered comment is wrong.
func TestMarkerStyleRenderings(t *testing.T) {
	if got, want := MarkerFor("abc123", MarkerHTMLComment), "<!-- docpreview:abc123 -->"; got != want {
		t.Errorf("MarkerHTMLComment = %q, want %q", got, want)
	}

	// A CommonMark link reference definition: `[label]: destination` at the start of
	// a line. Bitbucket escapes raw HTML, so this is the form that renders to nothing
	// there — observed on a live Vercel comment, see docs/design/15-bitbucket.md.
	got := MarkerFor("abc123", MarkerLinkRef)
	if want := "[docpreview]: #abc123"; got != want {
		t.Errorf("MarkerLinkRef = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "<>") {
		t.Errorf("MarkerLinkRef = %q contains angle brackets, which Bitbucket escapes "+
			"into visible text — the whole reason this style exists", got)
	}
}

// TestMarkerIsTheHTMLCommentStyle — Marker is what every comment-writing caller uses
// today and it must keep meaning the GitHub style. Changing what the no-argument form
// produces would change what is written to live pull requests as a side effect of a
// refactor.
func TestMarkerIsTheHTMLCommentStyle(t *testing.T) {
	if got, want := Marker("abc123"), MarkerFor("abc123", MarkerHTMLComment); got != want {
		t.Errorf("Marker = %q, want the HTML comment style %q", got, want)
	}
}
