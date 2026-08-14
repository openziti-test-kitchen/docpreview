package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// A viewer may rebuild, and may not do anything else on the projects admin.
//
// Rebuilding re-runs the command the branch already runs on every push, so a reviewer who can see
// that a build failed can retry it. Cancelling stops somebody else's build, and unlinking removes a
// preview and retracts its comment — neither is undone by pressing the button again, and neither is
// a read.
//
// Asserted through the gates rather than the handlers: what is under test is who is let through,
// and the handlers behind these routes need a daemon, a store and an exposer to say anything.
func TestAViewerMayRebuildAndNothingElse(t *testing.T) {
	// A remote request, with a forwarding header, which is what arriving through the read-only
	// dashboard tunnel looks like. Without the role every gate here must refuse it.
	remote := func(role Role) *http.Request {
		r := httptest.NewRequest("POST", "/api/builds/abc123/rebuild", nil)
		r.RemoteAddr = "203.0.113.7:44321"
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		if role != RoleNone {
			r = r.WithContext(context.WithValue(r.Context(), roleKey{}, role))
		}
		return r
	}

	// A loopback listener, so the locality route is available in principle and the only thing
	// refusing the remote request is the request itself.
	cfg := config.Server{Listeners: []config.Listener{{TCP: "127.0.0.1:8471"}}}
	admin := &ProjectsAdmin{cfg: cfg, log: slog.New(slog.DiscardHandler)}

	reached := false
	next := func(http.ResponseWriter, *http.Request) { reached = true }

	for _, tc := range []struct {
		name  string
		role  Role
		gate  func(http.HandlerFunc) http.HandlerFunc
		allow bool
	}{
		{"a viewer rebuilding", RoleViewer, admin.rebuildGated, true},
		{"an admin rebuilding", RoleAdmin, admin.rebuildGated, true},
		{"nobody rebuilding, from off-box", RoleNone, admin.rebuildGated, false},
		{"a viewer cancelling", RoleViewer, admin.gated, false},
		{"a viewer unlinking", RoleViewer, admin.gated, false},
		{"an admin cancelling", RoleAdmin, admin.gated, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			w := httptest.NewRecorder()
			tc.gate(next)(w, remote(tc.role))

			if reached != tc.allow {
				got, want := "refused", "allowed"
				if reached {
					got = "allowed"
				}
				if !tc.allow {
					want = "refused"
				}
				t.Errorf("%s was %s, want %s (status %d)", tc.name, got, want, w.Code)
			}
		})
	}
}
