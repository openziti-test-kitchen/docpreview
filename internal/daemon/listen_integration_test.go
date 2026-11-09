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
//	NFPREVIEW_ZITI_HOST_IDENTITY=…/nfpreview-host.json
//	NFPREVIEW_ZITI_READER_IDENTITY=…/reviewer.json
//	NFPREVIEW_ZITI_ADMIN_SERVICE=nfpreview-admin
//
// `nfpreview configure ziti` creates all three.
//
// Unlike the exposer's integration tests there is no shared listener here.
// Each test binds the admin service for its own duration and closes it, which
// is safe only because they run sequentially — two simultaneous binds create
// two terminators and the router load-balances between them, so half the
// requests would reach a listener that is not serving this test's handler.
func adminOverlay(t *testing.T) (hostIdentity, readerIdentity, service string) {
	t.Helper()

	hostIdentity = os.Getenv("NFPREVIEW_ZITI_HOST_IDENTITY")
	readerIdentity = os.Getenv("NFPREVIEW_ZITI_READER_IDENTITY")
	service = os.Getenv("NFPREVIEW_ZITI_ADMIN_SERVICE")
	if hostIdentity == "" || readerIdentity == "" || service == "" {
		t.Skip("set NFPREVIEW_ZITI_HOST_IDENTITY, NFPREVIEW_ZITI_READER_IDENTITY " +
			"and NFPREVIEW_ZITI_ADMIN_SERVICE to run")
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
	resp, err := client.Get("http://nfpreview.invalid/")
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
	resp, err = client.Get("http://nfpreview.invalid/status")
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
	resp, err := client.Get("http://nfpreview.invalid/healthz")
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

	if resp, err := client.Get("http://nfpreview.invalid/healthz"); err == nil {
		resp.Body.Close()
		t.Error("the service still answers after shutdown")
	}
}

// TestOverlayAdminGrantFollowsTheRealDialer is the live half of the admin_identities tests.
// The offline ones hand the listener a fake conn, so they cannot show that a real router
// sends the dialer, or that the id it sends is the one the controller lists. This one
// grants the reader identity, then dials as it and as a second, unlisted identity:
//
//	NFPREVIEW_ZITI_OTHER_IDENTITY=…/other.json  (enrolled, may Dial the service)
//
// "No id at all" stays an offline test: a real overlay always names its dialer.
func TestOverlayAdminGrantFollowsTheRealDialer(t *testing.T) {
	hostIdentity, readerIdentity, service := adminOverlay(t)
	otherIdentity := os.Getenv("NFPREVIEW_ZITI_OTHER_IDENTITY")
	if otherIdentity == "" {
		t.Skip("set NFPREVIEW_ZITI_OTHER_IDENTITY to an enrolled identity not in admin_identities")
	}

	// The id as the controller reports it to the identity itself. A grant written
	// from `ziti edge list identities` holds this value.
	idOf := func(file string) string {
		zctx, err := ziti.NewContextFromFile(file)
		if err != nil {
			t.Fatalf("loading %s: %v", file, err)
		}
		defer zctx.Close()
		if err := zctx.Authenticate(); err != nil {
			t.Fatalf("authenticating %s: %v", file, err)
		}
		me, err := zctx.GetCurrentIdentity()
		if err != nil {
			t.Fatalf("asking the controller who %s is: %v", file, err)
		}
		return *me.ID
	}
	adminID, otherID := idOf(readerIdentity), idOf(otherIdentity)

	listeners, err := Open([]config.Listener{{Ziti: &config.ZitiListener{
		IdentityFile:    hostIdentity,
		Service:         service,
		AdminIdentities: []string{adminID},
	}}}, discard())
	if err != nil {
		t.Fatalf("binding the admin service: %v", err)
	}
	defer listeners.Close()

	srv := &http.Server{
		ConnContext: ConnContext,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller, _ := overlayCallerFrom(r.Context())
			ok, why := isLocalRequest(r)
			w.Header().Set("X-Dialer", caller.Dialer)
			if !ok {
				http.Error(w, why, http.StatusForbidden)
				return
			}
			io.WriteString(w, "may write")
		}),
	}
	go srv.Serve(listeners.Net[0])
	t.Cleanup(func() { srv.Close() })

	ask := func(identity string) (int, string, string) {
		resp, err := overlayClient(t, identity, service).Get("http://nfpreview.invalid/")
		if err != nil {
			t.Fatalf("GET over the overlay as %s: %v", identity, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("X-Dialer"), string(body)
	}

	code, dialer, body := ask(readerIdentity)
	if dialer != adminID {
		t.Errorf("the listed identity arrived as %q, but the controller calls it %q", dialer, adminID)
	}
	if code != http.StatusOK {
		t.Errorf("the listed identity was refused: %d %q", code, body)
	}

	code, dialer, body = ask(otherIdentity)
	if dialer != otherID {
		t.Errorf("the unlisted identity arrived as %q, but the controller calls it %q", dialer, otherID)
	}
	if code != http.StatusForbidden {
		t.Errorf("the unlisted identity was not refused: %d %q", code, body)
	}
	if !strings.Contains(body, otherID) {
		t.Errorf("the refusal does not name the identity to add: %q", body)
	}
}

func firstBytes(b []byte) string {
	if len(b) > 120 {
		return string(b[:120]) + "…"
	}
	return string(b)
}
