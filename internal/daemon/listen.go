package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/docpreview/internal/config"
)

// Listeners is the set of places the ingress accepts connections, opened.
//
// It exists because a ziti listener owns more than a net.Listener: closing the
// listener leaves the enrolled identity's controller session open, and a
// process that does that on shutdown leaves a terminator behind for the
// controller to time out. Keeping the contexts beside the listeners is what
// makes Close complete rather than approximate.
type Listeners struct {
	// Net is what http.Server.Serve is called with, one goroutine each.
	Net []net.Listener

	// Descriptions parallels Net, for logging and `nfpreview doctor`. Held
	// separately because a ziti listener's Addr() is an overlay address that
	// says nothing about which service was bound.
	Descriptions []string

	contexts []ziti.Context
}

// Open binds every configured listener, or none of them.
//
// All-or-nothing on purpose. A partial bind would leave nfpreview serving the
// dashboard on loopback while the overlay listener — the one the reviewers
// use — silently does not exist, and the daemon would look healthy. Any
// failure closes what was already opened and reports which entry failed.
func Open(listeners []config.Listener, log *slog.Logger) (*Listeners, error) {
	if len(listeners) == 0 {
		return nil, errors.New("no listeners are configured")
	}

	open := &Listeners{}
	for i, l := range listeners {
		var (
			ln  net.Listener
			err error
		)
		switch {
		case l.Ziti != nil:
			ln, err = open.openZiti(*l.Ziti, log)
		case l.ZrokPrivate != nil:
			ln, err = open.openZrokPrivate(*l.ZrokPrivate, log)
		default:
			ln, err = net.Listen("tcp", l.TCP)
		}
		if err != nil {
			open.Close()
			return nil, fmt.Errorf("listeners[%d] (%s): %w", i, l.Describe(), err)
		}

		open.Net = append(open.Net, ln)
		open.Descriptions = append(open.Descriptions, l.Describe())
		log.Info("ingress listening", "listener", l.Describe())
	}
	return open, nil
}

// openZiti loads an identity and binds a service.
//
// Same shape as the ziti exposer's Validate, and the same operational
// constraint applies: exactly one process may bind a given service. A second
// binding creates a second terminator and the router load-balances between
// them, so half the dashboard requests would reach a nfpreview that knows
// nothing about them. The error text names the Bind policy because a missing
// one is the overwhelmingly common cause and the SDK's own message does not
// mention policies at all.
func (ls *Listeners) openZiti(cfg config.ZitiListener, log *slog.Logger) (net.Listener, error) {
	zctx, err := ziti.NewContextFromFile(cfg.IdentityFile)
	if err != nil {
		return nil, fmt.Errorf("loading the ziti identity %s "+
			"(enroll one with: ziti edge enroll <jwt> --out %s): %w",
			cfg.IdentityFile, cfg.IdentityFile, err)
	}
	if err := zctx.Authenticate(); err != nil {
		zctx.Close()
		return nil, fmt.Errorf("authenticating to the ziti controller with %s: %w", cfg.IdentityFile, err)
	}

	ln, err := zctx.Listen(cfg.Service)
	if err != nil {
		zctx.Close()
		return nil, fmt.Errorf("binding the ziti service %q "+
			"(is there a Bind service policy naming this identity?): %w", cfg.Service, err)
	}

	// Recorded only once the bind succeeded, so the failure paths above own
	// their own cleanup and Close never double-closes a context.
	ls.contexts = append(ls.contexts, zctx)
	if len(cfg.AdminIdentities) > 0 {
		log.Warn("admin_identities on this ziti listener grants nothing: admin needs the admin "+
			"password on every connection; remove it from the config", "service", cfg.Service)
	}
	return &overlayListener{Listener: ln}, nil
}

// overlayListener tags every connection it accepts as having arrived over the overlay, with who
// dialed it.
//
// The tag is made here, at accept, because `http.Server.ConnContext` is handed a `net.Conn` and
// nothing else, so by the time a request is being served there is no way back to which listener
// it arrived on. The tag grants nothing by itself: it is what lets privatePath honour an admin
// session on this connection.
type overlayListener struct {
	net.Listener

	// label stands in for the dialer when the overlay reports none, so the request is still
	// recognised as having arrived over the overlay.
	label string
}

func (l *overlayListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	dialer := dialerOf(c)
	if dialer == "" {
		dialer = l.label
	}
	return &overlayConn{Conn: c, dialer: dialer}, nil
}

// overlayConn carries the dialing identity through to ConnContext.
type overlayConn struct {
	net.Conn
	dialer string
}

// dialerOf reads the dialing identity from an accepted overlay connection.
//
// `edge.ServiceConn` is the interface carrying it; `edge.Conn` embeds that one, and the
// methods live on the narrower of the two. A conditional assertion rather than a blind one,
// because this is also called for anything a test hands the listener.
func dialerOf(c net.Conn) string {
	if ec, ok := c.(edge.ServiceConn); ok {
		return ec.GetDialerIdentityId()
	}
	return ""
}

// overlayKey types the context value. An unexported empty struct, so nothing outside this
// package can set it — a request that claims to be an admin identity because a middleware
// somewhere wrote a string key is exactly the forgery this prevents.
type overlayKey struct{}

// overlayCaller is what a request knows about the connection it arrived on.
type overlayCaller struct {
	// Dialer is the identity id the controller reported, empty for a plain TCP connection.
	Dialer string
}

// ConnContext stamps the accepted connection's overlay identity onto every request served on
// it. Installed on the ingress http.Server.
//
// A plain TCP connection stamps nothing, which leaves the existing loopback rule in charge
// for the ordinary case.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	oc, ok := c.(*overlayConn)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, overlayKey{}, overlayCaller{
		Dialer: oc.dialer,
	})
}

// overlayCallerFrom reads what ConnContext stamped.
//
// The zero value when there is nothing — no dialer, no write grant — so a caller that forgets
// to check `ok` still gets the closed answer.
func overlayCallerFrom(ctx context.Context) (overlayCaller, bool) {
	v, ok := ctx.Value(overlayKey{}).(overlayCaller)
	return v, ok
}

// Close releases every listener and overlay identity.
//
// Errors are joined rather than returned on the first one: a listener that
// will not close must not stop the remaining ziti contexts from being torn
// down, or the controller keeps terminators pointing at a dead process.
func (ls *Listeners) Close() error {
	if ls == nil {
		return nil
	}

	var errs []error
	for _, ln := range ls.Net {
		// http.Server.Shutdown closes every listener it was serving, so on the
		// ordinary shutdown path these are already closed. That is success,
		// not an error worth reporting.
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	for _, zctx := range ls.contexts {
		zctx.Close()
	}
	ls.Net = nil
	ls.contexts = nil
	return errors.Join(errs...)
}
