package expose

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openziti/sdk-golang/ziti"

	"github.com/netfoundry/docpreview/internal/config"
)

// These exercise the ziti exposer against a real OpenZiti network: a real
// controller, a real edge router, a real Bind policy, a real Dial policy, and a
// real overlay dial from a second identity.
//
// They skip unless pointed at identity files, so `go test ./...` on a machine
// with no network stays green. To run them, stand up a controller and set:
//
//	DOCPREVIEW_ZITI_HOST_IDENTITY=…/docpreview-host.json
//	DOCPREVIEW_ZITI_READER_IDENTITY=…/test-reader.json
//
// The scripts that produce those files are in the trial notes.
//
// What they prove that a unit test cannot: that Bind actually binds, that the
// Dial policy grants a differently-attributed identity access, and above all
// that the Host header survives the overlay — which is the single assumption
// the whole one-wildcard-service design rests on.

func zitiIdentities(t *testing.T) (host, reader string) {
	t.Helper()

	host = os.Getenv("DOCPREVIEW_ZITI_HOST_IDENTITY")
	reader = os.Getenv("DOCPREVIEW_ZITI_READER_IDENTITY")
	if host == "" || reader == "" {
		t.Skip("set DOCPREVIEW_ZITI_HOST_IDENTITY and DOCPREVIEW_ZITI_READER_IDENTITY to run")
	}
	for _, p := range []string{host, reader} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("identity file %s is not readable: %v", p, err)
		}
	}
	return host, reader
}

// overlayClient returns an http.Client whose transport dials the ziti service
// instead of the network, using the reader identity.
//
// This is what a tunneler does, minus the DNS and the TUN device: authorize,
// dial the service by name, speak HTTP over the resulting connection. If this
// works, a tunneler works.
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

// sharedZiti is one exposer for the whole package, created on first use.
//
// Not a convenience — a correctness requirement. Binding a ziti service creates
// a *terminator*; two bindings create two, and the router load-balances between
// them under the default smartrouting strategy. A test that opened its own
// exposer would find roughly half its requests answered by the previous test's
// still-closing instance, which presents as previews mysteriously 404ing.
//
// The same is true in production: exactly one docpreview may bind a given
// service. Two instances sharing one would split traffic between two disjoint
// routing tables, and every preview would work about half the time.
var (
	sharedZitiOnce sync.Once
	sharedZitiRef  *Ziti
	sharedZitiErr  error
)

func newZitiForTest(t *testing.T, hostIdentity string) *Ziti {
	t.Helper()

	sharedZitiOnce.Do(func() {
		z, err := NewZiti(config.ZitiConfig{
			IdentityFile: hostIdentity,
			Service:      "docpreview-svc",
			Domain:       "docpreview.ziti",
			NameTemplate: "{{.Name}}",
		}, discardLogger())
		if err != nil {
			sharedZitiErr = err
			return
		}
		if err := z.Validate(context.Background()); err != nil {
			sharedZitiErr = fmt.Errorf("Validate against the live controller: %w", err)
			return
		}
		sharedZitiRef = z
	})

	if sharedZitiErr != nil {
		t.Fatal(sharedZitiErr)
	}

	// Each test starts from an empty routing table so it is not reading another
	// test's previews.
	sharedZitiRef.mu.Lock()
	sharedZitiRef.live = map[string]*zitiPreview{}
	sharedZitiRef.mu.Unlock()

	return sharedZitiRef
}

func TestZitiEndToEndOverTheOverlay(t *testing.T) {
	hostIdentity, readerIdentity := zitiIdentities(t)

	z := newZitiForTest(t, hostIdentity)
	ctx := context.Background()

	pub, err := z.Publish(ctx,
		Spec{PreviewID: "p1", Name: "my-branch", BaseURL: "/"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "served over the overlay")
		}))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if pub.URL != "http://my-branch.docpreview.ziti/" {
		t.Errorf("URL = %q", pub.URL)
	}

	client := overlayClient(t, readerIdentity, "docpreview-svc")

	req, _ := http.NewRequest(http.MethodGet, "http://my-branch.docpreview.ziti/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("dialing the preview over the overlay: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}
	if string(body) != "served over the overlay" {
		t.Errorf("body = %q", body)
	}
}

func TestZitiRoutesByHostHeader(t *testing.T) {
	// The load-bearing assumption of the one-wildcard-service design: the
	// tunneler is a layer-4 proxy, so Host arrives verbatim and can select the
	// preview. If this fails, the design needs a service per pull request.
	hostIdentity, readerIdentity := zitiIdentities(t)

	z := newZitiForTest(t, hostIdentity)
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		body := "this is " + name
		if _, err := z.Publish(ctx,
			Spec{PreviewID: "p-" + name, Name: name, BaseURL: "/"},
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, body)
			})); err != nil {
			t.Fatalf("publishing %s: %v", name, err)
		}
	}

	client := overlayClient(t, readerIdentity, "docpreview-svc")

	for _, name := range []string{"alpha", "beta", "gamma"} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+name+".docpreview.ziti/", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if want := "this is " + name; string(body) != want {
			t.Errorf("Host %s.docpreview.ziti served %q, want %q — "+
				"one connection reached the wrong preview", name, body, want)
		}
	}
}

func TestZitiUnknownHostListsWhatIsLive(t *testing.T) {
	hostIdentity, readerIdentity := zitiIdentities(t)

	z := newZitiForTest(t, hostIdentity)

	if _, err := z.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "real-branch", BaseURL: "/"},
		http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}

	client := overlayClient(t, readerIdentity, "docpreview-svc")

	req, _ := http.NewRequest(http.MethodGet, "http://no-such-branch.docpreview.ziti/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET an unpublished host: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(string(body), "real-branch") {
		t.Errorf("the 404 does not list what is live: %q", body)
	}
}

func TestZitiWithdrawStopsServing(t *testing.T) {
	hostIdentity, readerIdentity := zitiIdentities(t)

	z := newZitiForTest(t, hostIdentity)

	pub, err := z.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "temporary", BaseURL: "/"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "still here")
		}))
	if err != nil {
		t.Fatal(err)
	}

	client := overlayClient(t, readerIdentity, "docpreview-svc")
	get := func() (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://temporary.docpreview.ziti/", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if code, body := get(); code != http.StatusOK || body != "still here" {
		t.Fatalf("before withdraw: %d %q", code, body)
	}

	if err := pub.Close(); err != nil {
		t.Fatal(err)
	}

	if code, _ := get(); code != http.StatusNotFound {
		t.Errorf("after withdraw: status = %d, want 404", code)
	}
}
