package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/redact"
)

// httptest.ResponseRecorder implements http.Flusher, so the SSE handlers run
// against it without a real connection.

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestSSELineCannotForgeAnEventWithCarriageReturns(t *testing.T) {
	// Build output is written by whoever opened the pull request, and it is
	// framed into the event stream as raw data. The event-stream grammar ends a
	// field on CR as well as LF, so a bare carriage return would let a build
	// inject its own `event:` line — for instance a `done` event that stops the
	// dashboard tailing and reports success on a build still running.
	//
	// Redaction is no help: a carriage return is not a secret.
	rec := httptest.NewRecorder()
	sse, err := newSSE(rec)
	if err != nil {
		t.Fatal(err)
	}

	if err := sse.line("x\revent: done\rdata: {\"reason\":\"build succeeded\"}\r"); err != nil {
		t.Fatal(err)
	}

	body := rec.Body.String()

	// The distinction that matters is per *line*: an "event:" appearing inside
	// a data field is inert text, one at the start of a line is a real field.
	lines := strings.Split(body, "\n")

	var fields int
	for i, l := range lines {
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "data: ") {
			fields++
			if i != 0 || l != "event: line" {
				t.Errorf("injected field escaped the data channel at line %d: %q\nframe:\n%q",
					i, l, body)
			}
		}
	}
	if fields != 1 {
		t.Errorf("frame contains %d non-data fields, want only our own:\n%q", fields, body)
	}
	if strings.Contains(body, "\r") {
		t.Errorf("a carriage return survived into the frame:\n%q", body)
	}
}

func TestSSELineSplitsEmbeddedNewlines(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, _ := newSSE(rec)

	if err := sse.line("one\ntwo"); err != nil {
		t.Fatal(err)
	}

	body := rec.Body.String()
	if n := strings.Count(body, "data: "); n != 2 {
		t.Errorf("got %d data fields, want 2:\n%q", n, body)
	}
}

func TestSSEHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	if _, err := newSSE(rec); err != nil {
		t.Fatal(err)
	}

	// X-Accel-Buffering matters in particular: nginx buffers proxied responses
	// by default, which holds every frame until the buffer fills and makes a
	// live stream arrive in bursts minutes apart.
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestStreamLogReplaysAFinishedBuild(t *testing.T) {
	ingress, d, _ := testIngress(t, &fakeClient{})

	r, _ := redact.New(nil)
	w, err := d.Logs().Begin("preview1", "20260101-000000-abc1234", r)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(w, "$ npm run build")
	fmt.Fprintln(w, "built 3 pages")
	if err := d.Logs().Finish("preview1", w); err != nil {
		t.Fatal(err)
	}

	rec := get(t, ingress.Handler(), "/logs/preview1/stream")
	body := rec.Body.String()

	// The content, not "data: " plus the content. Every stored line now begins with a
	// timestamp, so the text no longer sits flush against the SSE field name — and what
	// this test is about is whether the line was replayed at all.
	if !strings.Contains(body, "$ npm run build") {
		t.Errorf("the log was not replayed:\n%s", body)
	}
	// The done event is what stops the browser showing a spinner forever.
	if !strings.Contains(body, "event: done") {
		t.Errorf("the stream did not end with a done event:\n%s", body)
	}
}

func TestStreamLogSaysWhetherItIsLiveOrAReplay(t *testing.T) {
	// A queued preview has no output of its own — it is waiting for a worker —
	// so the stream replays its last completed build. Without saying so, the
	// pane showed a finished log under a row marked Queued, which reads as the
	// queued build having already produced all of it.
	ingress, d, _ := testIngress(t, &fakeClient{})

	r, _ := redact.New(nil)
	w, err := d.Logs().Begin("preview1", "20260101-000000-abc1234", r)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(w, "$ yarn build")
	if err := d.Logs().Finish("preview1", w); err != nil {
		t.Fatal(err)
	}

	body := get(t, ingress.Handler(), "/logs/preview1/stream").Body.String()

	if !strings.Contains(body, "event: start") {
		t.Errorf("no start event, so the client cannot label the pane:\n%s", body)
	}
	if !strings.Contains(body, `"live":false`) {
		t.Errorf("the replay does not declare itself historical:\n%s", body)
	}
	// And it names which build, so "previous build" is not the only thing said.
	if !strings.Contains(body, "20260101-000000-abc1234") {
		t.Errorf("the start event does not name the build:\n%s", body)
	}
	// The start event must precede the lines, or the label arrives after the
	// content it is labelling.
	if strings.Index(body, "event: start") > strings.Index(body, "$ yarn build") {
		t.Errorf("start arrives after the log lines:\n%s", body)
	}
}

func TestStreamLogOnAPreviewWithNoLog(t *testing.T) {
	ingress, _, _ := testIngress(t, &fakeClient{})

	rec := get(t, ingress.Handler(), "/logs/nosuchpreview/stream")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — the stream reports the absence in-band", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no build log") {
		t.Errorf("body does not explain the absence:\n%s", rec.Body.String())
	}
}

func TestDownloadLogNotFound(t *testing.T) {
	ingress, _, _ := testIngress(t, &fakeClient{})

	rec := get(t, ingress.Handler(), "/logs/nosuchpreview/download")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDownloadLogServesAsAnAttachment(t *testing.T) {
	ingress, d, _ := testIngress(t, &fakeClient{})

	r, _ := redact.New(nil)
	w, _ := d.Logs().Begin("preview1", "20260101-000000-abc1234", r)
	fmt.Fprintln(w, "log contents")
	d.Logs().Finish("preview1", w)

	rec := get(t, ingress.Handler(), "/logs/preview1/download")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	// An attachment with nosniff, because build output is arbitrary bytes from
	// a tool nobody vetted and rendering it inline would let it be interpreted.
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff on a build log download")
	}
	// A declared length is what lets a client notice a short read.
	if rec.Header().Get("Content-Length") == "" {
		t.Error("no Content-Length, so a truncated download is undetectable")
	}
	if !strings.Contains(rec.Body.String(), "log contents") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestListLogs(t *testing.T) {
	ingress, d, _ := testIngress(t, &fakeClient{})

	r, _ := redact.New(nil)
	w, _ := d.Logs().Begin("preview1", "20260101-000000-abc1234", r)
	fmt.Fprintln(w, "x")
	d.Logs().Finish("preview1", w)

	rec := get(t, ingress.Handler(), "/logs/preview1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{`"preview_id"`, `"logs"`, "20260101-000000-abc1234"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("payload missing %s:\n%s", want, rec.Body.String())
		}
	}
}

func TestDownloadRejectsATraversingPreviewID(t *testing.T) {
	// The id arrives from the URL path and becomes a directory name. Two
	// defences apply and either is sufficient: net/http's mux cleans the path
	// before routing (so `..` redirects rather than reaching the handler), and
	// Store.previewDir rejects anything containing a separator or a dot.
	//
	// The assertion is therefore "no log content is ever served", not a
	// specific status — pinning the status would make this test fail if the
	// mux's normalization changed, which would be a false alarm.
	ingress, _, _ := testIngress(t, &fakeClient{})

	for _, bad := range []string{"..", "a.b", "a:b", "..%2Fescape"} {
		rec := get(t, ingress.Handler(), "/logs/"+bad+"/download")

		if rec.Code == http.StatusOK {
			t.Errorf("preview id %q served content: %q", bad, rec.Body.String())
		}
		if cd := rec.Header().Get("Content-Disposition"); cd != "" {
			t.Errorf("preview id %q produced a download header %q", bad, cd)
		}
	}
}
