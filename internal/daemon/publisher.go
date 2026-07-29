package daemon

import (
	"sync"
	"time"

	"github.com/netfoundry/docpreview/internal/scm"
)

// reportDebounce is how long a report waits for a newer one to replace it.
//
// A quarter second, chosen against the two things it sits between. A build's
// states are seconds or minutes apart, so this is invisible to anyone watching a
// comment. The reports that land inside it are the ones emitted back-to-back by
// different goroutines for the same preview — queued and building, or a supersede
// storm's worth of pushes — which are exactly the writes worth collapsing.
//
// Longer would start delaying "ready", which is the one state somebody is waiting
// for. Shorter stops catching the bursts that motivate it.
const reportDebounce = 250 * time.Millisecond

// publisher coalesces reports per preview before writing them to a platform.
//
// # Why
//
// Every state change is an API write, and a comment upsert is two requests when
// it has to search for the comment first. A build emits four-ish reports, and a
// supersede storm multiplies that by the number of pushes — straight into
// GitHub's secondary rate limit, which fires on burst rather than on volume and
// is the one a busy repository actually hits.
//
// Collapsing is safe because a report is a *snapshot*, not an event. Nothing
// downstream accumulates them: the comment is rewritten whole each time. So a
// report superseded within the window was never going to leave a trace, and
// writing it would only have cost a request.
//
// # What it is not
//
// Not an ordering mechanism. The last report in a window wins, so a debouncer
// fed out-of-order reports faithfully publishes the wrong one — which is why the
// lifecycle guard in Daemon.staleReport exists and why this does not try to do
// its job. By the time a report reaches here it is already known to be the
// furthest state for its commit.
type publisher struct {
	debounce time.Duration
	publish  func(scm.Report, scm.Client)

	mu      sync.Mutex
	pending map[string]*pendingReport
	stopped bool
	wg      sync.WaitGroup
}

// pendingReport is the newest report for one preview and the timer that will
// write it.
type pendingReport struct {
	report scm.Report
	client scm.Client
	timer  *time.Timer
}

func newPublisher(debounce time.Duration, publish func(scm.Report, scm.Client)) *publisher {
	return &publisher{
		debounce: debounce,
		publish:  publish,
		pending:  map[string]*pendingReport{},
	}
}

// send queues a report, replacing any still waiting for the same preview.
//
// Keyed by preview rather than globally: two pull requests reporting at once are
// unrelated, and making one wait behind the other would turn a shared timer into
// a shared latency.
func (p *publisher) send(r scm.Report, client scm.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		// After Close the timers are gone, so a straggler is written through
		// rather than dropped. Shutdown is exactly when a terminal state most
		// needs to reach the pull request.
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.publish(r, client)
		}()
		return
	}

	if existing, ok := p.pending[r.PreviewID]; ok {
		// Replace the payload and leave the timer running. Resetting it on every
		// report would let a steady stream of them postpone the write forever,
		// which is the classic debounce failure — a preview that never reports
		// because it keeps almost reporting.
		existing.report = r
		existing.client = client
		return
	}

	previewID := r.PreviewID
	entry := &pendingReport{report: r, client: client}
	entry.timer = time.AfterFunc(p.debounce, func() { p.flush(previewID) })
	p.pending[previewID] = entry
}

// flush writes whatever is pending for a preview.
func (p *publisher) flush(previewID string) {
	p.mu.Lock()
	entry, ok := p.pending[previewID]
	if !ok {
		p.mu.Unlock()
		return
	}
	delete(p.pending, previewID)
	r, client := entry.report, entry.client
	p.wg.Add(1)
	p.mu.Unlock()

	defer p.wg.Done()
	p.publish(r, client)
}

// Close writes everything pending and waits for it.
//
// Without this, a shutdown inside the debounce window loses the last report of
// every in-flight build — most often the terminal one, leaving a comment reading
// "Building" for a build that finished. A quarter second of held-back writes is
// not worth losing that.
func (p *publisher) Close() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		p.wg.Wait()
		return
	}
	p.stopped = true

	entries := make([]*pendingReport, 0, len(p.pending))
	for id, entry := range p.pending {
		entry.timer.Stop()
		entries = append(entries, entry)
		delete(p.pending, id)
	}
	p.mu.Unlock()

	// Outside the lock: publishing is a network round trip, and a flush racing
	// this would otherwise block on it.
	for _, entry := range entries {
		p.publish(entry.report, entry.client)
	}
	p.wg.Wait()
}
