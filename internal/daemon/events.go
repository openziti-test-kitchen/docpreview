package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// eventLogSize is how many recent events are kept.
//
// Enough to see a few pull requests move through the whole pipeline, small
// enough that the whole thing serializes into a status response without
// thinking about it. This is a window onto what just happened, not an audit
// trail — the database holds anything durable.
const eventLogSize = 200

// Event is something worth watching happen.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Repo string    `json:"repo"`

	// PreviewID ties the entry to a row. Repo plus number would nearly always
	// identify the same thing, but "nearly" is not good enough for a link: the
	// dashboard would open the wrong build rather than none, which is worse.
	PreviewID string `json:"preview_id"`

	Number  int    `json:"number"`
	Branch  string `json:"branch"`
	Commit  string `json:"commit"`
	Message string `json:"message"`
	URL     string `json:"url,omitempty"`

	// Openable reports whether anything is left to show for this entry — a build
	// log, artifacts, or both.
	//
	// Decided by the server at assembly, never stored, because it is a fact about
	// right now rather than about the moment the event happened. Retention prunes
	// old builds, so an entry that was openable an hour ago may not be, and the
	// feed outlives what it describes: a torn-down preview leaves its history
	// behind on purpose.
	//
	// The page turns an entry into a button only when this is set. A link that
	// looks live and goes nowhere is worse than one that never offered.
	Openable bool `json:"openable"`
}

// eventLog is a fixed-size ring of recent events.
//
// A ring rather than a slice that grows: a daemon left running for a week
// should not accumulate a million events nobody will read, and the alternative —
// trimming a slice on every append — copies the whole thing each time.
type eventLog struct {
	mu   sync.Mutex
	buf  []Event
	next int
	full bool
}

func newEventLog() *eventLog {
	return &eventLog{buf: make([]Event, eventLogSize)}
}

func (l *eventLog) add(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.buf[l.next] = e
	l.next = (l.next + 1) % len(l.buf)
	if l.next == 0 {
		l.full = true
	}
}

// recent returns up to n events, newest first.
func (l *eventLog) recent(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()

	size := l.next
	if l.full {
		size = len(l.buf)
	}
	if n > size {
		n = size
	}

	out := make([]Event, 0, n)
	for i := range n {
		// Walk backwards from the most recent write.
		idx := (l.next - 1 - i + len(l.buf)) % len(l.buf)
		out = append(out, l.buf[idx])
	}
	return out
}

// backfill seeds the activity feed from the recorded build history.
//
// The feed is an in-memory ring, so before this it was empty after every restart
// — a list of "recent activity" that forgot everything the moment the process
// did, which reads as a broken feed rather than as an empty one.
//
// Oldest first, because add() writes into a ring and the newest entry has to end
// up last. Only builds; the queued and skipped states are not recorded per build
// and are transient anyway.
func (d *Daemon) backfill(ctx context.Context, limit int) {
	builds, err := d.store.RecentBuilds(ctx, limit)
	if err != nil {
		d.log.Warn("backfilling the activity feed", "error", err)
		return
	}

	for i := len(builds) - 1; i >= 0; i-- {
		b := builds[i]
		at := b.FinishedAt
		if at.IsZero() {
			at = b.StartedAt
		}
		message := b.State
		if b.Reason != "" {
			message = b.State + " — " + b.Reason
		}
		d.events.add(Event{
			At:        at,
			Kind:      b.State,
			Repo:      b.PR.Repo.String(),
			PreviewID: b.PreviewID,
			Number:    b.PR.Number,
			Branch:    b.PR.Branch,
			Commit:    shortSHA(b.Commit),
			Message:   message,
		})
	}
	if len(builds) > 0 {
		d.log.Info("restored activity history", "builds", len(builds))
	}
}

// record adds an event derived from a report.
func (d *Daemon) record(r scm.Report, message string) {
	d.events.add(Event{
		At:   time.Now(),
		Kind: string(r.State),
		// The same spelling StatusPreview uses, so the dashboard's one
		// project() helper can derive the display name for both. They shipped
		// under the same `repo` JSON key in different formats, which worked
		// only because String() happens to end in the name — and collapsed two
		// repositories with the same name under different owners into one
		// filter chip.
		Repo:      r.PR.Repo.String(),
		PreviewID: r.PreviewID,
		Number:    r.PR.Number,
		Branch:    r.PR.Branch,
		Commit:    shortSHA(r.Commit),
		Message:   message,
		URL:       r.URL,
	})
}

// recordf adds a free-form event for a pull request.
func (d *Daemon) recordf(pr model.PullRequest, kind, message string) {
	d.events.add(Event{
		At:        time.Now(),
		Kind:      kind,
		Repo:      pr.Repo.String(),
		PreviewID: pr.PreviewID(),
		Number:    pr.Number,
		Branch:    pr.Branch,
		Commit:    shortSHA(pr.HeadSHA),
		Message:   message,
	})
}

// shortSHA is model.ShortSHA, aliased so the call sites in this package stay
// short. The definition lives with the other identifier helpers.
func shortSHA(sha string) string { return model.ShortSHA(sha) }
