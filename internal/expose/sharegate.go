package expose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The zrok account's budget for creating shares, as nfpreview spends it.
//
// zrok answers 429 when an account creates shares too quickly, and the controller does not say what
// its limit is. These numbers are the operator's, chosen to stay under it: no more than fifteen
// shares in ten minutes, and ten minutes of silence once the controller has refused twice in a row,
// because a third attempt inside the same refusal only extends it.
const (
	ShareGateLimit  = 15
	ShareGateWindow = 10 * time.Minute
	ShareGateQuiet  = 10 * time.Minute

	// shareGateQuietAfter is how many 429s in a row start the quiet period. One can be a
	// neighbour's burst; two in a row means the account itself is over.
	shareGateQuietAfter = 2
)

// Why a share was asked for. Logged and recorded with every attempt, so a count of shares can be
// broken down by cause rather than read as one number.
const (
	ShareReasonPublish   = "publish"
	ShareReasonRestore   = "restore"
	ShareReasonReclaim   = "reclaim"
	ShareReasonWebhook   = "webhook listener"
	ShareReasonDashboard = "dashboard listener"
)

// File names inside the zrok environment directory. Beside the environment rather than in data_dir
// because the budget belongs to the account the environment is enrolled in, and the two tunnel
// processes know that directory and nothing else.
const (
	shareGateStateFile   = "nfpreview-share-gate.json"
	shareGateHistoryFile = "nfpreview-shares.jsonl"
)

// ShareGate queues every zrok share creation this machine makes and lets them through at no more
// than ShareGateLimit per ShareGateWindow.
//
// # The window
//
// Fixed, not sliding, and opened by the first attempt: the clock starts when a share is first
// asked for, the next fourteen go straight through, and anything after that waits for the window
// to close. The next attempt after that opens a new one. That is the rule the operator stated,
// and it is easy to read off the dashboard: "7 of 15 used, window closes at 14:32".
//
// # Across processes
//
// The daemon, `webhook-only` and `dashboard-only` all create shares from one zrok environment,
// and the controller counts them together. So the count lives in a file in that environment's
// directory, read and written under a lock file, and every process that loads the same
// environment spends from the same budget. That includes a tunnel unit stuck in a restart loop:
// each new process finds the window already spent and waits, instead of creating another share
// every ten seconds.
//
// Within one process, waiters are served in the order they asked.
type ShareGate struct {
	dir     string
	process string
	log     *slog.Logger
	now     func() time.Time
	sleep   time.Duration // the longest one wait lasts before the state file is read again

	limit  int
	window time.Duration
	quiet  time.Duration

	mu      sync.Mutex
	queue   []*ShareWaiter
	changed chan struct{}
}

// ShareWaiter is one share creation waiting for its turn.
type ShareWaiter struct {
	Name   string    `json:"name"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// shareGateState is what the state file holds. The whole budget, for every process.
type shareGateState struct {
	WindowStart    time.Time `json:"window_start"`
	Used           int       `json:"used"`
	QuietUntil     time.Time `json:"quiet_until"`
	Consecutive429 int       `json:"consecutive_429"`
	LastAttempt    time.Time `json:"last_attempt"`
}

// ShareAttempt is one line of the history file: a share creation and how it ended.
type ShareAttempt struct {
	At      time.Time `json:"at"`
	Process string    `json:"process"`
	Name    string    `json:"name"`
	Reason  string    `json:"reason"`
	Outcome string    `json:"outcome"` // "created", "rate-limited" or "failed"
	Error   string    `json:"error,omitempty"`
}

// NewShareGate builds a gate whose state lives in dir. process names this process in the log and
// the history ("daemon", "webhook-only", "dashboard-only").
func NewShareGate(dir, process string, log *slog.Logger) *ShareGate {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ShareGate{
		dir: dir, process: process, log: log, now: time.Now, sleep: 5 * time.Second,
		limit: ShareGateLimit, window: ShareGateWindow, quiet: ShareGateQuiet,
		changed: make(chan struct{}),
	}
}

var (
	defaultGateMu sync.Mutex
	defaultGate   *ShareGate
)

// SetDefaultShareGate installs the gate every zrok share creation in this process goes through.
// Called once at startup, after the zrok environment has been chosen.
func SetDefaultShareGate(g *ShareGate) {
	defaultGateMu.Lock()
	defaultGate = g
	defaultGateMu.Unlock()
}

// DefaultShareGate is the gate SetDefaultShareGate installed, or nil when there is none — a test,
// or an exposer that is not zrok. A nil gate lets everything through.
func DefaultShareGate() *ShareGate {
	defaultGateMu.Lock()
	defer defaultGateMu.Unlock()
	return defaultGate
}

// ZrokRootDir is the directory of the zrok environment this process uses: the one UseZrokRoot
// chose, or the machine's `~/.zrok2`.
func ZrokRootDir() (string, error) {
	zrokRootMu.Lock()
	chosen := zrokRootChosen
	zrokRootMu.Unlock()
	if chosen.set && filepath.IsAbs(chosen.path) {
		return chosen.path, nil
	}
	return SystemZrokDir()
}

// CreateShare runs create, one zrok share creation, when the gate lets it through.
//
// Every attempt is counted, including one that fails: the controller counted it. A nil gate runs
// create at once.
func (g *ShareGate) CreateShare(ctx context.Context, name, reason string, create func() error) error {
	if g == nil {
		return create()
	}
	if err := g.wait(ctx, name, reason); err != nil {
		return err
	}
	err := create()
	g.record(name, reason, err)
	return err
}

// wait blocks until this attempt may run, and counts it.
func (g *ShareGate) wait(ctx context.Context, name, reason string) error {
	me := &ShareWaiter{Name: name, Reason: reason, Since: g.now()}
	g.mu.Lock()
	g.queue = append(g.queue, me)
	g.mu.Unlock()
	defer g.leave(me)

	told := false
	for {
		g.mu.Lock()
		head := g.queue[0] == me
		changed := g.changed
		g.mu.Unlock()

		var until time.Time
		if head {
			var err error
			until, err = g.take()
			if err != nil {
				// The ledger cannot be read or written. Letting the share through is the lesser
				// failure: the controller still has its own limit, and a broken file must not
				// stop every preview from publishing.
				g.log.Warn("zrok share gate unavailable; not limiting", "error", err)
				return nil
			}
			if until.IsZero() {
				return nil
			}
			if !told {
				g.log.Info("zrok share queued", "name", name, "reason", reason,
					"until", until.UTC().Format(time.RFC3339))
				told = true
			}
		}

		// The head re-reads the file at least every g.sleep, because another process may have
		// changed the window. Everybody else waits for the queue to move.
		var timer <-chan time.Time
		if head {
			d := min(until.Sub(g.now()), g.sleep)
			timer = time.After(max(d, 10*time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for a zrok share slot for %s: %w", name, ctx.Err())
		case <-changed:
		case <-timer:
		}
	}
}

// leave removes a waiter from the queue and wakes the rest.
func (g *ShareGate) leave(me *ShareWaiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, w := range g.queue {
		if w == me {
			g.queue = append(g.queue[:i], g.queue[i+1:]...)
			break
		}
	}
	close(g.changed)
	g.changed = make(chan struct{})
}

// take spends one share from the budget, or says when to try again. A zero time means spent.
func (g *ShareGate) take() (time.Time, error) {
	var until time.Time
	err := g.update(func(s *shareGateState) {
		now := g.now()
		if now.Before(s.QuietUntil) {
			until = s.QuietUntil
			return
		}
		if s.WindowStart.IsZero() || !now.Before(s.WindowStart.Add(g.window)) {
			s.WindowStart, s.Used = now, 0
		}
		if s.Used >= g.limit {
			until = s.WindowStart.Add(g.window)
			return
		}
		s.Used++
		s.LastAttempt = now
	})
	return until, err
}

// record notes how an attempt ended: in the state, the history file and the log.
func (g *ShareGate) record(name, reason string, err error) {
	a := ShareAttempt{At: g.now().UTC(), Process: g.process, Name: name, Reason: reason, Outcome: "created"}
	limited := RateLimitedZrok(err)
	switch {
	case limited:
		a.Outcome, a.Error = "rate-limited", err.Error()
	case err != nil:
		a.Outcome, a.Error = "failed", err.Error()
	}

	var st shareGateState
	if uerr := g.update(func(s *shareGateState) {
		if !limited {
			s.Consecutive429 = 0
		} else {
			s.Consecutive429++
			if s.Consecutive429 >= shareGateQuietAfter {
				// The window is over: the controller says so. The next attempt after the quiet
				// period opens a new one.
				s.QuietUntil = g.now().Add(g.quiet)
				s.WindowStart, s.Used, s.Consecutive429 = time.Time{}, 0, 0
			}
		}
		st = *s
	}); uerr != nil {
		g.log.Warn("recording a zrok share attempt", "error", uerr)
	}

	attrs := []any{"name", name, "reason", reason, "outcome", a.Outcome, "process", g.process,
		"used", st.Used, "limit", g.limit}
	if !st.WindowStart.IsZero() {
		attrs = append(attrs, "window_closes", st.WindowStart.Add(g.window).UTC().Format(time.RFC3339))
	}
	if !st.QuietUntil.IsZero() && st.QuietUntil.After(g.now()) {
		attrs = append(attrs, "quiet_until", st.QuietUntil.UTC().Format(time.RFC3339))
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	if limited {
		g.log.Warn("zrok share", attrs...)
	} else {
		g.log.Info("zrok share", attrs...)
	}

	if herr := g.appendHistory(a); herr != nil {
		g.log.Warn("writing the zrok share history", "error", herr)
	}
}

// ShareGateStatus is the gate as the dashboard shows it.
type ShareGateStatus struct {
	Limit         int            `json:"limit"`
	WindowSeconds int            `json:"window_seconds"`
	Used          int            `json:"used"`
	WindowStart   *time.Time     `json:"window_start,omitempty"`
	WindowCloses  *time.Time     `json:"window_closes,omitempty"`
	QuietUntil    *time.Time     `json:"quiet_until,omitempty"`
	NextSlot      time.Time      `json:"next_slot"`
	Queued        []ShareWaiter  `json:"queued"`
	Recent        []ShareAttempt `json:"recent"`
}

// Status reads the budget and this process's queue. recent is how many history lines to include,
// newest first.
func (g *ShareGate) Status(recent int) (ShareGateStatus, error) {
	out := ShareGateStatus{Limit: g.limit, WindowSeconds: int(g.window / time.Second), Queued: []ShareWaiter{}}
	g.mu.Lock()
	for _, w := range g.queue {
		out.Queued = append(out.Queued, *w)
	}
	g.mu.Unlock()

	var s shareGateState
	if err := g.update(func(st *shareGateState) { s = *st }); err != nil {
		return out, err
	}
	now := g.now()
	out.NextSlot = now
	if now.Before(s.QuietUntil) {
		q := s.QuietUntil
		out.QuietUntil, out.NextSlot = &q, q
	}
	if !s.WindowStart.IsZero() && now.Before(s.WindowStart.Add(g.window)) {
		ws, wc := s.WindowStart, s.WindowStart.Add(g.window)
		out.WindowStart, out.WindowCloses, out.Used = &ws, &wc, s.Used
		if s.Used >= g.limit && wc.After(out.NextSlot) {
			out.NextSlot = wc
		}
	}

	hist, err := g.readHistory(recent)
	if err != nil {
		return out, err
	}
	out.Recent = hist
	return out, nil
}

// update reads the state file under the lock, lets fn change it, and writes it back.
func (g *ShareGate) update(fn func(*shareGateState)) error {
	unlock, err := g.lock()
	if err != nil {
		return err
	}
	defer unlock()

	path := filepath.Join(g.dir, shareGateStateFile)
	var s shareGateState
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if jerr := json.Unmarshal(b, &s); jerr != nil {
			// A torn or hand-edited file. Starting the budget again is safer than refusing every
			// share; the controller's own limit still stands behind this one.
			g.log.Warn("the zrok share gate state was unreadable; starting it again", "error", jerr)
			s = shareGateState{}
		}
	}
	before := s
	fn(&s)
	if s == before {
		return nil
	}
	out, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lock takes the cross-process lock: a file created exclusively, removed on unlock. One left
// behind by a killed process is taken over after half a minute, which is far longer than any
// holder keeps it — the lock covers a file read and a write, never a network call.
func (g *ShareGate) lock() (func(), error) {
	path := filepath.Join(g.dir, shareGateStateFile+".lock")
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > 30*time.Second {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the zrok share gate lock %s is held", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// appendHistory adds one attempt to the history file.
func (g *ShareGate) appendHistory(a ShareAttempt) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(g.dir, shareGateHistoryFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	return errors.Join(werr, f.Close())
}

// readHistory returns up to n of the newest attempts, newest first.
func (g *ShareGate) readHistory(n int) ([]ShareAttempt, error) {
	out := []ShareAttempt{}
	if n <= 0 {
		return out, nil
	}
	// The tail only. The file grows by a line per share for as long as the environment lives, and
	// this runs on every dashboard poll.
	f, err := os.Open(filepath.Join(g.dir, shareGateHistoryFile))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer f.Close()
	const tail = 64 << 10
	fi, err := f.Stat()
	if err != nil {
		return out, err
	}
	off := max(fi.Size()-tail, 0)
	b := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && !errors.Is(err, io.EOF) {
		return out, err
	}
	if off > 0 {
		// Drop the partial first line.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	end := len(b)
	for end > 0 && len(out) < n {
		start := end - 1
		for start > 0 && b[start-1] != '\n' {
			start--
		}
		line := b[start:end]
		end = start
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			continue
		}
		var a ShareAttempt
		if json.Unmarshal(line, &a) == nil {
			out = append(out, a)
		}
	}
	return out, nil
}
