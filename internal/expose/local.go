package expose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MountPrefix is the path every path-mounted preview lives under.
const MountPrefix = "/preview/"

// Local publishes previews on the daemon's own listener, under a path.
//
// It exists for two reasons. The first is development: you can exercise the
// whole clone-detect-build-comment pipeline without a zrok account, and the URL
// in the comment is one you can click. The second is that it is the reference
// implementation of Exposer — short enough to read in one sitting, so when the
// zrok or Frontdoor implementations misbehave you have something to compare
// against.
//
// It used to bind an ephemeral port per preview, and that was wrong in three
// ways at once. A port is not stable across a restart, so every URL recorded in
// the database went dead the moment the daemon was restarted while the row
// still claimed `ready`. A hundred open pull requests meant a hundred
// listeners. And `http://127.0.0.1:62725/` tells a reader nothing about which
// preview they are looking at.
//
// One path per preview instead: `/preview/<name>/`, on the address docpreview
// already serves the dashboard from. The URL survives restarts because it is
// derived from the name rather than from whatever the kernel handed out, it
// says what it points at, and there is exactly one listener.
//
// Local also carries the port-binding logic that the Frontdoor exposer builds
// on — see serve — since Frontdoor reaches previews over the network from an
// agent rather than dialing a listener we own.
type Local struct {
	log *slog.Logger

	// host is the address Frontdoor's per-preview ports bind to. Local's own
	// previews bind nothing.
	host string

	// origin is the scheme and authority previews are reachable at, e.g.
	// "http://127.0.0.1:8471". Empty means "emit a site-relative URL", which is
	// the honest answer when the daemon is reachable at an address it cannot
	// know — behind a ziti listener, say.
	origin string

	// mounted is keyed by preview id and holds the path-served previews.
	//
	// Keyed by id, not by name: a name is the branch by default, branch names
	// are not unique across repositories, and keying by name meant every
	// publish tore down a different project's preview. The name still decides
	// the path, and a second preview claiming a name another one holds is
	// refused rather than allowed to take it.
	mu      sync.Mutex
	mounted map[string]*mount

	// live holds the Frontdoor-style port shares, also keyed by preview id.
	live map[string]*localShare
}

type mount struct {
	name    string
	handler http.Handler
}

type localShare struct {
	listener net.Listener
	server   *http.Server
	port     int
}

// NewLocal builds a path-mounting exposer. origin is the scheme and authority
// the daemon is reachable at; pass "" if it is not known.
func NewLocal(log *slog.Logger, origin string) *Local {
	return &Local{
		log:     log.With("exposer", "local"),
		host:    "127.0.0.1",
		origin:  strings.TrimRight(origin, "/"),
		mounted: map[string]*mount{},
		live:    map[string]*localShare{},
	}
}

// SetOrigin sets the scheme and authority previews are advertised at.
//
// Separate from the constructor because the address is not always known when
// the exposer is built: a listener bound to port 0, or a test server, only has
// one after it starts, and the exposer has to exist first so its handler can be
// mounted. Call it before the first publish; URLs already handed out are
// recorded in the database and are not rewritten.
func (l *Local) SetOrigin(origin string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.origin = strings.TrimRight(origin, "/")
}

// MountPath is the path prefix a preview with this name is served under.
//
// Callers need it before the build, not at publish: Docusaurus bakes baseUrl in
// at build time, so a site that will live at /preview/docs-main/ has to be
// built knowing that. A preview built for "/" and served under a prefix loads
// its HTML and 404s every asset in it.
func (l *Local) MountPath(name string) string { return MountPrefix + name + "/" }

// Handler serves every mounted preview. The daemon mounts this at MountPrefix.
//
// Nothing is stripped from the path. Each preview.Site was built knowing its
// full prefix and strips its own, so handing it a shortened path would make it
// answer 404 to the only URL it accepts.
func (l *Local) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := mountedName(r.URL.Path)
		if name == "" {
			http.NotFound(w, r)
			return
		}

		l.mu.Lock()
		var h http.Handler
		for _, m := range l.mounted {
			if m.name == name {
				h = m.handler
				break
			}
		}
		l.mu.Unlock()

		if h == nil {
			// A preview that has been torn down, or a URL somebody kept. Say so
			// rather than serving the dashboard's 404, which reads as a routing
			// bug in docpreview rather than an expired link.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(w, "no preview named "+name+" is published", http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// mountedName extracts the preview name from a request path under MountPrefix.
func mountedName(path string) string {
	rest, ok := strings.CutPrefix(path, MountPrefix)
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

func (l *Local) Kind() string { return "local" }

// Validate always succeeds: binding an ephemeral loopback port needs no
// external service to be reachable.
func (l *Local) Validate(context.Context) error { return nil }

// Publish mounts h at MountPath(spec.Name).
func (l *Local) Publish(_ context.Context, spec Spec, h http.Handler) (*Publication, error) {
	l.mu.Lock()
	for id, m := range l.mounted {
		if m.name == spec.Name && id != spec.Key() {
			l.mu.Unlock()
			return nil, fmt.Errorf("the name %q is already serving a different preview (%s); "+
				"two previews render to the same path under this name_template — "+
				"use \"{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}\" to separate them",
				spec.Name, id)
		}
	}
	// Replacing this preview's own mount is the common path: a second push to
	// the same branch. Nothing to tear down — no listener, no remote object.
	entry := &mount{name: spec.Name, handler: h}
	l.mounted[spec.Key()] = entry
	l.mu.Unlock()

	url := l.origin + l.MountPath(spec.Name)
	l.log.Info("published preview",
		"preview", spec.PreviewID, "build", spec.BuildID, "name", spec.Name, "url", url)

	return NewPublication(url, spec.Name, func() error {
		l.unmount(spec.Key(), entry)
		return nil
	}), nil
}

// unmount removes a preview's mount, but only if the map still holds this exact
// entry.
//
// The identity check is the whole function. The daemon replaces a preview by
// publishing the new one and then closing the old Publication, in that order —
// so a close that deleted by key alone would remove the mount its own
// replacement had just installed. Every rebuilt preview went 404 while the
// database still said `ready`, and only previews nobody had pushed to twice
// kept working.
//
// The same shape guards d.running in the daemon and z.live in the ziti exposer,
// for the same reason: an object that outlives its successor must not be able
// to clean up on its behalf.
// count reports how many publications are mounted. For tests: whether closing one
// publication left its siblings alone is not observable from a Publication.
func (l *Local) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.mounted)
}

func (l *Local) unmount(previewID string, entry *mount) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mounted[previewID] == entry {
		delete(l.mounted, previewID)
	}
}

// serve binds an ephemeral port and starts an HTTP server on it. Shared with
// the Frontdoor exposer.
func (l *Local) serve(h http.Handler) (*localShare, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(l.host, "0"))
	if err != nil {
		return nil, fmt.Errorf("binding preview port on %s: %w", l.host, err)
	}

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, fmt.Errorf("unexpected listener address type %T", ln.Addr())
	}

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.log.Error("preview server stopped", "port", addr.Port, "error", err)
		}
	}()

	return &localShare{listener: ln, server: srv, port: addr.Port}, nil
}

func (l *Local) withdraw(previewID string) {
	l.mu.Lock()
	entry, ok := l.live[previewID]
	delete(l.live, previewID)
	l.mu.Unlock()
	if !ok {
		return
	}
	if err := closeLocal(entry); err != nil {
		l.log.Error("withdrawing preview", "preview", previewID, "error", err)
	}
}

func closeLocal(entry *localShare) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := entry.server.Shutdown(ctx); err != nil {
		// Shutdown already closed the listener on the happy path; a forced
		// close here is the fallback for a hung connection.
		entry.listener.Close()
		return fmt.Errorf("shutting down preview server on port %d: %w", entry.port, err)
	}
	return nil
}

// Reap drops mounts whose previews the database no longer recognises.
//
// It used to be a no-op, on the reasoning that a loopback listener cannot
// outlive its process. That held while every preview owned a port; a mount is
// a map entry, and a map entry left behind after a preview is deleted keeps
// serving a URL nothing records — which is the same leak, in memory.
//
// A nil keep set means "keep nothing", matching the startup call.
func (l *Local) Reap(_ context.Context, keep map[string]bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := range l.mounted {
		if !keep[id] {
			delete(l.mounted, id)
		}
	}
	return nil
}

// Close stops every live preview.
func (l *Local) Close() error {
	l.mu.Lock()
	entries := make([]*localShare, 0, len(l.live))
	for name, entry := range l.live {
		entries = append(entries, entry)
		delete(l.live, name)
	}
	clear(l.mounted)
	l.mu.Unlock()

	var errs []error
	for _, entry := range entries {
		if err := closeLocal(entry); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
