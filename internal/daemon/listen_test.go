package daemon

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestOpenServesEveryTCPListener(t *testing.T) {
	// One http.Server over several listeners is the whole mechanism behind
	// mixing TCP and overlay ingress. If a second listener were not served,
	// the failure in production is an address that accepts connections and
	// never answers — which looks like a hung daemon, not a missing goroutine.
	listeners, err := Open([]config.Listener{
		{TCP: "127.0.0.1:0"},
		{TCP: "127.0.0.1:0"},
	}, discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer listeners.Close()

	if len(listeners.Net) != 2 || len(listeners.Descriptions) != 2 {
		t.Fatalf("opened %d listeners, %d descriptions", len(listeners.Net), len(listeners.Descriptions))
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	})}
	for _, ln := range listeners.Net {
		go srv.Serve(ln)
	}
	defer srv.Close()

	for _, ln := range listeners.Net {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			t.Fatalf("GET %s: %v", ln.Addr(), err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "ok" {
			t.Errorf("%s served %q", ln.Addr(), body)
		}
	}
}

func TestOpenIsAllOrNothing(t *testing.T) {
	// A partial bind would leave the daemon serving on loopback while the
	// listener the reviewers actually use silently does not exist, and nothing
	// in the logs would say so. The first listener here succeeds and the
	// second cannot, so the first must be closed again.
	first, err := Open([]config.Listener{{TCP: "127.0.0.1:0"}}, discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer first.Close()
	taken := first.Net[0].Addr().String()

	open, err := Open([]config.Listener{{TCP: "127.0.0.1:0"}, {TCP: taken}}, discard())
	if err == nil {
		open.Close()
		t.Fatal("Open succeeded with an address already in use")
	}
	if open != nil {
		t.Error("Open returned a listener set alongside an error")
	}
}

func TestOpenRejectsAnEmptyList(t *testing.T) {
	// Config normalization guarantees at least one listener, so reaching here
	// means that guarantee broke. Starting with nothing bound would be a
	// daemon that logs "listening" and answers nowhere.
	if _, err := Open(nil, discard()); err == nil {
		t.Fatal("Open accepted an empty listener list")
	}
}
