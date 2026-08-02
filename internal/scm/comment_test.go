package scm

import (
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
)

func testReport(state State) Report {
	return Report{
		PR: model.PullRequest{
			Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
			Number: 42,
		},
		PreviewID: "abc123",
		Commit:    "d141f6efa1b1f686117b7d8b141199d025f0061a",
		UpdatedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		State:     state,
	}
}

// A pull request comment is public on any public repository, and two of the
// fields on a Report were written for an operator's terminal rather than for
// publication. These tests keep them out of it.

func TestFailureCommentQuotesNeitherTheErrorNorTheLog(t *testing.T) {
	r := testReport(StateFailed)
	r.Reason = `registering zrok name "docpreview-first-preview" in namespace "public": ` +
		`[POST /share/name][409] createShareNameConflict ""`
	r.LogExcerpt = "npm ERR! path D:\\worktrees\\tangents\\vercel-replacement\\.docpreview\\data\\workspaces\\7ac8"

	body := RenderComment(r)

	// The error string carries an internal API path and a conflict code; the log
	// carries the build host's filesystem layout. Neither is a decision anybody
	// made about what to publish.
	for _, leaked := range []string{
		"createShareNameConflict",
		"/share/name",
		"npm ERR!",
		"worktrees",
		".docpreview",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("the comment leaks %q:\n%s", leaked, body)
		}
	}

	if !strings.Contains(body, "build log") {
		t.Errorf("the comment does not say where to find the detail:\n%s", body)
	}
}

func TestFailureCommentLinksToTheBuildLogWhenItCan(t *testing.T) {
	r := testReport(StateFailed)
	r.Reason = "internal detail"
	r.DetailURL = "https://docpreview.example/logs/abc123"

	body := RenderComment(r)
	if !strings.Contains(body, r.DetailURL) {
		t.Errorf("the comment omits the detail URL:\n%s", body)
	}
}

// Every URL the comment publishes must be a markdown link.
//
// GitHub autolinks a bare URL in a table cell; Bitbucket does not, and renders one as
// unclickable text — which is the only thing the comment is for.
func TestEveryURLInTheCommentIsALink(t *testing.T) {
	ready := testReport(StateReady)
	ready.URL = "https://docs-feature-x-b400e0.shares.zrok.io/"
	ready.Name = "docs-feature-x-b400e0"

	failed := testReport(StateFailed)
	failed.DetailURL = "https://docpreview.example/logs/abc123"

	for _, r := range []Report{ready, failed} {
		body := RenderComment(r)
		for _, url := range []string{r.URL, r.DetailURL} {
			if url == "" {
				continue
			}
			if !strings.Contains(body, "["+url+"]("+url+")") {
				t.Errorf("%s is not a link in the %s comment:\n%s", url, r.State, body)
			}
		}
	}
}

func TestFailureCommentNamesTheDashboardWithNoURL(t *testing.T) {
	// dashboard_url is unset, which is the default: the daemon binds loopback and
	// cannot know an address a link would work from. Saying so beats emitting a
	// link to 127.0.0.1.
	r := testReport(StateFailed)
	r.Reason = "internal detail"

	body := RenderComment(r)
	if !strings.Contains(body, "dashboard") {
		t.Errorf("the comment does not say where to look:\n%s", body)
	}
	if strings.Contains(body, "http") {
		t.Errorf("the comment invented a link with no dashboard_url set:\n%s", body)
	}
}

func TestSkipCommentKeepsItsReason(t *testing.T) {
	// A skip's reason is written for the person who opened the pull request, and
	// suppressing it would leave them with a comment that explains nothing.
	r := testReport(StateSkipped)
	r.Reason = "no documentation changes in this push"

	body := RenderComment(r)
	if !strings.Contains(body, r.Reason) {
		t.Errorf("the skip reason was suppressed:\n%s", body)
	}
}

func TestEveryCommentCarriesItsMarkerFirst(t *testing.T) {
	// findComment locates the comment by this marker, and putting it first means
	// a truncated body still identifies itself. A failure path that returns early
	// must not skip it.
	for _, state := range []State{StateQueued, StateBuilding, StateReady, StateFailed, StateSkipped} {
		r := testReport(state)
		r.Reason = "something"
		body := RenderComment(r)
		// Asserted through HasMarker rather than against one spelling: which style is
		// written is a choice that has already changed once, and what must hold is that the
		// matcher finds it. The position is checked separately, because a truncated body
		// still has to identify itself.
		if !HasMarker(body, "abc123") {
			t.Errorf("%s: the body carries no marker:\n%s", state, body)
		}
		if !strings.HasPrefix(body, MarkerFor("abc123", MarkerLinkRef)) {
			t.Errorf("%s: body does not start with the marker:\n%s", state, body)
		}
		// And the HTML comment form must not appear: it is visible in the raw body, which
		// is what taking it out was for.
		if strings.Contains(body, "<!--") {
			t.Errorf("%s: an HTML comment is in the body:\n%s", state, body)
		}
	}
}
