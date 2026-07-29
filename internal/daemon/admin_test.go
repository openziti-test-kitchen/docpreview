package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The admin links on the dashboard are drawn from this endpoint, so it has to
// answer the same way the write endpoints will. A page that offers Secrets to a
// caller the daemon will refuse produces a 403 in a dialog with no explanation.
func TestAdminStateIsFalseWhenForwarded(t *testing.T) {
	i, _, _ := testIngress(t, &fakeClient{})
	h := i.Handler()

	// The tunnel case. RemoteAddr is loopback because the daemon sees the local
	// tunnel process, which is why the forwarding header is the test that matters.
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"} {
		r := httptest.NewRequest("GET", "/api/admin", nil)
		r.RemoteAddr = "127.0.0.1:54321"
		r.Header.Set(header, "203.0.113.7")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		var got adminState
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v", header, err)
		}
		if got.Secrets || got.Projects {
			t.Errorf("%s: state = %+v, want both false for a forwarded request", header, got)
		}
		if got.Why == "" {
			t.Errorf("%s: no reason given; the page has nothing to explain itself with", header)
		}
	}
}

func TestAdminStateIsFalseFromARemoteAddress(t *testing.T) {
	i, _, _ := testIngress(t, &fakeClient{})
	h := i.Handler()

	r := httptest.NewRequest("GET", "/api/admin", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the page needs an answer, not an error", rec.Code)
	}
	var got adminState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Secrets || got.Projects {
		t.Errorf("state = %+v, want both false", got)
	}
}

// A local request still reports false for a surface that was never wired: from the
// page's side "not configured" and "not allowed" are the same fact, and drawing a
// link to a 404 is the same mistake as drawing one to a 403.
func TestAdminStateIsFalseForUnwiredSurfaces(t *testing.T) {
	i, _, _ := testIngress(t, &fakeClient{})
	h := i.Handler()

	r := httptest.NewRequest("GET", "/api/admin", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	var got adminState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Secrets {
		t.Error("secrets reported available with no SecretsAdmin wired")
	}
	if got.Projects {
		t.Error("projects reported available with no ProjectsAdmin wired")
	}
}
