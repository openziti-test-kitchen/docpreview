package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /readyz answers without a login, because the thing that waits on a restart is a script.
//
// If it were gated, `Restart-Docpreview.ps1` would poll a redirect to the login form until it
// times out, since `/status` — the only payload reporting recovery — sits behind the login with
// everything else. Gating /readyz turns a restart into a wait loop that never returns.
func TestReadyzAnswersWithoutALogin(t *testing.T) {
	ingress, d, _ := testIngress(t, &fakeClient{})
	if _, err := ingress.WithLogin(d.store); err != nil {
		t.Fatal(err)
	}
	if err := SetConsolePassword(context.Background(), d.store, RoleViewer, "a-viewer-password"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	r.RemoteAddr = "203.0.113.9:44444"
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	ingress.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /readyz = %d with a viewer password set, want 200", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("the body was not JSON: %v\n%s", err, w.Body.String())
	}
	for _, k := range []string{"starting", "pending", "running", "ready", "instance"} {
		if _, ok := got[k]; !ok {
			t.Errorf("/readyz reported no %q; the wait script reads it", k)
		}
	}
}

// /readyz must not name anything. It is the one payload an unauthenticated caller can read, and
// what it must never answer is "which repositories does this installation build".
//
// The keys are checked rather than the values, because an empty daemon has no previews to leak and
// a test that only looked at the rendered body would pass on a payload that leaks as soon as
// there is one thing to leak.
func TestReadyzNamesNothing(t *testing.T) {
	ingress, d, _ := testIngress(t, &fakeClient{})
	if _, err := ingress.WithLogin(d.store); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	ingress.Handler().ServeHTTP(w, r)

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// Every one of these is on /status and none of them belongs here.
	for _, k := range []string{"previews", "events", "projects", "last_startup", "exposer", "role"} {
		if _, ok := got[k]; ok {
			t.Errorf("/readyz reported %q, which identifies what this installation builds", k)
		}
	}

	// The stage's Items read "adopted a-customer-connect-docs-feature-…", which is a repository
	// and a branch. readyStage is a separate type for exactly this reason, so the assertion is on
	// the shape of the field rather than on a body that happens to be empty.
	if body := w.Body.String(); strings.Contains(body, `"items"`) {
		t.Errorf("/readyz carried the startup items, which name previews:\n%s", body)
	}
}
