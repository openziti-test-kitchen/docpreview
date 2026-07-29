package expose

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// The ziti exposer routes by hostname label while the rest of docpreview
// identifies previews by ID. These cover the places those two namespaces can
// disagree — which is the whole hazard of a single shared service.
//
// They exercise the routing and lifecycle directly, with no overlay: the map,
// the collision check and the withdraw guard are all reachable without a
// controller. The end-to-end path is in ziti_integration_test.go.

func testZiti(t *testing.T) *Ziti {
	t.Helper()

	z, err := NewZiti(config.ZitiConfig{
		IdentityFile: "unused-in-these-tests.json",
		Service:      "docpreview-svc",
		Domain:       "docpreview.ziti",
		NameTemplate: "{{.Name}}",
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	// Publish refuses to run before Validate has bound the service, and binding
	// needs a controller. Setting the flag directly keeps these offline; the
	// end-to-end path is covered in ziti_integration_test.go.
	z.bound = true
	return z
}

func handler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	})
}

func serve(t *testing.T, z *Ziti, host string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	req.Host = host
	z.route(rec, req)
	return rec.Code, rec.Body.String()
}

func TestZitiRoutesEachLabelToItsOwnPreview(t *testing.T) {
	z := testZiti(t)
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta"} {
		if _, err := z.Publish(ctx,
			Spec{PreviewID: "p-" + name, Name: name, BaseURL: "/"},
			handler("this is "+name)); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"alpha", "beta"} {
		code, body := serve(t, z, name+".docpreview.ziti")
		if code != http.StatusOK || body != "this is "+name {
			t.Errorf("%s served %d %q", name, code, body)
		}
	}
}

func TestZitiRebuildReplacesItsOwnEntry(t *testing.T) {
	// The common case: the same preview publishing again after a new push.
	z := testZiti(t)
	ctx := context.Background()

	spec := Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"}

	if _, err := z.Publish(ctx, spec, handler("first build")); err != nil {
		t.Fatal(err)
	}
	if _, err := z.Publish(ctx, spec, handler("second build")); err != nil {
		t.Fatalf("republishing the same preview was refused: %v", err)
	}

	if _, body := serve(t, z, "my-branch.docpreview.ziti"); body != "second build" {
		t.Errorf("serving %q, want the rebuild", body)
	}
}

func TestZitiRefusesACollisionBetweenDifferentPreviews(t *testing.T) {
	// Two repositories, both with a branch called main, under the default
	// branch-only name template. Silently letting the second win would serve
	// one repository's documentation at the other's URL while both keep a
	// comment pointing there.
	z := testZiti(t)
	ctx := context.Background()

	if _, err := z.Publish(ctx,
		Spec{PreviewID: "repo-a-pr-1", Name: "main", BaseURL: "/"},
		handler("repo A")); err != nil {
		t.Fatal(err)
	}

	_, err := z.Publish(ctx,
		Spec{PreviewID: "repo-b-pr-1", Name: "main", BaseURL: "/"},
		handler("repo B"))
	if err == nil {
		t.Fatal("a second preview silently took over the hostname")
	}
	// The error has to name the fix, or it is just an obstruction.
	if !strings.Contains(err.Error(), "Repo.Name") {
		t.Errorf("the error does not suggest a name template: %v", err)
	}

	if _, body := serve(t, z, "main.docpreview.ziti"); body != "repo A" {
		t.Errorf("the incumbent was displaced: serving %q", body)
	}
}

func TestZitiStalePublicationCannotWithdrawItsSuccessor(t *testing.T) {
	// The subtler half. A preview is torn down *after* its label was legitimately
	// taken over by another one — which happens once the first is withdrawn and
	// a second claims the name. Closing the stale handle must not delete the
	// live route.
	z := testZiti(t)
	ctx := context.Background()

	first, err := z.Publish(ctx,
		Spec{PreviewID: "p1", Name: "shared", BaseURL: "/"},
		handler("first preview"))
	if err != nil {
		t.Fatal(err)
	}

	// The first goes away, freeing the label.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A different preview now takes it.
	if _, err := z.Publish(ctx,
		Spec{PreviewID: "p2", Name: "shared", BaseURL: "/"},
		handler("second preview")); err != nil {
		t.Fatal(err)
	}

	// The daemon still holds the first publication and closes it again on
	// teardown. Close is documented as safe to call twice.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	code, body := serve(t, z, "shared.docpreview.ziti")
	if code != http.StatusOK || body != "second preview" {
		t.Fatalf("a stale publication tore down the live route: %d %q", code, body)
	}
}

func TestZitiUnknownLabelListsWhatIsLive(t *testing.T) {
	z := testZiti(t)

	if _, err := z.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "real", BaseURL: "/"}, handler("x")); err != nil {
		t.Fatal(err)
	}

	code, body := serve(t, z, "missing.docpreview.ziti")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
	if !strings.Contains(body, "real") {
		t.Errorf("the 404 does not list live previews: %q", body)
	}
}

func TestZiti404EscapesTheHostHeader(t *testing.T) {
	// The 404 interpolates Host, which anyone on the overlay controls.
	z := testZiti(t)

	_, body := serve(t, z, "<script>alert(1)</script>.docpreview.ziti")
	if strings.Contains(body, "<script>") {
		t.Errorf("the Host header was not escaped: %q", body)
	}
}

func TestHostLabel(t *testing.T) {
	tests := map[string]string{
		"my-branch.docpreview.ziti":      "my-branch",
		"my-branch.docpreview.ziti:8080": "my-branch",
		"MY-BRANCH.docpreview.ziti":      "my-branch",
		"docpreview.ziti":                "docpreview",
		"bare":                           "bare",

		// An IP literal has no DNS label. Whatever comes back cannot name a
		// preview, so the request 404s — which is the right answer for a
		// connection that reached the service by address rather than by name.
		"[::1]:8080": "::1",
	}
	for host, want := range tests {
		if got := hostLabel(host); got != want {
			t.Errorf("hostLabel(%q) = %q, want %q", host, got, want)
		}
	}
}
