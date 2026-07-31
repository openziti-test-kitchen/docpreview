package expose

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openziti/sdk-golang/ziti/edge"

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

// The dialing-identity plumbing.
//
// The identity comes off the accepted connection, so the only thing an offline
// test can honestly cover is the wiring: that ConnContext puts the value where
// a handler can read it, and that a connection which cannot answer the question
// yields empty rather than panicking. Whether the *router* populates the header
// the SDK reads it from needs an overlay — see ziti_integration_test.go.

// fakeServiceConn is an edge.ServiceConn over an in-memory pipe. edge.Conn's
// two identity methods live on the narrower edge.ServiceConn interface, which
// is why zitiConnContext asserts that one and why this stub can exist at all
// without reimplementing a circuit.
type fakeServiceConn struct {
	net.Conn // the pipe end, so the HTTP server has something to read and write

	id   string
	name string
}

func (c *fakeServiceConn) CloseWrite() error        { return c.Conn.Close() }
func (c *fakeServiceConn) IsClosed() bool           { return false }
func (c *fakeServiceConn) GetAppData() []byte       { return nil }
func (c *fakeServiceConn) SourceIdentifier() string { return c.name }
func (c *fakeServiceConn) TraceRoute(uint32, time.Duration) (*edge.TraceRouteResult, error) {
	return nil, errors.New("not supported on a fake conn")
}
func (c *fakeServiceConn) GetCircuitId() string        { return "fake-circuit" }
func (c *fakeServiceConn) GetStickinessToken() []byte  { return nil }
func (c *fakeServiceConn) GetDialerIdentityId() string { return c.id }
func (c *fakeServiceConn) GetDialerIdentityName() string {
	return c.name
}

// oneConnListener hands an http.Server exactly one connection and then blocks
// until closed, which is enough to drive a real Serve loop — and driving the
// real loop is the point: it proves http.Server actually calls ConnContext with
// the conn the listener returned, rather than proving that a function we call
// ourselves does what it says.
type oneConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, errors.New("listener closed")
}

func (l *oneConnListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return dummyAddr("ziti") }

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }

// serveOverConn runs one HTTP request over conn against a server wired exactly
// as Validate wires it, and returns whatever the handler saw.
func serveOverConn(t *testing.T, server net.Conn, client net.Conn, h http.Handler) {
	t.Helper()

	l := &oneConnListener{conn: server, done: make(chan struct{})}
	srv := &http.Server{Handler: h, ConnContext: zitiConnContext}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(l)
	}()
	t.Cleanup(func() {
		_ = l.Close()
		_ = srv.Close()
		<-served
	})

	if _, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: alpha.docpreview.ziti\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// Reading the response is what guarantees the handler ran before the test
	// asserts on what it recorded.
	if _, err := bufio.NewReader(client).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
}

func TestZitiConnContextCarriesTheDialingIdentityToTheHandler(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	conn := &fakeServiceConn{Conn: serverEnd, id: "abc123", name: "reviewer-bob"}

	var got zitiDialer
	serveOverConn(t, conn, clientEnd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = zitiDialerFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	if got.id != "abc123" || got.name != "reviewer-bob" {
		t.Errorf("the handler saw dialer %+v, want id abc123 name reviewer-bob", got)
	}
}

func TestZitiConnContextMustNotPanicOnAConnWithoutAnIdentity(t *testing.T) {
	// A plain TCP conn is what every test provides and what any non-overlay
	// listener would provide. The type assertion in zitiConnContext is the one
	// line most likely to be written unchecked, and unchecked it turns an
	// unauthenticated request into a dead daemon.
	serverEnd, clientEnd := net.Pipe()

	var got zitiDialer
	var reached bool
	serveOverConn(t, serverEnd, clientEnd, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		got = zitiDialerFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	if !reached {
		t.Fatal("the handler never ran; a conn carrying no identity must still be served")
	}
	if got != (zitiDialer{}) {
		t.Errorf("a conn with no identity produced %+v, want the zero value", got)
	}
}

func TestZitiDialerFromAContextWithoutOneIsEmpty(t *testing.T) {
	// The direct form of the same guarantee, including a context carrying a
	// *different* type under nothing docpreview owns.
	if got := zitiDialerFrom(context.Background()); got != (zitiDialer{}) {
		t.Errorf("an empty context produced %+v", got)
	}
	//nolint:staticcheck // the point is a foreign value, so a string key is correct here
	ctx := context.WithValue(context.Background(), "dialer", "not-a-zitiDialer")
	if got := zitiDialerFrom(ctx); got != (zitiDialer{}) {
		t.Errorf("a foreign context value was mistaken for a dialer: %+v", got)
	}
}

func TestZitiLogsANewIdentityOncePerPreview(t *testing.T) {
	// A documentation page is on the order of a hundred asset requests. The Info
	// line must fire on the first and not on the rest, or the log is unreadable.
	z := testZiti(t)
	if _, err := z.Publish(context.Background(),
		Spec{PreviewID: "p1", Name: "alpha", BaseURL: "/"}, handler("x")); err != nil {
		t.Fatal(err)
	}

	z.mu.Lock()
	entry := z.live["alpha"]
	z.mu.Unlock()

	if !entry.noteDialer("abc123") {
		t.Fatal("the first sighting of an identity was not reported as new")
	}
	for i := 0; i < 10; i++ {
		if entry.noteDialer("abc123") {
			t.Fatalf("request %d reported the same identity as new again", i)
		}
	}
	if !entry.noteDialer("def456") {
		t.Error("a second identity was not reported as new")
	}

	// An unknown identity — every offline path, and the overlay too if the
	// header is ever absent — is still one distinct entry rather than a panic.
	if !entry.noteDialer("") {
		t.Error("an empty identity was not reported as new")
	}
	if entry.noteDialer("") {
		t.Error("an empty identity was reported as new twice")
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
