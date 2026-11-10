package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// zrokPrivateRequest builds a request as if it had arrived on a zrok private listener, through the
// real accept path.
func zrokPrivateRequest(t *testing.T, conn net.Conn) *http.Request {
	t.Helper()
	ln := &overlayListener{Listener: oneShotListener{conn: conn}, grantAll: true, label: "zrok private nfpadmin"}
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/api/secrets/github.app_id", nil)
	r.RemoteAddr = "100.64.0.7:39000"
	// zrok's access process proxies, so a forwarding header is the normal case here.
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	return r.WithContext(ConnContext(context.Background(), c))
}

func TestAZrokPrivateConnectionMayUseTheAdminSurfaces(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	// With a dialer the overlay reported, and without one: the share's closed permission mode is
	// the grant, so which environment dialed does not change the answer.
	for name, conn := range map[string]net.Conn{
		"with a dialer":  fakeEdgeConn{Conn: server, id: "env-zid", name: "env"},
		"with no dialer": server,
	} {
		t.Run(name, func(t *testing.T) {
			if ok, why := isLocalRequest(zrokPrivateRequest(t, conn)); !ok {
				t.Fatalf("isLocalRequest = false (%s), want true over a zrok private share", why)
			}
		})
	}
}

func TestAViewerSessionStaysReadOnlyOverAZrokPrivateShare(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	r := zrokPrivateRequest(t, server)
	r = r.WithContext(context.WithValue(r.Context(), roleKey{}, RoleViewer))
	if ok, _ := isLocalRequest(r); ok {
		t.Fatal("a viewer session over a zrok private share was treated as admin")
	}
}

func TestAZrokPrivateListenerKeepsTheAdminSurfacesAvailable(t *testing.T) {
	listeners := []config.Listener{
		{TCP: "127.0.0.1:8471"},
		{ZrokPrivate: &config.ZrokPrivateListener{Token: "nfpadmin"}},
	}
	if ok, why := listenersAllowAdmin(listeners, "secrets"); !ok {
		t.Fatalf("listenersAllowAdmin = false (%s), want true", why)
	}
}

func TestTheAdminShareIsNotTaggedAsAPreview(t *testing.T) {
	// Reap deletes every unclaimed share whose target starts with the preview tag.
	if strings.HasPrefix(zrokPrivateTarget, "nfpreview:") {
		t.Fatalf("zrokPrivateTarget %q carries the preview tag, so Reap would delete it", zrokPrivateTarget)
	}
}
