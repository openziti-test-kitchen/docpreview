package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openziti/sdk-golang/ziti"

	"github.com/netfoundry/docpreview/internal/config"
)

// This proves the admin surface — the dashboard and the webhook endpoint — is
// reachable over an OpenZiti overlay and not merely bindable.
//
// It skips unless pointed at a live network, so `go test ./...` on a machine
// with no controller stays green:
//
//	DOCPREVIEW_ZITI_HOST_IDENTITY=…/docpreview-host.json
//	DOCPREVIEW_ZITI_READER_IDENTITY=…/reviewer.json
//	DOCPREVIEW_ZITI_ADMIN_SERVICE=docpreview-admin
//
// `docpreview configure ziti` creates all three.
//
// Unlike the exposer's integration tests there is no shared listener here.
// Each test binds the admin service for its own duration and closes it, which
// is safe only because they run sequentially — two simultaneous binds create
// two terminators and the router load-balances between them, so half the
// requests would reach a listener that is not serving this test's handler.
func adminOverlay(t *testing.T) (hostIdentity, readerIdentity, service string) {
	t.Helper()

	hostIdentity = os.Getenv("DOCPREVIEW_ZITI_HOST_IDENTITY")
	readerIdentity = os.Getenv("DOCPREVIEW_ZITI_READER_IDENTITY")
	service = os.Getenv("DOCPREVIEW_ZITI_ADMIN_SERVICE")
	if hostIdentity == "" || readerIdentity == "" || service == "" {
		t.Skip("set DOCPREVIEW_ZITI_HOST_IDENTITY, DOCPREVIEW_ZITI_READER_IDENTITY " +
			"and DOCPREVIEW_ZITI_ADMIN_SERVICE to run")
	}
	for _, p := range []string{hostIdentity, readerIdentity} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("identity file %s is not readable: %v", p, err)
		}
	}
	return hostIdentity, readerIdentity, service
}

// overlayClient dials the service by name instead of resolving a hostname,
// which is what a tunneler does minus the DNS and the TUN device.
func overlayClient(t *testing.T, readerIdentity, service string) *http.Client {
	t.Helper()

	zctx, err := ziti.NewContextFromFile(readerIdentity)
	if err != nil {
		t.Fatalf("loading the reader identity: %v", err)
	}
	t.Cleanup(zctx.Close)

	if err := zctx.Authenticate(); err != nil {
		t.Fatalf("authenticating the reader identity: %v", err)
	}

	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return zctx.Dial(service)
			},
		},
	}
}

func TestIngressReachableOverTheOverlay(t *testing.T) {
	hostIdentity, readerIdentity, service := adminOverlay(t)

	ingress, _, _ := testIngress(t, &fakeClient{})

	listeners, err := Open([]config.Listener{{Ziti: &config.ZitiListener{
		IdentityFile: hostIdentity,
		Service:      service,
	}}}, discard())
	if err != nil {
		t.Fatalf("binding the admin service: %v", err)
	}
	defer listeners.Close()

	srv := &http.Server{Handler: ingress.Handler()}
	go srv.Serve(listeners.Net[0])
	t.Cleanup(func() { srv.Close() })

	client := overlayClient(t, readerIdentity, service)

	// The dashboard, which is the sensitive one: it enumerates every open
	// documentation pull request, and this is the whole reason for putting the
	// ingress on the overlay.
	resp, err := client.Get("http://docpreview.invalid/")
	if err != nil {
		t.Fatalf("GET the dashboard over the overlay: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "<!doctype html") {
		t.Errorf("the dashboard did not come back as HTML: %q", firstBytes(body))
	}

	// /status is what the dashboard polls. Serving the page but not the data
	// it needs would look like a working overlay and a broken daemon.
	resp, err = client.Get("http://docpreview.invalid/status")
	if err != nil {
		t.Fatalf("GET /status over the overlay: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status status = %d, body = %q", resp.StatusCode, body)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		t.Errorf("/status did not return JSON: %q", firstBytes(body))
	}
}

func TestOverlayIngressStopsWhenClosed(t *testing.T) {
	// Shutdown has to actually release the overlay listener. A terminator left
	// pointing at a dead process is worse than no terminator: the controller
	// keeps routing to it until it times out, so requests fail rather than
	// falling through to whatever replaced it.
	hostIdentity, readerIdentity, service := adminOverlay(t)

	ingress, _, _ := testIngress(t, &fakeClient{})

	listeners, err := Open([]config.Listener{{Ziti: &config.ZitiListener{
		IdentityFile: hostIdentity,
		Service:      service,
	}}}, discard())
	if err != nil {
		t.Fatalf("binding the admin service: %v", err)
	}

	srv := &http.Server{Handler: ingress.Handler()}
	go srv.Serve(listeners.Net[0])

	client := overlayClient(t, readerIdentity, service)
	resp, err := client.Get("http://docpreview.invalid/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over the overlay: %v", err)
	}
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Shutdown already closed the listener; Close must not report that as a
	// failure, or every clean exit would log an error.
	if err := listeners.Close(); err != nil {
		t.Fatalf("Close after Shutdown: %v", err)
	}

	if resp, err := client.Get("http://docpreview.invalid/healthz"); err == nil {
		resp.Body.Close()
		t.Error("the service still answers after shutdown")
	}
}

func firstBytes(b []byte) string {
	if len(b) > 120 {
		return string(b[:120]) + "…"
	}
	return string(b)
}
