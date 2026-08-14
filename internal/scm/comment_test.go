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

// Every URL the comment publishes must be the target of a markdown link.
//
// GitHub autolinks a bare URL and Bitbucket does not, rendering one as unclickable text — which is
// the only thing the comment is for. Asserted on the target rather than on `[url](url)`, because
// the link text is not always the URL: the preview link shows its own address, and the build log is
// reached through the state word.
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
			if !strings.Contains(body, "]("+url+")") {
				t.Errorf("%s is not a link target in the %s comment:\n%s", url, r.State, body)
			}
			// And it appears nowhere as bare text: every occurrence is inside a link.
			// Whole links first: stripping the target out of `[url](url)` would leave `[url`
			// behind and report the link text as bare.
			bare := strings.ReplaceAll(body, "["+url+"]("+url+")", "")
			bare = strings.ReplaceAll(bare, "]("+url+")", "")
			if strings.Contains(bare, url) {
				t.Errorf("%s also appears as bare text in the %s comment:\n%s", url, r.State, body)
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

// One row under headings, five columns wide, however the build turned out.
//
// A field per row grew the table every time something was added and made it taller than the
// information in it. Across, the shape is fixed: a reviewer learns where to look once, and a
// column that has no value says so rather than collapsing and shifting the ones after it.
func TestTheReportIsOneRowUnderHeadings(t *testing.T) {
	r := testReport(StateReady)
	r.URL = "https://a-acme-docs-add-guide.shares.zrok.io/"
	r.Name = "a-acme-docs-add-guide"
	r.DashboardURL = "https://docpreview.example/"
	r.Duration = 117 * time.Second

	body := RenderComment(r)

	for _, want := range []string{
		"| Project | Status | Preview | Commit | Updated (UTC) |",
		"[acme/docs](https://docpreview.example/)",
		"✅ Ready · 1m57s",
		"[Preview](" + r.URL + ")",
		"`d141f6e`",
		"Jul 29, 2026 12:00pm",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the comment does not carry %q:\n%s", want, body)
		}
	}

	// The publication name has no column. It is the first label of the hostname in the preview
	// link, so it said nothing the URL does not, in forty characters.
	//
	// Checked with the URL removed, because the name is a substring of it — that is the whole
	// reason the column went away.
	if strings.Contains(strings.ReplaceAll(body, r.URL, ""), r.Name) {
		t.Errorf("the publication name is back in the comment:\n%s", body)
	}

	// Every state renders the same five columns, so nothing shifts as a build progresses.
	for _, state := range []State{StateQueued, StateBuilding, StateReady, StateFailed, StateSkipped} {
		s := testReport(state)
		s.DashboardURL = "https://docpreview.example/"
		row := dataRow(t, RenderComment(s))
		if got := strings.Count(row, "|"); got != 6 {
			t.Errorf("%s renders %d cell borders, want 6 — the row is not five columns:\n%s",
				state, got, row)
		}
	}
}

// A column with no value carries a placeholder rather than nothing.
//
// An empty cell in a rendered table is a gap the eye reads as a rendering fault, and on a build
// that has not published there are two of them side by side.
func TestAnEmptyColumnSaysSo(t *testing.T) {
	r := testReport(StateBuilding)
	r.DashboardURL = "https://docpreview.example/"

	row := dataRow(t, RenderComment(r))
	if strings.Contains(row, "|  |") || strings.Contains(row, "| |") {
		t.Errorf("a cell is empty rather than saying it has no value:\n%s", row)
	}
}

// With no dashboard address configured the project is plain text. A link to nowhere is worse than
// no link, and the daemon binds loopback so it cannot know an address that would work.
func TestTheProjectIsPlainTextWithNoDashboardURL(t *testing.T) {
	body := RenderComment(testReport(StateReady))
	if !strings.Contains(body, "| acme/docs |") {
		t.Errorf("the project is not named as plain text:\n%s", body)
	}
	if strings.Contains(body, "http") {
		t.Errorf("a link was invented with no dashboard_url set:\n%s", body)
	}
}

// dataRow returns the table's single data row: the line after the delimiter.
func dataRow(t *testing.T, body string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "|---") && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	t.Fatalf("no table in:\n%s", body)
	return ""
}

// "Building" links to the build log, so a reviewer who just pushed can watch it.
//
// The log is tailed live, which makes the useful moment the one while the build is running. A link
// that only appears once the build has failed arrives after the thing worth watching.
func TestBuildingLinksToItsOwnLog(t *testing.T) {
	r := testReport(StateBuilding)
	r.DetailURL = "https://docpreview.example/#preview=abc123"

	body := RenderComment(r)
	if !strings.Contains(body, "[Building]("+r.DetailURL+")") {
		t.Errorf("the state is not a link to the log:\n%s", body)
	}
}

// With no dashboard address configured there is nothing to link to, and the state is still there.
// A link to nowhere is worse than a word.
func TestTheStateIsPlainTextWithNoLogToLinkTo(t *testing.T) {
	body := RenderComment(testReport(StateBuilding))
	if strings.Contains(body, "[Building]") {
		t.Errorf("a link was rendered with no URL behind it:\n%s", body)
	}
	if !strings.Contains(body, "🔨 Building") {
		t.Errorf("the state is missing:\n%s", body)
	}
}

// A build that has not finished has no duration, and the row must not invent one.
func TestTheStatusRowOmitsADurationItDoesNotHave(t *testing.T) {
	body := RenderComment(testReport(StateBuilding))
	if strings.Contains(body, "built in") {
		t.Errorf("a build in flight reports a duration:\n%s", body)
	}
	if !strings.Contains(body, "🔨 Building") {
		t.Errorf("the state is missing from the status row:\n%s", body)
	}
}
