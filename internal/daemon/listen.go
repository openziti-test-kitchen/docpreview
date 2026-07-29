package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/openziti/sdk-golang/ziti"

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

	// Descriptions parallels Net, for logging and `docpreview doctor`. Held
	// separately because a ziti listener's Addr() is an overlay address that
	// says nothing about which service was bound.
	Descriptions []string

	contexts []ziti.Context
}

// Open binds every configured listener, or none of them.
//
// All-or-nothing on purpose. A partial bind would leave docpreview serving the
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
		if l.Ziti != nil {
			ln, err = open.openZiti(*l.Ziti)
		} else {
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
// them, so half the dashboard requests would reach a docpreview that knows
// nothing about them. The error text names the Bind policy because a missing
// one is the overwhelmingly common cause and the SDK's own message does not
// mention policies at all.
func (ls *Listeners) openZiti(cfg config.ZitiListener) (net.Listener, error) {
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
	return ln, nil
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
