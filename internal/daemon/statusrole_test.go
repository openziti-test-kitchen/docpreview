package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every payload the dashboard reads carries the session role, not just one of them.
//
// The page takes its state from the `/events` stream; `/status` is a fallback it polls only when
// the stream is unavailable. So a field added to `/status` alone is a field the page never sees —
// which is what happened here: after a correct login the sign-out control stayed hidden, because
// the only payload carrying the role was the one nobody reads.
//
// Asserted on both, together, so adding a third surface has an obvious pattern to follow and
// removing it from either is a failure rather than a silent regression.
func TestBothStatusPayloadsCarryTheSessionRole(t *testing.T) {
	ingress, d, _ := testIngress(t, &fakeClient{})
	if _, err := ingress.WithLogin(d.store); err != nil {
		t.Fatal(err)
	}
	if err := SetConsolePassword(context.Background(), d.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}

	token := ingress.console.issue(RoleViewer, time.Now())
	h := ingress.Handler()

	t.Run("status", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/status", nil)
		r.RemoteAddr = "203.0.113.9:44444"
		r.AddCookie(&http.Cookie{Name: consoleCookie, Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("GET /status = %d", w.Code)
		}
		var st struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if st.Role != string(RoleViewer) {
			t.Errorf(`/status reported role %q, want "viewer"`, st.Role)
		}
	})

	t.Run("events", func(t *testing.T) {
		// The stream never returns on its own, so it is cancelled once a payload has been
		// written. What matters is the first `status` event, which is what the page applies on
		// connect.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		r := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
		r.RemoteAddr = "203.0.113.9:44444"
		r.AddCookie(&http.Cookie{Name: consoleCookie, Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		body := w.Body.String()
		if !strings.Contains(body, `"role":"viewer"`) {
			t.Errorf("the event stream carried no viewer role.\n%s", firstLines(body, 6))
		}
	})
}

// An unauthenticated request reports no role, so the page offers no sign-out to somebody who
// never signed in.
func TestNoSessionMeansNoRoleOnTheStatusPayload(t *testing.T) {
	ingress, d, _ := testIngress(t, &fakeClient{})
	if _, err := ingress.WithLogin(d.store); err != nil {
		t.Fatal(err)
	}

	// No password set at all, so the request is served without a login — and must still report
	// an empty role rather than inventing one.
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	ingress.Handler().ServeHTTP(w, r)

	var st struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Role != "" {
		t.Errorf("an unauthenticated request reported role %q", st.Role)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
