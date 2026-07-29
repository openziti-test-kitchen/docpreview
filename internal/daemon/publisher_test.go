package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/scm"
)

// recorder captures what a publisher actually wrote.
type recorder struct {
	mu      sync.Mutex
	written []scm.Report
}

func (r *recorder) publish(rep scm.Report, _ scm.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.written = append(r.written, rep)
}

func (r *recorder) states() []scm.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]scm.State, len(r.written))
	for i, rep := range r.written {
		out[i] = rep.State
	}
	return out
}

func report(previewID string, state scm.State) scm.Report {
	return scm.Report{PreviewID: previewID, State: state, Commit: "abc123"}
}

func TestPublisherCollapsesABurstToTheLastReport(t *testing.T) {
	// The point of the thing. A build emits several states in quick succession and
	// each one is an API write; a report is a snapshot rather than an event, so the
	// ones overtaken inside the window were never going to leave a trace.
	rec := &recorder{}
	p := newPublisher(20*time.Millisecond, rec.publish)

	p.send(report("one", scm.StateQueued), nil)
	p.send(report("one", scm.StateBuilding), nil)
	p.send(report("one", scm.StateReady), nil)

	p.Close()

	got := rec.states()
	if len(got) != 1 || got[0] != scm.StateReady {
		t.Errorf("wrote %v, want one %q", got, scm.StateReady)
	}
}

func TestPublisherKeepsPreviewsIndependent(t *testing.T) {
	// Keyed per preview, because two pull requests reporting at once are
	// unrelated and a shared timer would make one wait on the other.
	rec := &recorder{}
	p := newPublisher(20*time.Millisecond, rec.publish)

	p.send(report("one", scm.StateReady), nil)
	p.send(report("two", scm.StateReady), nil)
	p.Close()

	if got := rec.states(); len(got) != 2 {
		t.Errorf("wrote %d reports for two previews, want 2: %v", len(got), got)
	}
}

func TestPublisherDoesNotPostponeIndefinitely(t *testing.T) {
	// The classic debounce failure: resetting the timer on every report lets a
	// steady stream postpone the write forever, so a preview that reports
	// constantly never reports at all. The timer is deliberately not reset.
	rec := &recorder{}
	p := newPublisher(40*time.Millisecond, rec.publish)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			p.send(report("one", scm.StateBuilding), nil)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// Well inside the 200 ms the stream runs for, and well past one window.
	time.Sleep(120 * time.Millisecond)
	wrote := len(rec.states())

	<-done
	p.Close()

	if wrote == 0 {
		t.Error("nothing was written while reports kept arriving; the timer is being reset")
	}
}

func TestPublisherFlushesOnClose(t *testing.T) {
	// A shutdown inside the window would otherwise lose the last report of every
	// in-flight build — usually the terminal one, leaving a comment reading
	// "Building" for a build that finished.
	rec := &recorder{}
	p := newPublisher(time.Hour, rec.publish)

	p.send(report("one", scm.StateReady), nil)
	p.Close()

	got := rec.states()
	if len(got) != 1 || got[0] != scm.StateReady {
		t.Errorf("wrote %v after Close, want one %q", got, scm.StateReady)
	}
}

func TestPublisherWritesThroughAfterClose(t *testing.T) {
	// The timers are gone after Close, so a straggler is written directly rather
	// than dropped. Shutdown is when a terminal state most needs to arrive.
	rec := &recorder{}
	p := newPublisher(time.Hour, rec.publish)
	p.Close()

	p.send(report("one", scm.StateFailed), nil)
	p.Close() // waits for the in-flight write

	if got := rec.states(); len(got) != 1 || got[0] != scm.StateFailed {
		t.Errorf("wrote %v, want one %q", got, scm.StateFailed)
	}
}

func TestPublisherCloseIsIdempotent(t *testing.T) {
	// Run calls it on shutdown and a test or a second signal may call it again.
	rec := &recorder{}
	p := newPublisher(time.Hour, rec.publish)
	p.send(report("one", scm.StateReady), nil)

	p.Close()
	p.Close()

	if got := rec.states(); len(got) != 1 {
		t.Errorf("wrote %d reports across two Closes, want 1", len(got))
	}
}

// The lifecycle guard. A debouncer publishes whichever report was last, so it
// cannot fix ordering — these cover the thing that does.

func TestStaleReportsAreDroppedWithinACommit(t *testing.T) {
	d := &Daemon{reported: map[string]reportMark{}}

	if d.staleReport(report("one", scm.StateBuilding)) {
		t.Fatal("the first report was treated as stale")
	}
	// queued after building: the inversion that put a live build's comment back to
	// "Queued". Its timestamp is later, which is why time cannot decide this.
	if !d.staleReport(report("one", scm.StateQueued)) {
		t.Error("queued was accepted after building for the same commit")
	}
	if d.staleReport(report("one", scm.StateReady)) {
		t.Error("ready was rejected after building")
	}
}

func TestEqualStatesAreNotStale(t *testing.T) {
	// Two ready reports for one commit are legitimate: a republish after a restart
	// can move the URL, and the comment has to be rewritten. So the test is
	// strictly-below, not below-or-equal.
	d := &Daemon{reported: map[string]reportMark{}}

	d.staleReport(report("one", scm.StateReady))
	if d.staleReport(report("one", scm.StateReady)) {
		t.Error("a second ready for the same commit was dropped")
	}
}

func TestANewCommitResetsTheLifecycle(t *testing.T) {
	// ready → queued is backwards within one commit and correct across two.
	d := &Daemon{reported: map[string]reportMark{}}

	d.staleReport(report("one", scm.StateReady))

	next := report("one", scm.StateQueued)
	next.Commit = "def456"
	if d.staleReport(next) {
		t.Error("the next push's queued was dropped as stale")
	}
}

func TestUnknownStatesAreTerminalNotStale(t *testing.T) {
	// A state this code has not been taught about must not be silently dropped.
	d := &Daemon{reported: map[string]reportMark{}}

	d.staleReport(report("one", scm.StateReady))
	if d.staleReport(report("one", scm.State("torn-down"))) {
		t.Error("an unrecognised state was dropped as stale")
	}
}
