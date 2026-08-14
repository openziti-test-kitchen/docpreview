package scm

import (
	"fmt"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
)

// maxLogExcerpt caps how much build output is quoted anywhere a length limit
// applies. GitHub's comment limit is 65536 characters; this leaves the rendering
// room and keeps a runaway build from producing something nobody can scroll past.
//
// No longer used by RenderComment, which quotes no build output at all — see the
// failure branch there. Kept for the surfaces that do quote it and are not
// public.
const maxLogExcerpt = 4000

// RenderComment produces the pull request comment body.
//
// The shape is copied from what Vercel does, because it turns out to be right:
// a single small table that a reviewer can read without scrolling, a link that
// does not move between rebuilds, and a timestamp so it is obvious whether the
// comment reflects the latest push. Everything else — build logs, the reason a
// change was skipped — goes in a collapsed <details> block so the default view
// stays two lines tall on a busy pull request.
func RenderComment(r Report) string {
	var b strings.Builder

	// The marker must be first. It is what findComment looks for, and putting
	// it at the top means a truncated body still identifies itself.
	//
	// A link reference definition rather than an HTML comment, on every platform.
	// `<!-- docpreview:… -->` is invisible in *rendered* markdown but visible in the raw
	// body, which anybody sees when they quote the comment, edit it, or read it through
	// an API. `[docpreview]: #<id>` is consumed by every CommonMark renderer, emits
	// nothing, and needs no raw-HTML support, which is also why Bitbucket cannot render
	// it as a stray paragraph.
	//
	// Safe to switch precisely because HasMarker matches both forms and always will: a
	// daemon upgraded across this change still finds the comments it wrote in the old one
	// and edits them in place. Without that it would post a second comment on every open
	// pull request at once. See scm.HasMarker.
	b.WriteString(MarkerFor(r.PreviewID, MarkerLinkRef))
	b.WriteString("\n\n")

	// One row under headings, rather than one labelled row per field.
	//
	// A field per row made the table taller than the information in it and grew every time
	// something was added. Across, the labels are paid for once, the row is scannable, and a second
	// preview of the same pull request would be a second row rather than a second table.
	//
	// Five columns, and every one of them answers a question somebody actually asks: which project,
	// what state, where do I click, which commit, and is this current.
	b.WriteString("**Documentation preview**\n\n")
	b.WriteString("| Project | Status | Preview | Commit | Updated (UTC) |\n")
	b.WriteString("|---|---|---|---|---|\n")

	// The project is the repository, linked to the dashboard.
	//
	// Not to the build log — that is the status cell below. This is "where does this thing live",
	// which is the dashboard itself, and it is the link that stays useful after the preview is
	// gone.
	project := r.PR.Repo.Owner + "/" + r.PR.Repo.Name
	if r.DashboardURL != "" {
		project = fmt.Sprintf("[%s](%s)", project, r.DashboardURL)
	}

	// The state, linked to its own build log when there is one.
	//
	// "Building" is the state this matters for: the log is tailed live, so the moment worth
	// watching is while the build runs, and the reviewer who just pushed is reading this then. The
	// duration joins it rather than taking a column of its own — it is a footnote to the state and
	// exists only after the build finishes, so a column for it would be empty in every row anybody
	// is watching.
	status := fmt.Sprintf("%s %s", stateIcon(r.State), stateText(r))
	if r.DetailURL != "" {
		status = fmt.Sprintf("%s [%s](%s)", stateIcon(r.State), stateText(r), r.DetailURL)
	}
	if r.Duration > 0 {
		status += fmt.Sprintf(" · %s", r.Duration.Round(time.Second))
	}

	// An explicit link, not a bare URL: GitHub autolinks one and Bitbucket does not, so the one
	// thing this comment exists to deliver would render as unclickable text on one of the two
	// hosts.
	//
	// The word rather than the address. The hostname is forty characters of DNS label that pushes
	// every column after it off the screen, and a reviewer wants to click it rather than read it —
	// the address is still there to copy, in the link.
	preview := "—"
	if r.URL != "" {
		preview = fmt.Sprintf("[Preview](%s)", r.URL)
	}

	commit := "—"
	if r.Commit != "" {
		commit = fmt.Sprintf("`%s`", shortSHA(r.Commit))
	}

	// UTC, and the header says so. A build host, a reviewer and a pull request are routinely in
	// three zones, and the comment cannot know the reader's — so it names the one it used.
	updated := r.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}

	b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
		project, status, preview, commit, updated.UTC().Format("Jan 2, 2006 3:04pm")))

	// A failure says where to look, and nothing else.
	//
	// This comment is public on any public repository, and neither the error
	// string nor the build output was written with that in mind: the reason
	// carries host paths, internal hostnames and third-party API detail, and the
	// log is whatever a build script chose to print. The redactor removes known
	// secret *values*, which is not the same as deciding a line is fit to
	// publish.
	//
	// The detail is not lost. It is in the daemon's log and in the build log,
	// both of which stay on the machine that ran the build.
	// Only when there is no link on the state word. With one, this sentence printed the same URL a
	// second time and said in twelve words what "Failed" already links to.
	if r.State == StateFailed {
		if r.DetailURL == "" {
			b.WriteString("\nThe build failed. See the build log on the docpreview dashboard.\n")
		}
		return b.String()
	}

	// A skip is an explanation written for the person who opened the pull
	// request — "no documentation changes" — so it belongs here.
	if r.Reason != "" {
		b.WriteString("\n")
		b.WriteString(r.Reason)
		b.WriteString("\n")
	}

	return b.String()
}

func stateIcon(s State) string {
	switch s {
	case StateQueued:
		return "⏳"
	case StateBuilding:
		return "🔨"
	case StateReady:
		return "✅"
	case StateSkipped:
		return "⏭️"
	case StateFailed:
		return "❌"
	default:
		return "•"
	}
}

func stateText(r Report) string {
	switch r.State {
	case StateQueued:
		return "Queued"
	case StateBuilding:
		return "Building"
	case StateReady:
		return "Ready"
	case StateSkipped:
		// The reason carries the detail when there is one, and it is written for the person who
		// opened the pull request. Spelling it out here as well printed the same sentence twice.
		if r.Reason != "" {
			return "Skipped"
		}
		return "Skipped, no documentation changes"
	case StateFailed:
		return "Failed"
	default:
		return string(r.State)
	}
}

// shortSHA is model.ShortSHA. The comment, the dashboard and the build log
// filename all render the same commit and must not disagree about it.
func shortSHA(sha string) string { return model.ShortSHA(sha) }

// tail returns the last n characters of s, cut at a line boundary.
//
// The end of a build log is where the error is; the beginning is npm telling
// you about funding. Cutting at a newline avoids opening the excerpt
// mid-escape-sequence, which renders as garbage in a code fence.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return "... (truncated)\n" + cut
}
