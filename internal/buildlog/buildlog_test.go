package buildlog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/redact"
)

const secret = "dpfake_9f2a1c4b7e01d3f5a8c6b2e4"

// unstamp removes a line's timestamp prefix.
//
// Every emitted line begins with `15:04:05.000 ` — see stamp. Tests about ordering, replay
// and redaction are about the *rest* of the line, so they strip it rather than embedding a
// clock in every expectation. The prefix's own shape is asserted once, in
// TestEveryLineCarriesATimestamp, which is where a change to it should fail.
func unstamp(line string) string {
	if len(line) < stampWidth {
		return line
	}
	// Only when it looks like one: a line shorter than the prefix, or one from a test that
	// bypassed emit, must come back unchanged rather than losing its first thirteen bytes.
	if line[2] != ':' || line[5] != ':' || line[8] != '.' || line[stampWidth-1] != ' ' {
		return line
	}
	return line[stampWidth:]
}

func unstampAll(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = unstamp(l)
	}
	return out
}

func newTestWriter(t *testing.T, secrets ...string) (*Writer, string) {
	t.Helper()

	r, _ := redact.New(secrets)
	path := filepath.Join(t.TempDir(), "build.log")

	w, err := Create(path, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, path
}

func TestNothingUnredactedReachesDisk(t *testing.T) {
	// The core promise. A log file that holds a secret for even a moment is a
	// file that can be read, backed up, or swept into a crash dump.
	w, path := newTestWriter(t, secret)

	fmt.Fprintf(w, "npm ERR! token %s rejected\n", secret)
	w.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the secret reached disk: %s", raw)
	}
	if !strings.Contains(string(raw), redact.Mask) {
		t.Errorf("nothing was masked: %s", raw)
	}
}

func TestSecretSplitAcrossWritesIsStillRedacted(t *testing.T) {
	// The reason this writer buffers by line at all. A scrubber looking at each
	// Write call in isolation sees two harmless fragments; the secret only
	// exists once they are joined. os/exec hands over whatever the pipe
	// happened to deliver, so the split point is arbitrary and this happens.
	w, path := newTestWriter(t, secret)

	half := len(secret) / 2
	w.Write([]byte("token " + secret[:half]))
	w.Write([]byte(secret[half:] + " end\n"))
	w.Close()

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("a secret split across two writes survived: %s", raw)
	}
}

func TestSecretSplitAcrossManyWrites(t *testing.T) {
	// The pathological version: one byte at a time.
	w, path := newTestWriter(t, secret)

	w.Write([]byte("prefix "))
	for _, c := range []byte(secret) {
		w.Write([]byte{c})
	}
	w.Write([]byte(" suffix\n"))
	w.Close()

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("a byte-at-a-time secret survived: %s", raw)
	}
}

func TestPartialLineIsFlushedOnClose(t *testing.T) {
	// A build whose last line has no trailing newline must not lose it — that
	// last line is very often the error.
	w, path := newTestWriter(t)

	w.Write([]byte("complete\nincomplete without newline"))
	w.Close()

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "incomplete without newline") {
		t.Errorf("the trailing partial line was lost: %q", raw)
	}
}

func TestOverlongLineIsFlushedRatherThanBuffered(t *testing.T) {
	// A tool that emits a megabyte with no newline — a minified bundle, a
	// progress bar drawn with carriage returns — must not grow the buffer
	// without limit.
	w, path := newTestWriter(t)

	w.Write([]byte(strings.Repeat("x", maxLineLength+1024)))
	w.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < maxLineLength {
		t.Errorf("the overlong line was dropped rather than flushed: %d bytes", info.Size())
	}
}

func TestOverlongLineStaysReadable(t *testing.T) {
	// The writer's flush cap and the reader's scanner cap were the same number
	// with opposite meanings — "flush at least this much" against "reject more
	// than this much" — so every flushed overlong line was one byte too long to
	// read back. bufio.Scanner returned ErrTooLong and Tail then discarded the
	// *entire* log, which surfaced as a blank log viewer with no explanation.
	//
	// Stat'ing the file, which is all the test above did, cannot catch that.
	// Reading it back is the whole point.
	for _, size := range []int{
		maxLineLength,
		maxLineLength + 1,
		maxLineLength * 3,
		maxLineLength*2 + 77,
	} {
		w, path := newTestWriter(t)

		fmt.Fprintln(w, "before")
		w.Write([]byte(strings.Repeat("x", size)))
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "after")
		w.Close()

		lines, err := Tail(path, 1000)
		if err != nil {
			t.Fatalf("a %d-byte unterminated line made the whole log unreadable: %v", size, err)
		}
		if len(lines) == 0 {
			t.Fatalf("a %d-byte unterminated line produced no readable lines", size)
		}
		if unstamp(lines[0]) != "before" {
			t.Errorf("size %d: first line = %q, want \"before\"", size, lines[0])
		}
		if last := unstamp(lines[len(lines)-1]); last != "after" {
			t.Errorf("size %d: last line = %q, want \"after\"", size, last)
		}
		// No emitted line may exceed the cap, or the next reader hits the same
		// wall.
		for i, l := range lines {
			if len(l) > maxLineLength {
				t.Errorf("size %d: line %d is %d bytes, over the %d cap",
					size, i, len(l), maxLineLength)
			}
		}
	}
}

func TestWriteAfterCloseDoesNotFailTheBuild(t *testing.T) {
	// This writer is one arm of an io.MultiWriter feeding a running build, and
	// io.MultiWriter abandons the whole write on the first arm that errors. A
	// superseding build or a teardown closes this log out from under a build
	// still running; returning an error then would truncate the build's own
	// captured output and fail it for an unrelated reason.
	//
	// An observer must not be able to break the thing it observes.
	w, _ := newTestWriter(t)
	w.Close()

	n, err := w.Write([]byte("output after the log was closed\n"))
	if err != nil {
		t.Fatalf("Write after Close returned %v; io.MultiWriter would abort the build", err)
	}
	if n != len("output after the log was closed\n") {
		t.Errorf("Write reported %d bytes, want the full length", n)
	}

	// And the realistic shape: a MultiWriter with a closed log as one arm still
	// delivers everything to the build's own buffer.
	var buf strings.Builder
	closed, _ := newTestWriter(t)
	closed.Close()

	multi := io.MultiWriter(&buf, closed)
	if _, err := io.WriteString(multi, "line one\nline two\n"); err != nil {
		t.Fatalf("MultiWriter aborted because of the closed log: %v", err)
	}
	if buf.String() != "line one\nline two\n" {
		t.Errorf("the build's own log was truncated: %q", buf.String())
	}
}

func TestWriteErrorSurfacesFromClose(t *testing.T) {
	// A full disk mid-build otherwise produces a live tail that keeps working,
	// a truncated file, and a Download button serving a partial log — with
	// nothing anywhere saying why.
	r, _ := redact.New(nil)
	path := filepath.Join(t.TempDir(), "build.log")

	w, err := Create(path, r)
	if err != nil {
		t.Fatal(err)
	}

	// Close the descriptor behind the writer's back to simulate the failure.
	w.mu.Lock()
	w.file.Close()
	w.mu.Unlock()

	fmt.Fprintln(w, "this write cannot land")

	if err := w.Close(); err == nil {
		t.Fatal("Close reported success after the file write failed")
	}
}

func TestSubscriberReceivesLinesLive(t *testing.T) {
	w, _ := newTestWriter(t, secret)

	lines, cancel := w.Subscribe(16, 0)
	defer cancel()

	go func() {
		fmt.Fprintf(w, "first\ntoken %s\nthird\n", secret)
	}()

	got := make([]string, 0, 3)
	deadline := time.After(5 * time.Second)
	for len(got) < 3 {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the subscriber channel closed early")
			}
			got = append(got, l)
		case <-deadline:
			t.Fatalf("timed out after %d lines: %v", len(got), got)
		}
	}

	if unstamp(got[0]) != "first" || unstamp(got[2]) != "third" {
		t.Errorf("lines arrived wrong: %v", got)
	}
	if strings.Contains(got[1], secret) {
		t.Errorf("a subscriber received an unredacted secret: %q", got[1])
	}
}

func TestSubscribeMidBuildLosesNoLines(t *testing.T) {
	// The replay-then-register race. A line emitted after Tail reads the file
	// but before the channel is in w.subs is in neither the backlog nor the
	// live stream, so a tail attaching mid-build silently skips exactly the
	// line somebody was waiting for.
	//
	// Driven hard because the window is small: a writer running flat out while
	// subscribers attach, then every line accounted for.
	const (
		total    = 300
		backlog  = 25 // far smaller than total, so most lines must arrive live
		attempts = 25
	)

	for attempt := range attempts {
		w, _ := newTestWriter(t)

		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := range total {
				fmt.Fprintf(w, "line %d\n", i)
				// Slow enough that the subscriber reliably attaches partway
				// through rather than after the writer has already finished,
				// which would make this test prove nothing.
				time.Sleep(50 * time.Microsecond)
			}
			w.Close()
		}()

		time.Sleep(time.Duration(attempt) * 300 * time.Microsecond)

		lines, cancel := w.Subscribe(total*2, backlog)

		var got []string
		for l := range lines {
			got = append(got, l)
		}
		cancel()
		<-done

		if len(got) < backlog+10 {
			t.Fatalf("attempt %d: only %d lines, so the subscriber attached "+
				"after the writer finished and this proves nothing", attempt, len(got))
		}

		// The run must be contiguous: whatever the first line delivered was,
		// every line after it must follow with no gap. A dropped line in the
		// handoff window shows up here as a jump.
		var first int
		if _, err := fmt.Sscanf(unstamp(got[0]), "line %d", &first); err != nil {
			t.Fatalf("unexpected first line %q", got[0])
		}
		for i, l := range unstampAll(got) {
			want := fmt.Sprintf("line %d", first+i)
			if l != want {
				t.Fatalf("attempt %d: gap in the stream at index %d: got %q, want %q "+
					"(a line was lost between backlog replay and live registration)",
					attempt, i, l, want)
			}
		}

		// And it must reach the end, or lines were lost at the close.
		if last := unstamp(got[len(got)-1]); last != fmt.Sprintf("line %d", total-1) {
			t.Fatalf("attempt %d: stream ended at %q, want the final line", attempt, last)
		}
	}
}

func TestSubscribeToAFinishedWriterReplaysAndCloses(t *testing.T) {
	// Attaching after the build ended must show the log and finish, not hang
	// on an empty stream.
	w, _ := newTestWriter(t)
	fmt.Fprintln(w, "one")
	fmt.Fprintln(w, "two")
	w.Close()

	lines, cancel := w.Subscribe(16, 10)
	defer cancel()

	var got []string
	for l := range lines {
		got = append(got, l)
	}

	if plain := unstampAll(got); len(plain) != 2 || plain[0] != "one" || plain[1] != "two" {
		t.Errorf("replay of a finished log = %v, want [one two]", got)
	}
}

func TestCloseEndsSubscribers(t *testing.T) {
	// This is how a live tail learns the build finished rather than merely went
	// quiet. Without it a browser shows a spinner forever.
	w, _ := newTestWriter(t)

	lines, cancel := w.Subscribe(4, 0)
	defer cancel()

	w.Close()

	select {
	case _, ok := <-lines:
		if ok {
			t.Error("expected the channel to be closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the subscriber")
	}
}

func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	// A stalled browser tab must never hold up a build. The full log is on disk
	// regardless, so dropping the subscriber costs nothing that matters.
	w, _ := newTestWriter(t)

	_, cancel := w.Subscribe(1, 0) // never read from
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := range 500 {
			fmt.Fprintf(w, "line %d\n", i)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a slow subscriber blocked the build")
	}
}

func TestConcurrentWritesAreSafe(t *testing.T) {
	// stdout and stderr are both piped into one writer, and os/exec gives each
	// its own goroutine.
	w, path := newTestWriter(t, secret)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				fmt.Fprintf(w, "worker %d line %d token %s\n", i, j, secret)
			}
		}()
	}
	wg.Wait()
	w.Close()

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), secret) {
		t.Error("a secret survived concurrent writes")
	}
	if n := strings.Count(string(raw), "\n"); n != 400 {
		t.Errorf("got %d lines, want 400 — writes interleaved destructively", n)
	}
}

func TestTail(t *testing.T) {
	w, path := newTestWriter(t)
	for i := range 100 {
		fmt.Fprintf(w, "line %d\n", i)
	}
	w.Close()

	got, err := Tail(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if plain := unstampAll(got); len(plain) != 5 || plain[0] != "line 95" || plain[4] != "line 99" {
		t.Errorf("Tail = %v", got)
	}
}

// TestEveryLineCarriesATimestamp is where the prefix's shape is pinned, so the other tests
// can strip it without each embedding a clock.
//
// The stamp is when this process *read* the line, not when the build printed it — the two
// differ whenever the writing process block-buffers, which everything inside the container
// does because its stdout is a pipe. That is stated on `stamp` and is the reason this
// asserts a format rather than a value.
func TestEveryLineCarriesATimestamp(t *testing.T) {
	w, path := newTestWriter(t, secret)
	w.clockFn = func() time.Time {
		return time.Date(2026, 7, 30, 21, 56, 7, 762_000_000, time.UTC)
	}

	fmt.Fprintln(w, "hello")
	fmt.Fprintln(w, secret)
	w.Close()

	lines, err := Tail(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %v", len(lines), lines)
	}
	if want := "21:56:07.762 hello"; lines[0] != want {
		t.Errorf("line = %q, want %q", lines[0], want)
	}
	// Stamped *and* redacted: the prefix is added after scrubbing, so neither can displace
	// the other.
	if !strings.HasPrefix(lines[1], "21:56:07.762 ") {
		t.Errorf("the second line lost its stamp: %q", lines[1])
	}
	if strings.Contains(lines[1], secret) {
		t.Errorf("stamping a line skipped its redaction: %q", lines[1])
	}
}

func TestStoreLifecycle(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, _ := redact.New([]string{secret})

	w, err := s.Begin("preview1", "20260101-120000-abc1234", r)
	if err != nil {
		t.Fatal(err)
	}

	if _, live := s.Live("preview1"); !live {
		t.Error("the in-flight build is not reported as live")
	}

	fmt.Fprintf(w, "building with %s\n", secret)

	if err := s.Finish("preview1", w); err != nil {
		t.Fatal(err)
	}
	if _, live := s.Live("preview1"); live {
		t.Error("a finished build is still reported as live")
	}

	logs, err := s.List("preview1")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("got %d logs, want 1", len(logs))
	}

	f, _, err := s.Open("preview1", logs[0].BuildID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
}

func TestStorePrunesOldLogs(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, _ := redact.New(nil)

	for i := range keepPerPreview + 4 {
		w, err := s.Begin("preview1", fmt.Sprintf("build-%02d", i), r)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "build %d\n", i)
		s.Finish("preview1", w)
		// Distinct mtimes, since pruning sorts by them.
		time.Sleep(10 * time.Millisecond)
	}

	logs, err := s.List("preview1")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != keepPerPreview {
		t.Errorf("kept %d logs, want %d", len(logs), keepPerPreview)
	}
	// Newest first, and the newest must be the last one written.
	if logs[0].BuildID != "build-08" {
		t.Errorf("newest log is %q, want build-08", logs[0].BuildID)
	}
}

func TestStoreRejectsPathTraversal(t *testing.T) {
	// Preview IDs are hex from a hash so this cannot happen today, but this is
	// the one place a caller-supplied string becomes a filesystem path.
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, _ := redact.New(nil)

	for _, bad := range []string{"", "..", "../escape", `..\escape`, "a/b", "a:b", "a.b"} {
		if _, err := s.Begin(bad, "build", r); err == nil {
			t.Errorf("Begin accepted preview id %q", bad)
		}
		if _, err := s.List(bad); err == nil && bad != "" {
			t.Errorf("List accepted preview id %q", bad)
		}
	}
}

func TestStoreSweepRespectsLiveBuilds(t *testing.T) {
	// A build in flight must survive the sweep however old its directory looks,
	// or a long build would have its own log deleted out from under it.
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, _ := redact.New(nil)

	w, err := s.Begin("preview1", "build-1", r)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(w, "still going")

	if _, err := s.Sweep(time.Nanosecond); err != nil {
		t.Fatal(err)
	}

	if _, live := s.Live("preview1"); !live {
		t.Fatal("the sweep dropped a live build")
	}
	if logs, _ := s.List("preview1"); len(logs) != 1 {
		t.Error("the sweep deleted an in-flight build's log")
	}
	s.Finish("preview1", w)
}

func TestNilStoreIsUsable(t *testing.T) {
	// A store that could not be created degrades to "builds run, they just are
	// not captured" rather than a nil check at every call site.
	var s *Store
	r, _ := redact.New(nil)

	w, err := s.Begin("p", "b", r)
	if err != nil || w != nil {
		t.Errorf("Begin on a nil store = %v, %v", w, err)
	}
	if err := s.Finish("p", nil); err != nil {
		t.Errorf("Finish: %v", err)
	}
	if _, live := s.Live("p"); live {
		t.Error("a nil store reports a live build")
	}
	if err := s.Remove("p"); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if n, err := s.Sweep(time.Hour); n != 0 || err != nil {
		t.Errorf("Sweep = %d, %v", n, err)
	}
}
