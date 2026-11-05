package expose

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

var err429 = errors.New("unable to create share: [POST /share] share (status 429): {}")

func testGate(t *testing.T, dir string, limit int, window, quiet time.Duration) *ShareGate {
	t.Helper()
	g := NewShareGate(dir, "test", nil)
	g.limit, g.window, g.quiet, g.sleep = limit, window, quiet, 20*time.Millisecond
	return g
}

func ok() error { return nil }

func TestShareGateLetsTheLimitThroughThenWaitsForTheWindow(t *testing.T) {
	g := testGate(t, t.TempDir(), 3, 2*time.Second, time.Second)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := g.CreateShare(context.Background(), "a", ShareReasonPublish, ok); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the first three waited %s; the window should let them straight through", d)
	}
	st, err := g.Status(10)
	if err != nil {
		t.Fatal(err)
	}
	if st.Used != 3 || st.WindowCloses == nil || !st.NextSlot.Equal(*st.WindowCloses) {
		t.Fatalf("status after the limit = %+v, want 3 used and the next slot at the window's close", st)
	}
	if len(st.Recent) != 3 || st.Recent[0].Outcome != "created" {
		t.Fatalf("history = %+v, want three created", st.Recent)
	}

	if err := g.CreateShare(context.Background(), "d", ShareReasonPublish, ok); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 1900*time.Millisecond {
		t.Fatalf("the fourth went through after %s, before the window closed", d)
	}
}

func TestShareGateCountsAcrossGatesOnOneDirectory(t *testing.T) {
	dir := t.TempDir()
	a := testGate(t, dir, 2, 400*time.Millisecond, time.Second)
	b := testGate(t, dir, 2, 400*time.Millisecond, time.Second)
	start := time.Now()
	_ = a.CreateShare(context.Background(), "a", ShareReasonPublish, ok)
	_ = b.CreateShare(context.Background(), "b", ShareReasonWebhook, ok)
	_ = a.CreateShare(context.Background(), "c", ShareReasonPublish, ok)
	if d := time.Since(start); d < 350*time.Millisecond {
		t.Fatalf("the third share across two processes went through after %s; they must share one budget", d)
	}
}

func TestShareGateGoesQuietAfterTwo429sInARow(t *testing.T) {
	g := testGate(t, t.TempDir(), 15, time.Hour, 400*time.Millisecond)
	limited := func() error { return err429 }

	_ = g.CreateShare(context.Background(), "a", ShareReasonPublish, limited)
	if st, _ := g.Status(0); st.QuietUntil != nil {
		t.Fatal("one 429 started the quiet period; it takes two in a row")
	}
	_ = g.CreateShare(context.Background(), "a", ShareReasonPublish, ok) // resets the run
	_ = g.CreateShare(context.Background(), "a", ShareReasonPublish, limited)
	if st, _ := g.Status(0); st.QuietUntil != nil {
		t.Fatal("a 429 after a success started the quiet period")
	}
	_ = g.CreateShare(context.Background(), "a", ShareReasonPublish, limited)
	st, _ := g.Status(5)
	if st.QuietUntil == nil {
		t.Fatal("two 429s in a row did not start the quiet period")
	}
	if st.Used != 0 {
		t.Fatalf("used = %d during the quiet period, want the window closed", st.Used)
	}
	if st.Recent[0].Outcome != "rate-limited" {
		t.Fatalf("newest history entry = %+v, want rate-limited", st.Recent[0])
	}

	start := time.Now()
	if err := g.CreateShare(context.Background(), "b", ShareReasonPublish, ok); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 350*time.Millisecond {
		t.Fatalf("a share went through %s into the quiet period", d)
	}
}

func TestShareGateServesWaitersInOrderAndShowsThem(t *testing.T) {
	g := testGate(t, t.TempDir(), 1, 300*time.Millisecond, time.Second)
	_ = g.CreateShare(context.Background(), "first", ShareReasonPublish, ok)

	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for _, name := range []string{"second", "third"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.CreateShare(context.Background(), name, ShareReasonPublish, func() error {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				return nil
			})
		}()
		time.Sleep(30 * time.Millisecond) // queued in this order
	}

	st, _ := g.Status(0)
	if len(st.Queued) != 2 || st.Queued[0].Name != "second" {
		t.Fatalf("queued = %+v, want second then third", st.Queued)
	}
	wg.Wait()
	if len(order) != 2 || order[0] != "second" {
		t.Fatalf("served %v, want second before third", order)
	}
}

func TestShareGateGivesUpWhenTheContextEnds(t *testing.T) {
	g := testGate(t, t.TempDir(), 1, time.Hour, time.Second)
	_ = g.CreateShare(context.Background(), "a", ShareReasonPublish, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ran := false
	err := g.CreateShare(ctx, "b", ShareReasonPublish, func() error { ran = true; return nil })
	if err == nil || ran {
		t.Fatalf("err = %v, ran = %v; want the wait abandoned and nothing created", err, ran)
	}
	if st, _ := g.Status(0); len(st.Queued) != 0 {
		t.Fatalf("an abandoned waiter is still queued: %+v", st.Queued)
	}
}

func TestNilShareGateLetsEverythingThrough(t *testing.T) {
	var g *ShareGate
	if err := g.CreateShare(context.Background(), "a", ShareReasonPublish, ok); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimitedZrokRecognisesTheShareCreate429(t *testing.T) {
	if !RateLimitedZrok(err429) {
		t.Fatal("the 429 creating a share answers is not recognised as a rate limit")
	}
}

// A rebuild serves the new site on the share the preview already has, and the daemon closing the
// previous publication afterwards does not take that share down.
func TestZrokRebuildReusesTheLiveShare(t *testing.T) {
	z := &Zrok{live: map[string]*zrokShare{}, log: slog.New(slog.DiscardHandler)}
	serve := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
	}
	site := newSwapHandler(serve("old"))
	first := &zrokShare{token: "tok", previewID: "p1", name: "repo-main", site: site,
		url: "https://repo-main.shares.zrok.io/"}
	z.live["p1"] = first
	oldPub := NewPublication(first.url, first.name, func() error { z.withdrawEntry("p1", first); return nil })

	pub := z.reuse(Spec{PreviewID: "p1", Name: "repo-main"}, serve("new"))
	if pub == nil || pub.URL != first.url {
		t.Fatalf("reuse = %+v, want the existing share's URL", pub)
	}
	if err := oldPub.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := z.live["p1"]; !ok {
		t.Fatal("closing the previous publication withdrew the reused share")
	}
	rec := httptest.NewRecorder()
	site.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Body.String() != "new" {
		t.Fatalf("the share serves %q, want the new build", rec.Body.String())
	}

	if z.reuse(Spec{PreviewID: "p1", Name: "repo-renamed"}, serve("x")) != nil {
		t.Fatal("a different name reused the share; it needs a new one")
	}
	if z.reuse(Spec{PreviewID: "p1", Name: "repo-main", BaseURL: "/docs/"}, serve("x")) != nil {
		t.Fatal("a different base URL reused the share; its URL would be wrong")
	}
}
