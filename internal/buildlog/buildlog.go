// Package buildlog captures build output: redacted before it touches disk,
// persisted for download, and fanned out to anyone tailing it live.
//
// Three requirements pull in different directions and the design is mostly
// about reconciling them.
//
// Output must be *streamed*, because the point of a live tail is watching a
// build as it happens; buffering until the end defeats it. Output must be
// *redacted*, and redacted before anything durable exists — a file that
// contains a secret for even a moment is a file that can be read, backed up, or
// swept into a crash dump. And redaction operates on text, which means it needs
// whole lines: a secret split across two write calls is invisible to a scrubber
// looking at each call in isolation.
//
// So the writer buffers by line, scrubs each complete line, and only then
// writes it anywhere. Nothing unredacted is ever handed to the file or to a
// subscriber.
package buildlog

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/netfoundry/docpreview/internal/redact"
)

// maxLineLength bounds how much is buffered while waiting for a newline.
//
// A build tool that emits a megabyte without one — a minified bundle echoed to
// stdout, a progress bar drawing with carriage returns — would otherwise grow
// the buffer without limit. At the cap the buffer is flushed in exact
// maxLineLength chunks, which is the one place a secret could slip through a
// split: each chunk is scrubbed, so a secret is only missed if it straddles
// that boundary.
const maxLineLength = 64 * 1024

// readBufferSize is the cap the reader allows, and it is deliberately larger
// than maxLineLength.
//
// The producer and the consumer of this file were originally given the same
// constant with opposite semantics — "flush once at least this much" on one
// side, "reject anything longer than this" on the other. The result was that
// every overlong line written was one byte too long to read back, so a single
// unterminated 64 KiB line made bufio.Scanner return ErrTooLong and Tail
// discard the entire log. Any constant shared between a writer and a reader
// needs slack on the reading side.
const readBufferSize = maxLineLength + 4096

// Writer accepts build output and distributes it, redacted.
//
// Safe for concurrent use: a build's stdout and stderr are both piped into one
// of these, and they interleave.
type Writer struct {
	redactor *redact.Redactor

	mu      sync.Mutex
	partial []byte
	file    *os.File
	path    string
	bytes   int64
	lines   int
	closed  bool

	// writeErr is the first failure writing to the file, surfaced from Close.
	//
	// Dropping it left the worst possible signature for a full disk: the live
	// tail keeps working, the stored log is truncated at an arbitrary point,
	// and the Download button serves a partial file — with nothing anywhere
	// saying why. The daemon already logs a warning when Finish returns an
	// error, so reporting it there needs no new plumbing.
	writeErr error

	subs map[int]chan string
	next int
}

// Create opens a log file and returns a writer for it.
func Create(path string, r *redact.Redactor) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the log directory: %w", err)
	}
	// 0600 rather than os.Create's 0666. The directory is already 0700, but a
	// build log is the artifact whose entire purpose is to hold output that may
	// have contained a credential before scrubbing, and redaction is documented
	// as best effort. Defence in depth costs nothing here.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	return &Writer{
		redactor: r,
		file:     f,
		path:     path,
		subs:     map[int]chan string{},
	}, nil
}

// Write accepts raw build output.
//
// Complete lines are scrubbed and emitted; a trailing partial line is held
// until the rest arrives. The return value reports the full input length even
// though less may have reached the file, because io.Writer's contract is about
// bytes consumed and short writes would make the callers upstream retry.
//
// It never returns an error, and that is deliberate. This writer is one arm of
// an io.MultiWriter feeding a running build, and io.MultiWriter abandons the
// whole write on the first arm that fails. Returning an error after Close —
// which happens whenever a superseding build or a teardown closes this log out
// from under a build still running — would abort the build's own output
// capture, truncating the log text that goes into the pull request comment and
// failing the build for a reason that has nothing to do with the build.
//
// A log is an observer. An observer must not be able to break the thing it
// observes.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return len(p), nil
	}

	w.partial = append(w.partial, p...)

	for {
		i := bytes.IndexByte(w.partial, '\n')

		if i < 0 {
			// No newline yet. Hold the remainder unless it has grown past the
			// cap, in which case flush in exact-sized chunks rather than
			// buffering without limit.
			//
			// Exact chunks, not "whatever has accumulated": one Write can
			// deliver far more than the cap at once, and emitting all of it as
			// a single line would produce a line no reader can scan back.
			for len(w.partial) >= maxLineLength {
				w.emit(string(w.partial[:maxLineLength]))
				w.partial = w.partial[maxLineLength:]
			}
			break
		}

		line := string(w.partial[:i])
		w.partial = w.partial[i+1:]
		w.emit(line)
	}

	return len(p), nil
}

// emit scrubs one line and sends it onwards. The caller holds the mutex.
//
// This is the only path from build output to anywhere persistent or visible, so
// it is the only place redaction has to be correct.
func (w *Writer) emit(line string) {
	line = w.redactor.Scrub(line)

	if w.file != nil {
		if _, err := w.file.WriteString(line + "\n"); err != nil {
			// Record the first failure and stop touching the file. Continuing
			// to write to a broken descriptor produces a stream of identical
			// errors and no more log.
			if w.writeErr == nil {
				w.writeErr = fmt.Errorf("writing to %s: %w", w.path, err)
			}
			w.file.Close()
			w.file = nil
		} else {
			w.bytes += int64(len(line)) + 1
		}
	}
	w.lines++

	for id, ch := range w.subs {
		select {
		case ch <- line:
		default:
			// A subscriber that cannot keep up is dropped rather than allowed
			// to block the build. A stalled browser tab must never be able to
			// hold up a build, and the full log is on disk regardless.
			close(ch)
			delete(w.subs, id)
		}
	}
}

// Subscribe returns a channel of log lines, plus a cancel function.
//
// backlog is how many already-written lines to replay first, so a tail that
// attaches mid-build shows context rather than starting blank.
//
// Replay and registration happen together, under the writer's own lock, and
// that is the whole subtlety. Doing them in either order without the lock
// leaves a window: replay-then-register drops any line emitted in between, and
// register-then-replay delivers the live lines before the older ones they
// follow. Holding the lock across both closes it — emit cannot run, so the file
// cannot grow, so what is on disk is exactly the backlog and everything after
// it arrives on the channel in order.
//
// The cost is that a build is blocked for the length of one file read each time
// a browser attaches. Build logs are kilobytes and this happens once per
// viewer, which is a better trade than a tail that silently skips the line
// somebody was waiting for.
func (w *Writer) Subscribe(buffer, backlog int) (<-chan string, func()) {
	if buffer < 1 {
		buffer = 256
	}
	ch := make(chan string, buffer)

	w.mu.Lock()

	if w.closed {
		// Nothing more is coming. Replay from disk and close, so the caller
		// sees a finished log rather than an empty stream.
		w.mu.Unlock()
		if backlog > 0 {
			if lines, err := Tail(w.path, backlog); err == nil {
				for _, line := range lines {
					select {
					case ch <- line:
					default:
					}
				}
			}
		}
		close(ch)
		return ch, func() {}
	}

	if backlog > 0 {
		// Safe to read the file while holding the lock: emit is the only writer
		// and it needs this same lock, so nothing can append underneath us.
		if lines, err := Tail(w.path, backlog); err == nil {
			for _, line := range lines {
				select {
				case ch <- line:
				default:
					// The backlog does not fit in the buffer. Dropping the
					// oldest context is right — the live lines matter more,
					// and the full log is on disk.
				}
			}
		}
	}

	id := w.next
	w.next++
	w.subs[id] = ch
	w.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if c, ok := w.subs[id]; ok {
				delete(w.subs, id)
				close(c)
			}
		})
	}
}

// Close flushes any partial line and releases the file.
//
// Subscribers are closed too, which is what tells a live tail the build has
// finished rather than merely gone quiet.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true

	if len(w.partial) > 0 {
		w.emit(string(w.partial))
		w.partial = nil
	}

	for id, ch := range w.subs {
		close(ch)
		delete(w.subs, id)
	}

	if w.file == nil {
		return w.writeErr
	}
	err := w.file.Close()
	w.file = nil

	// A write failure mid-build is the more informative of the two, so it wins.
	if w.writeErr != nil {
		return w.writeErr
	}
	return err
}

// Path is where the log is being written.
func (w *Writer) Path() string { return w.path }

// Stats reports what has been emitted so far.
//
// lines counts lines that were scrubbed and sent onwards, which is not quite
// the same as lines on disk: a failed file write still reaches subscribers and
// still counts here. Emitted is the more useful number — it is what a viewer
// saw — but the two can drift if the disk is failing, so do not use this to
// predict what a download will contain.
func (w *Writer) Stats() (lines int, bytes int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lines, w.bytes
}

// Tail returns the last n lines of a log file.
//
// It reads the whole file rather than seeking from the end. Build logs are
// measured in kilobytes, and the seek-and-scan version is fiddly enough to get
// wrong that the simple one is worth the read until a log turns out to be large
// enough to care.
func Tail(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8*1024), readBufferSize)

	for sc.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ring, nil
}

// Meta describes a stored log.
type Meta struct {
	PreviewID string    `json:"preview_id"`
	BuildID   string    `json:"build_id"`
	Path      string    `json:"-"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
}
