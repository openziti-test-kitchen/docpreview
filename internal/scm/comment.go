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
	b.WriteString(Marker(r.PreviewID))
	b.WriteString("\n\n**Documentation preview**\n\n")

	b.WriteString("| | |\n|---|---|\n")
	b.WriteString(fmt.Sprintf("| **Status** | %s %s |\n", stateIcon(r.State), stateText(r)))

	if r.URL != "" {
		b.WriteString(fmt.Sprintf("| **Preview** | %s |\n", r.URL))
	}
	if r.Name != "" {
		b.WriteString(fmt.Sprintf("| **Name** | `%s` |\n", r.Name))
	}
	if r.Commit != "" {
		b.WriteString(fmt.Sprintf("| **Commit** | `%s` |\n", shortSHA(r.Commit)))
	}
	if r.Duration > 0 {
		b.WriteString(fmt.Sprintf("| **Built in** | %s |\n", r.Duration.Round(time.Second)))
	}

	updated := r.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	b.WriteString(fmt.Sprintf("| **Updated** | %s |\n", updated.UTC().Format("2006-01-02 15:04:05 UTC")))

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
	if r.State == StateFailed {
		b.WriteString("\nThe build failed. See the build log for details")
		if r.DetailURL != "" {
			b.WriteString(": " + r.DetailURL)
		} else {
			b.WriteString(" on the docpreview dashboard")
		}
		b.WriteString("\n")
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
		return "Skipped — no documentation changes"
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
