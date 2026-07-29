# Build logs and streaming

Three requirements pull in different directions, and the design is mostly about reconciling them.

1. **Streamed.** The point of a live tail is watching a build as it happens; buffering until the end defeats it.
2. **Redacted, before anything durable exists.** A file containing a secret for even a moment can be read,
   backed up, or swept into a crash dump.
3. **Redaction needs whole lines.** A secret split across two `Write` calls is invisible to a scrubber looking
   at each call in isolation.

So the writer **buffers by line, scrubs each complete line, and only then writes it anywhere.**

## `buildlog.Writer`

```go
w.partial = append(w.partial, p...)
for {
    i := bytes.IndexByte(w.partial, '\n')
    if i < 0 {
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
```

Safe for concurrent use: a build's stdout and stderr are both piped into one of these and they interleave.

`emit` is the only path from build output to anywhere persistent or visible, so it is the only place redaction
has to be correct.

### `Write` never returns an error

Deliberate. This writer is one arm of an `io.MultiWriter` feeding a running build, and **`io.MultiWriter`
abandons the whole write on the first arm that fails**. Returning an error after `Close` — which happens
whenever a superseding build or a teardown closes this log out from under a build still running — would abort
the build's own output capture, truncating the log text that goes into the pull request comment and failing the
build for a reason that has nothing to do with the build.

**A log is an observer. An observer must not be able to break the thing it observes.**

Write errors are recorded in `writeErr` and surfaced from `Close`. Dropping them left the worst possible
signature for a full disk: the live tail keeps working, the stored log is truncated at an arbitrary point, and
the Download button serves a partial file, with nothing anywhere saying why.

### The two length constants

```go
const maxLineLength  = 64 * 1024
const readBufferSize = maxLineLength + 4096
```

They differ on purpose. The producer and consumer were originally given the *same* constant with opposite
semantics — "flush once at least this much" on one side, "reject anything longer than this" on the other. Every
overlong line written was one byte too long to read back, so a single unterminated 64 KiB line made
`bufio.Scanner` return `ErrTooLong` and `Tail` discard **the entire log**.

**Any constant shared between a writer and a reader needs slack on the reading side.**

The test that missed this only `stat`'d the file. A test that asserts a file is non-empty asserts nothing about
whether it can be read.

### Subscribe

```go
func (w *Writer) Subscribe(buffer, replay int) (<-chan string, func())
```

Replay and registration happen **under one lock**. An earlier version replayed, then registered, and the comment
above it claimed "at worst a line is delivered twice" while the code produced a *gap* — lines emitted between
the two steps were lost.

This was the third instance in one session of a comment stating the correct invariant above code doing the
opposite. Treat a confident comment next to concurrency code as a claim to verify, not a fact.

The race test was proved to have teeth by reintroducing the bug: it failed at attempt 3 of 25.

A slow subscriber is dropped rather than allowed to block `emit`. A browser that cannot keep up must not stall
the build.

## `buildlog.Store`

One directory per preview, one file per build:

```
data/logs/<previewID>/<YYYYMMDD-HHMMSS>-<shortSHA>.log
```

Commit plus timestamp, because the commit alone collides when the same commit is rebuilt — every retry and every
reopened pull request — and a timestamp alone sorts correctly but tells you nothing about what was built.

Files are `0600`; the directory is `0700`.

**Every method tolerates a nil receiver.** A log store that could not be created is not fatal — the daemon
degrades to building without capturing output rather than refusing to start, because a full disk should not take
previews down entirely. Nil-tolerance is what keeps that from scattering nil checks through the daemon.

`Begin` swaps the live writer under the lock and closes the old one **outside** it, so a slow close cannot block
a new build from starting.

`Sweep` re-checks liveness under the lock before `RemoveAll`, so a build that started between the listing and
the delete does not have its directory removed underneath it.

`previewDir` rejects any preview ID containing a separator or a dot. The ID arrives from a URL path and becomes
a directory name.

## Server-sent events

Two streams, both in `internal/daemon/stream.go`.

| Endpoint | Carries |
|---|---|
| `GET /events` | `status` events — the whole `Status` payload |
| `GET /logs/{preview}/stream` | `start`, `line`, `done` |

SSE rather than polling: the dashboard used to poll every second from every open tab.

### Framing, and the carriage-return injection

```go
func (s *sse) line(text string) error {
    text = strings.ReplaceAll(text, "\r\n", "\n")
    text = strings.ReplaceAll(text, "\r", "\n")
    ...
}
```

**`\r` is a field terminator in the event-stream grammar, not just `\n`.** Build output is written by whoever
opened the pull request. Without this normalization a build could emit

```
x\revent: done\rdata: {"reason":"build succeeded"}\r
```

and forge its own event — for instance a `done` that stops the dashboard tailing and reports success on a build
still running. Redaction is no help: a carriage return is not a secret.

Test: `TestSSELineCannotForgeAnEventWithCarriageReturns`, which asserts per *line* that no field other than our
own `event: line` appears at the start of one.

### `start` — is this live or history?

A queued preview has no output of its own; it is waiting for a worker. The stream replays its last completed
build. Without saying so, the pane showed a finished log under a row marked Queued — which reads as the queued
build having already produced all of it.

```
event: start
data: {"build_id":"20260728-183138-7c6a873","live":false}
```

The dashboard renders a banner from it. `start` is sent **before** any line: a label that arrives after the
content it labels is not a label.

### Headers

```
Content-Type: text/event-stream
Cache-Control: no-store
X-Accel-Buffering: no
```

The last one matters in particular: nginx buffers proxied responses by default, which holds every frame until
the buffer fills and makes a live stream arrive in bursts minutes apart.

### Shutdown

`http.Server.Shutdown` waits for in-flight requests and does **not** cancel their contexts. SSE handlers block
until their client disconnects, and the dashboard opens two the moment it loads — so without `BaseContext`
wiring the signal context into every request, one open browser tab makes every Ctrl-C sit for the full
30-second shutdown timeout and then log a failure.

### Downloads

`GET /logs/{preview}/download` serves as an **attachment** with `nosniff` and a declared `Content-Length`.
Build output is arbitrary bytes from a tool nobody vetted; rendering it inline would let it be interpreted. The
length is what lets a client notice a short read.

## Deliberate limits

- **`statusPollInterval` is per connection.** Each open dashboard costs two sqlite reads every 700 ms. One
  shared poller fanning out to subscribers would fix it; deferred because two open dashboards is the realistic
  ceiling.
- **`Tail` reads at most 5000 lines.** A longer log is downloadable in full.
- **No search.** Finding a line in a 5000-line build means downloading it.

## Invariants

1. Nothing unredacted is written to the file or handed to a subscriber.
2. `Write` never returns an error.
3. The reader's cap exceeds the writer's.
4. Replay and registration are atomic with respect to `emit`.
5. A slow subscriber is dropped, never blocks the build.
6. `\r` cannot survive into an event frame.
7. `start` precedes the lines it describes.
8. Every `Store` method tolerates a nil receiver.
