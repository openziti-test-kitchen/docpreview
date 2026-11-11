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

// overlayRequest builds a request as if it had arrived on an overlay listener, dialed by `id`.
//
// It goes through the real accept path — overlayListener.Accept, then ConnContext — rather
// than constructing the context value directly. The value is what privatePath reads, so
// building it by hand would test against a fixture instead of against the code that produces it.
func overlayRequest(t *testing.T, id string) *http.Request {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	ln := &overlayListener{Listener: oneShotListener{conn: fakeEdgeConn{Conn: server, id: id, name: id}}}
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPut, "/api/secrets/github.app_id", nil)
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

// An overlay connection is a private path, so an admin session is honoured on it. It grants
// nothing without one.
func TestAnOverlayConnectionIsAPrivatePathButNotAGrant(t *testing.T) {
	for _, id := range []string{"abc123", ""} {
		r := overlayRequest(t, id)
		if !privatePath(r) {
			t.Errorf("dialer %q: an overlay connection was not a private path", id)
		}
		if ok, _ := isLocalRequest(r); ok {
			t.Errorf("dialer %q: an overlay connection with no session was allowed to write", id)
		}
	}
}

// Where a request came from never grants admin on its own.
func TestNoAddressGrantsAdminWithoutASession(t *testing.T) {
	for _, tc := range []struct{ name, remote, fwd string }{
		{"loopback", "127.0.0.1:5000", ""},
		{"off-machine", "10.0.0.5:5000", ""},
		{"loopback behind a proxy", "127.0.0.1:5000", "203.0.113.9"},
	} {
		r := httptest.NewRequest(http.MethodPut, "/api/secrets/x", nil)
		r.RemoteAddr = tc.remote
		if tc.fwd != "" {
			r.Header.Set("X-Forwarded-For", tc.fwd)
		}
		ok, why := isLocalRequest(r)
		if ok {
			t.Errorf("%s: a request with no session was allowed to write", tc.name)
		}
		if why == "" {
			t.Errorf("%s: the refusal gives no reason", tc.name)
		}
	}
}

// The listener half refuses only a daemon with no listeners.
func TestListenersAllowAdmin(t *testing.T) {
	for _, tt := range []struct {
		name      string
		listeners []config.Listener
		want      bool
	}{
		{"loopback", []config.Listener{{TCP: "127.0.0.1:8471"}}, true},
		{"off-machine", []config.Listener{{TCP: "0.0.0.0:8471"}}, true},
		{"ziti", []config.Listener{{Ziti: &config.ZitiListener{Service: "nfpreview-admin"}}}, true},
		{"none", nil, false},
	} {
		got, why := listenersAllowAdmin(tt.listeners, "projects")
		if got != tt.want {
			t.Errorf("%s: listenersAllowAdmin = %v (%s), want %v", tt.name, got, why, tt.want)
		}
		if !got && why == "" {
			t.Errorf("%s: refused with no reason", tt.name)
		}
	}
}

// The context key must not be forgeable from outside this package: a string key called "dialer"
// must not make a request from off-machine a private path.
func TestAForgedContextValueIsNotAPrivatePath(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/api/secrets/x", nil)
	r.RemoteAddr = "100.64.0.7:39000"
	//nolint:staticcheck // a string key is the forgery being tested
	ctx := context.WithValue(context.Background(), "dialer", "abc123")
	if privatePath(r.WithContext(ctx)) {
		t.Error("a string-keyed context value made an off-machine request a private path")
	}
}
