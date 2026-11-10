package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// An admin session shows the admin links on any listener it reaches, as the write gates already allow.
//
// The case that used to fail: a ziti listener naming no admin_identities makes the listener check
// say no, so a signed-in admin got no Projects or Secrets link while every write would have worked.
func TestAnAdminSessionSeesTheAdminLinksOnAReadOnlyZitiListener(t *testing.T) {
	cfg := config.Server{Listeners: []config.Listener{
		{Ziti: &config.ZitiListener{IdentityFile: "id.json", Service: "nfpreview-admin"}},
	}}
	i := &Ingress{secrets: &SecretsAdmin{cfg: cfg}, projects: &ProjectsAdmin{cfg: cfg}}

	r := httptest.NewRequest(http.MethodGet, "/api/admin", nil)
	r.RemoteAddr = "100.64.0.7:39000"

	for _, tt := range []struct {
		role Role
		want bool
	}{{RoleAdmin, true}, {RoleViewer, false}, {RoleNone, false}} {
		rec := httptest.NewRecorder()
		i.admin(rec, r.WithContext(context.WithValue(context.Background(), roleKey{}, tt.role)))

		var got adminState
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Secrets != tt.want || got.Projects != tt.want {
			t.Errorf("role %q: admin state = %+v, want secrets and projects %v", tt.role, got, tt.want)
		}
	}
}
