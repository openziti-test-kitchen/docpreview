package expose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/edge"

	"github.com/netfoundry/docpreview/internal/config"
)

// Ziti publishes previews on an OpenZiti overlay, reachable only from a machine
// running a tunneler with an enrolled identity.
//
// This is the one exposer with no public surface at all. zrok and Frontdoor
// both produce an address that exists on the internet and then decide who to
// let through; here the hostname does not resolve, the address is not routable,
// and the service cannot be dialed without a right granted on the controller.
//
// It is also the one exposer that is not one-listener-per-preview. A single
// wildcard service covers every preview that will ever exist, and requests are
// separated by Host header. The reasons are in www/docs/future/ziti-native-previews.md;
// the short version is that a service per pull request would create four
// management-API objects each time someone pushes and would churn DNS rules on
// every connected tunneler.
//
// Publishing therefore costs nothing remote. It is a map insert.
//
// One operational constraint follows from the single shared service, and it is
// not obvious: exactly one docpreview may bind it. Binding creates a ziti
// *terminator*, two bindings create two, and the router load-balances between
// them under the default smartrouting strategy. Two instances sharing a service
// would each hold a disjoint routing table, so every preview would work about
// half the time and 404 the rest — which looks like a flaky network rather than
// a configuration error. Give a second instance its own service and domain.
type Ziti struct {
	cfg config.ZitiConfig
	log *slog.Logger

	// ctx is the enrolled overlay identity. Opened in Validate rather than in
	// the constructor so that a missing or unenrolled identity is one clear
	// startup error alongside every other check.
	ctx      ziti.Context
	listener edge.Listener
	srv      *http.Server

	// bound records that Validate has run and the service is being served.
	// Separate from the listener because that is what Publish actually needs to
	// know, and because it lets the routing and lifecycle be tested without a
	// controller to bind against.
	bound bool

	mu   sync.Mutex
	live map[string]*zitiPreview
}

// zitiPreview is one published preview.
//
// It carries the preview ID alongside the handler because this exposer routes
// by hostname label while the rest of docpreview identifies previews by ID, and
// those two namespaces can disagree. Two pull requests in different
// repositories both on a branch called `main` render to the same label under
// the default name template.
//
// The pointer matters as much as the ID. A publication's Close must delete only
// the entry it created, not whatever currently occupies that label — otherwise
// tearing down an old preview silently unpublishes the one that replaced it.
type zitiPreview struct {
	previewID string
	handler   http.Handler
}

// NewZiti builds a ziti exposer. Nothing touches the network until Validate.
func NewZiti(cfg config.ZitiConfig, log *slog.Logger) (*Ziti, error) {
	if cfg.IdentityFile == "" {
		return nil, errors.New("exposer.ziti.identity_file must be set " +
			"(the enrolled identity docpreview hosts the service with)")
	}
	if cfg.Service == "" {
		return nil, errors.New("exposer.ziti.service must be set")
	}
	if cfg.Domain == "" {
		return nil, errors.New("exposer.ziti.domain must be set " +
			"(it must match the addresses in the service's intercept.v1 config)")
	}

	return &Ziti{
		cfg:  cfg,
		log:  log.With("exposer", "ziti"),
		live: map[string]*zitiPreview{},
	}, nil
}

func (z *Ziti) Kind() string { return "ziti" }

// Validate loads the identity, binds the service, and starts serving.
//
// Binding here rather than on first publish is deliberate. It reaches the
// controller, which is what the Exposer contract asks of Validate — and the
// two failures worth catching early both surface at exactly this point: an
// identity that will not authenticate, and a Bind policy that does not name it.
// Both otherwise appear as a mystifying failure on the first pull request.
func (z *Ziti) Validate(ctx context.Context) error {
	if z.bound {
		return nil
	}

	zctx, err := ziti.NewContextFromFile(z.cfg.IdentityFile)
	if err != nil {
		return fmt.Errorf("loading the ziti identity %s "+
			"(enroll one with: ziti edge enroll <jwt> --out %s): %w",
			z.cfg.IdentityFile, z.cfg.IdentityFile, err)
	}
	if err := zctx.Authenticate(); err != nil {
		return fmt.Errorf("authenticating to the ziti controller with %s: %w", z.cfg.IdentityFile, err)
	}

	listener, err := zctx.Listen(z.cfg.Service)
	if err != nil {
		zctx.Close()
		return fmt.Errorf("binding the ziti service %q "+
			"(is there a Bind service policy naming this identity?): %w", z.cfg.Service, err)
	}

	z.ctx = zctx
	z.listener = listener
	z.bound = true
	z.srv = &http.Server{
		Handler:           http.HandlerFunc(z.route),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		if err := z.srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			z.log.Error("overlay server stopped", "error", err)
		}
	}()

	z.log.Info("bound ziti service",
		"service", z.cfg.Service, "domain", z.cfg.Domain, "identity", z.cfg.IdentityFile)
	return nil
}

// route dispatches by Host header.
//
// The tunneler is a layer-4 proxy, so the browser's Host arrives verbatim —
// that is the property the whole wildcard design rests on. The first label
// selects the preview.
func (z *Ziti) route(w http.ResponseWriter, r *http.Request) {
	name := hostLabel(r.Host)

	z.mu.Lock()
	entry, ok := z.live[name]
	names := make([]string, 0, len(z.live))
	if !ok {
		for n := range z.live {
			names = append(names, n)
		}
	}
	z.mu.Unlock()

	if ok {
		entry.handler.ServeHTTP(w, r)
		return
	}

	// A 404 here means the reviewer reached docpreview but asked for a preview
	// that is not published — a closed pull request, a typo, a stale bookmark.
	// Listing what is live turns that into a usable page rather than a dead
	// end, and it is the only thing the apex hostname would ever serve.
	z.log.Debug("no preview for host", "host", r.Host, "label", name)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)

	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>No such preview</title>"+
		"<h1>No preview at <code>%s</code></h1>", htmlEscape(r.Host))
	if len(names) == 0 {
		fmt.Fprint(w, "<p>Nothing is published right now.</p>")
		return
	}
	fmt.Fprint(w, "<p>Currently published:</p><ul>")
	for _, n := range names {
		fmt.Fprintf(w, `<li><a href="http://%s.%s/">%s</a></li>`,
			htmlEscape(n), htmlEscape(z.cfg.Domain), htmlEscape(n))
	}
	fmt.Fprint(w, "</ul>")
}

// hostLabel extracts the first DNS label from a Host header, dropping any port.
func hostLabel(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	label, _, _ := strings.Cut(host, ".")
	return strings.ToLower(label)
}

// htmlEscape is the minimum needed for the 404 page, which interpolates a
// Host header — attacker-controlled, since anyone on the overlay can send one.
func htmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;",
	).Replace(s)
}

// Publish registers a preview under its hostname label.
//
// No remote call, no listener, no port — publishing is a map insert. Two things
// that costs us, both of which the other exposers get from the remote side for
// free:
//
// A rebuild of the same preview must replace its own entry, but a *different*
// preview must not silently take over a label somebody else is serving. zrok
// gets this from the controller, which owns the namespace and rejects a
// duplicate; here the map is the only registry, so the check has to be explicit.
// It is refused rather than disambiguated because a collision means the name
// template is too loose — the default is the branch alone, and two repositories
// both with a `main` branch collide — and quietly renaming a preview would hide
// a configuration problem behind a URL nobody expects.
//
// And the returned Close must remove only what this call inserted. Deleting
// whatever currently holds the label would let a torn-down preview unpublish
// the one that replaced it.
func (z *Ziti) Publish(ctx context.Context, spec Spec, h http.Handler) (*Publication, error) {
	if !z.bound {
		return nil, errors.New("the ziti exposer is not bound; Validate must run first")
	}

	name := strings.ToLower(spec.Name)
	entry := &zitiPreview{previewID: spec.PreviewID, handler: h}

	z.mu.Lock()
	if existing, ok := z.live[name]; ok && existing.previewID != spec.PreviewID {
		z.mu.Unlock()
		return nil, fmt.Errorf("the name %q is already serving a different preview (%s); "+
			"two previews render to the same hostname under this name_template — "+
			"use \"{{.Repo.Name}}-{{.Name}}\" to separate them",
			name, existing.previewID)
	}
	z.live[name] = entry
	z.mu.Unlock()

	url := JoinURL("http://"+name+"."+z.cfg.Domain, spec.BaseURL)
	z.log.Info("published preview", "name", name, "url", url, "preview", spec.PreviewID)

	return NewPublication(url, name, func() error {
		z.withdraw(name, entry)
		return nil
	}), nil
}

// withdraw removes a preview, but only if the label still holds that exact
// entry. See the note on Publish.
func (z *Ziti) withdraw(name string, entry *zitiPreview) {
	z.mu.Lock()
	defer z.mu.Unlock()

	if current, ok := z.live[name]; ok && current == entry {
		delete(z.live, name)
	}
}

// Reap drops routing entries whose preview is no longer wanted.
//
// Unlike the other exposers there is nothing on a controller to collect: the
// wildcard service is permanent and previews never created remote objects. So
// this only prunes memory, and at startup — when keep is empty and the map is
// too — it does nothing at all.
func (z *Ziti) Reap(_ context.Context, keep map[string]bool) error {
	if len(keep) == 0 {
		return nil
	}
	// The map is keyed by hostname label while keep is keyed by preview ID, so
	// there is no direct correspondence to prune on. Entries are removed by
	// Publication.Close, which teardown and supersede both call; anything left
	// here belongs to a live preview.
	return nil
}

// Close stops serving and releases the overlay identity.
func (z *Ziti) Close() error {
	if z.srv == nil {
		return nil
	}

	var errs []error

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := z.srv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutting down the overlay server: %w", err))
	}
	if err := z.listener.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing the overlay listener: %w", err))
	}
	if z.ctx != nil {
		z.ctx.Close()
	}

	z.mu.Lock()
	z.live = map[string]*zitiPreview{}
	z.mu.Unlock()

	return errors.Join(errs...)
}
