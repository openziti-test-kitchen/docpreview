package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/docpreview/internal/config"
)

// fakeEdgeConn stands in for an accepted overlay connection.
//
// Only GetDialerIdentityId is read by the code under test, but `dialerOf` asserts against the whole
// edge.ServiceConn interface, so a stub missing one method is not an overlay connection at all: every
// test would then pass through the "no dialing identity" branch and agree with itself about the wrong
// thing. A net.Pipe end supplies the net.Conn half.
type fakeEdgeConn struct {
	net.Conn
	id   string
	name string
}

func (c fakeEdgeConn) CloseWrite() error        { return c.Conn.Close() }
func (c fakeEdgeConn) IsClosed() bool           { return false }
func (c fakeEdgeConn) GetAppData() []byte       { return nil }
func (c fakeEdgeConn) SourceIdentifier() string { return c.name }
func (c fakeEdgeConn) TraceRoute(uint32, time.Duration) (*edge.TraceRouteResult, error) {
	return nil, errors.New("not supported on a fake conn")
}
func (c fakeEdgeConn) GetCircuitId() string          { return "fake-circuit" }
func (c fakeEdgeConn) GetStickinessToken() []byte    { return nil }
func (c fakeEdgeConn) GetDialerIdentityId() string   { return c.id }
func (c fakeEdgeConn) GetDialerIdentityName() string { return c.name }

// overlayRequest builds a request as if it had arrived on an overlay listener whose admin
// list holds `admins`, dialed by `id`.
//
// It goes through the real accept path — overlayListener.Accept, then ConnContext — rather
// than constructing the context value directly. The value is what the gate reads, so
// building it by hand would test the gate against a fixture instead of against the code that
// produces it, and the interesting bugs are all in that production.
func overlayRequest(t *testing.T, id string, admins []string) *http.Request {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	ln := &overlayListener{
		Listener: oneShotListener{conn: fakeEdgeConn{Conn: server, id: id, name: id}},
		admins:   adminSet(admins),
	}
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPut, "/api/secrets/github.app_id", nil)
	// An overlay connection's RemoteAddr is not loopback, which is the whole reason the
	// identity check has to come first.
	r.RemoteAddr = "100.64.0.7:39000"
	return r.WithContext(ConnContext(context.Background(), c))
}

// oneShotListener hands out one connection and then blocks forever, which is enough for a
// test that only ever accepts once.
type oneShotListener struct {
	conn net.Conn
	done bool
}

func (l oneShotListener) Accept() (net.Conn, error) { return l.conn, nil }
func (l oneShotListener) Close() error              { return nil }
func (l oneShotListener) Addr() net.Addr            { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "ziti" }
func (dummyAddr) String() string  { return "overlay" }

// An identity named in admin_identities may write.
//
// This is the grant the whole change exists for: before it, any ziti listener made both admin
// surfaces read-only, so a daemon reachable only over the overlay could never be administered
// from it.
func TestANamedOverlayIdentityMayWrite(t *testing.T) {
	r := overlayRequest(t, "abc123", []string{"abc123", "someone-else"})
	if ok, why := isLocalRequest(r); !ok {
		t.Errorf("a named admin identity was refused: %s", why)
	}
}

// Everything else is refused, and each of these is a separate way to get it wrong.
func TestOverlayWritesFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		admins  []string
		wantWhy string
	}{
		{
			// The default. A listener that names nobody is read-only, so this does not widen access for
			// an installation that has not opted in.
			name: "no admin_identities",
			id:   "abc123",
		},
		{
			// An enrolled identity that is not on the list. "Enrolled at all" was never
			// meant to be the authorization.
			name:   "an identity not on the list",
			id:     "stranger",
			admins: []string{"abc123"},
		},
		{
			// The router sent no dialer, or something that is not an overlay connection
			// reached this path. Not knowing who somebody is cannot be a grant — this is
			// the case that would be an open credential API if it defaulted the other way.
			name:   "no dialing identity",
			id:     "",
			admins: []string{"abc123"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := overlayRequest(t, tt.id, tt.admins)
			ok, why := isLocalRequest(r)
			if ok {
				t.Fatalf("%s was allowed to write", tt.name)
			}
			if why == "" {
				t.Error("the refusal gives no reason, so nobody can fix it")
			}
		})
	}
}

// A plain TCP request is unaffected: the loopback rule still decides it.
//
// The overlay check runs first and must not swallow the ordinary case — this is the test that
// fails if ConnContext ever stamps a value for a non-overlay connection.
func TestATCPRequestStillUsesTheLoopbackRule(t *testing.T) {
	local := httptest.NewRequest(http.MethodPut, "/api/secrets/x", nil)
	local.RemoteAddr = "127.0.0.1:5000"
	if ok, why := isLocalRequest(local); !ok {
		t.Errorf("a loopback request was refused: %s", why)
	}

	remote := httptest.NewRequest(http.MethodPut, "/api/secrets/x", nil)
	remote.RemoteAddr = "10.0.0.5:5000"
	if ok, _ := isLocalRequest(remote); ok {
		t.Error("a request from off-machine was allowed")
	}

	// And a forwarded loopback request is still refused, which is the tunnel hole the
	// header check exists to close.
	forwarded := httptest.NewRequest(http.MethodPut, "/api/secrets/x", nil)
	forwarded.RemoteAddr = "127.0.0.1:5000"
	forwarded.Header.Set("X-Forwarded-For", "203.0.113.9")
	if ok, _ := isLocalRequest(forwarded); ok {
		t.Error("a forwarded request was allowed")
	}
}

// The listener half of the gate, which is a property of the daemon rather than of a request.
func TestListenersAllowAdmin(t *testing.T) {
	tests := []struct {
		name      string
		listeners []config.Listener
		want      bool
	}{
		{"loopback", []config.Listener{{TCP: "127.0.0.1:8471"}}, true},
		{"off-machine", []config.Listener{{TCP: "0.0.0.0:8471"}}, false},
		{"none", nil, false},
		{
			// The opt-in. A ziti listener naming identities is allowed at this level; which
			// identity dialed is then the request's business.
			name: "ziti with admin identities",
			listeners: []config.Listener{{Ziti: &config.ZitiListener{
				Service: "docpreview-admin", AdminIdentities: []string{"abc123"}}}},
			want: true,
		},
		{
			name: "ziti naming nobody",
			listeners: []config.Listener{{Ziti: &config.ZitiListener{
				Service: "docpreview-admin"}}},
			want: false,
		},
		{
			// A mixed config is judged on its weakest listener, because one http.Server
			// serves them all: allowing writes because *a* listener is loopback would allow
			// them through the one that is not.
			name: "loopback plus a ziti listener naming nobody",
			listeners: []config.Listener{
				{TCP: "127.0.0.1:8471"},
				{Ziti: &config.ZitiListener{Service: "docpreview-admin"}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := listenersAllowAdmin(tt.listeners, "projects")
			if got != tt.want {
				t.Errorf("listenersAllowAdmin = %v (%s), want %v", got, why, tt.want)
			}
			if !got && why == "" {
				t.Error("refused with no reason")
			}
		})
	}
}

// The context key must not be forgeable from outside this package.
//
// It is an unexported empty struct for that reason: a handler or middleware elsewhere writing
// a string key called "dialer" must not be able to grant itself the admin surface. Asserted
// by writing exactly that and checking the gate ignores it.
func TestAForgedContextValueGrantsNothing(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/api/secrets/x", nil)
	r.RemoteAddr = "100.64.0.7:39000"
	//nolint:staticcheck // a string key is the forgery being tested
	ctx := context.WithValue(context.Background(), "dialer", "abc123")
	if ok, _ := isLocalRequest(r.WithContext(ctx)); ok {
		t.Error("a string-keyed context value was accepted as a dialing identity")
	}
}
