package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/buildlog"
)

// Server-sent events, replacing what used to be a one-second poll.
//
// Polling was wrong in both directions at once. It is too slow to feel live —
// a build that starts and finishes between two polls is never seen at all — and
// too fast when nothing is happening, since an idle daemon with a tab open
// still answers a request every second forever. SSE inverts that: the browser
// holds one connection and hears about changes when they happen.
//
// SSE rather than WebSockets because the traffic is one-directional. The browser
// has nothing to say back, and a protocol upgrade would buy nothing but a
// second code path for proxies to mishandle.

// heartbeatInterval is how often a comment frame is sent on an idle stream.
//
// Something has to travel periodically or an idle connection is
// indistinguishable from a dead one, and every proxy in between eventually
// reclaims it. A comment frame is the cheapest legal thing to send: the browser
// ignores it, and it resets everyone's idle timer.
const heartbeatInterval = 20 * time.Second

// statusPollInterval is how often the daemon's state is re-read for a stream.
//
// Be clear about what SSE did and did not buy here, because the first version
// of this comment claimed a saving it does not make. The ticker below lives in
// the per-request handler, so the database is still read once per connected
// browser, and at 700ms that is *more* database work than the one-second poll
// it replaced.
//
// What changed is on the other side of the wire. The browser holds one
// connection instead of opening a request per second; it is told when something
// happens rather than asking; a change is visible in well under a second; and
// the payload is only sent when it actually differs, so the dashboard stops
// redrawing itself — and losing text selection — twice a second while nothing
// is going on.
//
// The remaining per-connection read is worth eliminating with one shared poller
// fanning out to subscribers, the way buildlog.Writer already does for log
// lines. It has not been, because two open dashboards is the realistic ceiling
// here and two sqlite reads a second against a local file is not a problem
// worth the machinery yet.
const statusPollInterval = 700 * time.Millisecond

// sseWriter frames values as server-sent events.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSE(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without flushing, every frame sits in the response buffer until the
		// handler returns — which for a stream is never. Better to say so than
		// to hang.
		return nil, errors.New("streaming is not supported by this connection")
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which holds every frame until
	// the buffer fills and makes a live stream arrive in bursts minutes apart.
	h.Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &sseWriter{w: w, flusher: flusher}, nil
}

// event sends a named event carrying JSON.
func (s *sseWriter) event(name string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, payload); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// line sends one log line.
//
// Log lines are sent as raw data rather than JSON because there can be
// thousands of them and the framing is the only structure they need. That makes
// the framing itself the security boundary, because the text is build output
// and anyone who can open a pull request writes it.
//
// The hazard is not just \n. The event-stream grammar terminates a field on
// CR, LF, *or* CRLF, so a bare carriage return inside a log line ends the data
// field as far as the browser is concerned and everything after it is parsed as
// a fresh field. A build printing
//
//	x\revent: done\rdata: {"reason":"build succeeded"}\r\r
//
// would otherwise inject a `done` event, and the dashboard would stop tailing
// and report success on a build that was still running. Redaction does not help
// here: a carriage return is not a secret.
//
// So every terminator is normalized to a single \n and the text is split on
// that, one `data:` field per real line. A control character cannot then
// escape its field.
func (s *sseWriter) line(text string) error {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var b strings.Builder
	b.WriteString("event: line\n")
	for _, part := range strings.Split(text, "\n") {
		b.WriteString("data: ")
		b.WriteString(part)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	if _, err := io.WriteString(s.w, b.String()); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// comment sends a heartbeat the browser ignores.
func (s *sseWriter) comment() error {
	if _, err := io.WriteString(s.w, ": ping\n\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// streamStatus pushes the daemon's state whenever it changes.
func (i *Ingress) streamStatus(w http.ResponseWriter, r *http.Request) {
	sse, err := newSSE(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	ticker := time.NewTicker(statusPollInterval)
	defer ticker.Stop()

	beat := time.NewTicker(heartbeatInterval)
	defer beat.Stop()

	// Only send when something actually changed. A dashboard that redraws
	// itself twice a second flickers, loses text selection, and makes the
	// browser's diff work for nothing.
	var last string

	// Rate-limit the failure log. A locked or corrupt database would otherwise
	// produce a record every 700ms per open tab, indefinitely — which is the
	// classic way a small host fills its disk with logs about being unable to
	// write to the disk.
	var lastLogged time.Time
	logOnce := func(msg string, err error) {
		if time.Since(lastLogged) < time.Minute {
			return
		}
		lastLogged = time.Now()
		i.log.Error(msg, "error", err)
	}

	send := func() bool {
		st, err := i.daemon.Status(ctx)
		if err != nil {
			logOnce("building status for a stream", err)
			return true
		}
		payload, err := json.Marshal(st)
		if err != nil {
			logOnce("encoding status for a stream", err)
			return true
		}
		if string(payload) == last {
			return true
		}
		last = string(payload)
		return sse.event("status", st) == nil
	}

	if !send() {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		case <-beat.C:
			if sse.comment() != nil {
				return
			}
		}
	}
}

// streamLog tails a preview's build log.
//
// Three cases, and the interesting one is the middle. A build in flight streams
// live and ends when it does. A finished build replays from disk and closes
// immediately, so a browser opening the log of yesterday's failure sees it
// without waiting. And a build that finishes *while* being tailed must end the
// stream rather than hang, which is what closing the subscriber channel
// signals.
func (i *Ingress) streamLog(w http.ResponseWriter, r *http.Request) {
	previewID := r.PathValue("preview")
	logs := i.daemon.Logs()

	live, isLive := logs.Live(previewID)

	sse, err := newSSE(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx := r.Context()

	if !isLive {
		// Nothing running. Replay the most recent log and finish.
		meta, err := logs.Latest(previewID)
		if err != nil {
			sse.event("done", map[string]string{"reason": "no build log"})
			return
		}
		lines, err := buildlog.Tail(meta.Path, 5000)
		if err != nil {
			sse.event("done", map[string]string{"reason": "log unreadable"})
			return
		}

		// Say up front that this is history, and which build it is.
		//
		// A queued preview has no output of its own — it is waiting for a
		// worker — so replaying its last build put a completed log under a row
		// marked Queued with nothing distinguishing the two. It reads as the
		// queued build having already produced all of that, which is the
		// opposite of what the row is saying.
		sse.event("start", map[string]any{"build_id": meta.BuildID, "live": false})

		for _, l := range lines {
			if sse.line(l) != nil {
				return
			}
		}
		sse.event("done", map[string]any{"build_id": meta.BuildID, "live": false})
		return
	}

	lines, cancel := live.Subscribe(1024, 500)
	defer cancel()

	sse.event("start", map[string]any{"live": true})

	beat := time.NewTicker(heartbeatInterval)
	defer beat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case l, ok := <-lines:
			if !ok {
				// The writer closed: the build is over. Saying so is what lets
				// the browser stop showing a spinner.
				sse.event("done", map[string]any{"live": true})
				return
			}
			if sse.line(l) != nil {
				return
			}

		case <-beat.C:
			if sse.comment() != nil {
				return
			}
		}
	}
}

// downloadLog serves a complete build log as a file.
func (i *Ingress) downloadLog(w http.ResponseWriter, r *http.Request) {
	previewID := r.PathValue("preview")
	buildID := r.PathValue("build")
	logs := i.daemon.Logs()

	var (
		f    io.ReadCloser
		meta buildlog.Meta
		err  error
	)

	if buildID == "" || buildID == "latest" {
		meta, err = logs.Latest(previewID)
		if err == nil {
			var rc *os.File
			rc, meta, err = logs.Open(previewID, meta.BuildID)
			f = rc
		}
	} else {
		var rc *os.File
		rc, meta, err = logs.Open(previewID, buildID)
		f = rc
	}

	if err != nil {
		// ErrNoLog is the ordinary case — a preview whose logs have been swept,
		// or a build that never produced one. Anything else is a real problem.
		if !errors.Is(err, buildlog.ErrNoLog) {
			i.log.Error("opening a build log", "preview", previewID, "error", err)
		}
		http.Error(w, "no such build log", http.StatusNotFound)
		return
	}
	defer f.Close()

	// A download rather than a render. Build output is arbitrary bytes from a
	// tool nobody vetted, and serving it inline would let it be interpreted.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", previewID+"-"+meta.BuildID+".log"))
	w.Header().Set("Cache-Control", "no-store")

	// A declared length lets the client detect a short read. Without it the
	// response is chunked and simply stops, and a truncated download reads as
	// "the build stopped there" — which is worse than an obvious failure.
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))

	if _, err := io.Copy(w, f); err != nil {
		i.log.Warn("serving a build log", "preview", previewID, "error", err)
	}
}

// listLogs returns the stored logs for a preview.
func (i *Ingress) listLogs(w http.ResponseWriter, r *http.Request) {
	previewID := r.PathValue("preview")

	metas, err := i.daemon.Logs().List(previewID)
	if err != nil {
		// A filesystem failure on the data directory is exactly the thing an
		// operator needs told; the client gets the opaque body either way.
		i.log.Error("listing build logs", "preview", previewID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	_, live := i.daemon.Logs().Live(previewID)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"preview_id": previewID,
		"live":       live,
		"logs":       metas,
	})
}
