package daemon

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// sameOriginWrites refuses a state-changing request that a browser says came from another site.
//
// The session cookie is SameSite=Lax, which stops cross-site posts and nothing nearer. Every zrok
// share is the same site as the dashboard's share, since they sit under one registrable domain,
// and every loopback port is the same site as a dashboard on 127.0.0.1. A page on any of those,
// a pull request's own preview among them, could post to this daemon with the visitor's session
// attached.
//
// A browser states where a request came from in Sec-Fetch-Site, and in Origin on anything that
// is not a GET. A request carrying neither is not a browser acting on a page's behalf: it is
// curl, the CLI or an SCM's webhook sender, none of which holds somebody else's cookie.
func sameOriginWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if crossSiteWrite(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this request came from another site and was refused"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// crossSiteWrite reports whether r changes state and was sent by a page on another origin.
func crossSiteWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	// Deliveries are authenticated by their signature, and an unsigned one has its own rule:
	// see refuseUnsignedDelivery.
	//
	// Only a path that is already in its clean form: the router redirects any other before a
	// handler sees it, and "/webhook/../api/x" must not be read here as a delivery.
	if strings.HasPrefix(r.URL.Path, "/webhook/") && path.Clean(r.URL.Path) == r.URL.Path {
		return false
	}

	// `none` is a request the user made directly: a typed address or a bookmark.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site != "same-origin" && site != "none"
	}

	// Sec-Fetch-Site is only sent from a secure context, so a dashboard reached over plain http
	// on a LAN address falls through to here.
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		// "null" included: a sandboxed document, which is what a local preview is.
		return true
	}
	if strings.EqualFold(u.Host, r.Host) {
		return false
	}
	// Behind the public dashboard proxy the Host is the daemon's loopback address, and the
	// name the browser used arrives in X-Forwarded-Host. A page cannot set that header on a
	// cross-site request without a preflight this daemon never answers.
	for _, fh := range r.Header.Values("X-Forwarded-Host") {
		if strings.EqualFold(u.Host, strings.TrimSpace(fh)) {
			return false
		}
	}
	return true
}

// jsonBody reports whether a request declares a JSON body.
//
// A page can post text/plain, a form or an untyped blob to another origin with no preflight, and
// a JSON decoder reads any of them. Only application/json forces the preflight, so only that is
// decoded.
func jsonBody(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// setPageHeaders adds the headers every HTML page of the dashboard carries.
//
// No page of this daemon belongs in a frame, so framing is refused outright: the dashboard has
// one-click controls that rebuild, cancel and unlink. base-uri and form-action stop injected
// markup from repointing the page's relative URLs or posting its forms elsewhere.
//
// There is no script-src here because each page adds its own, naming its one script by hash:
// see dashboardCSP and loginCSP.
func setPageHeaders(h http.Header) {
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("X-Frame-Options", "DENY")
	// A page that opened this one, or that this one opened, holds no handle on it: a preview
	// that opens the dashboard in a new tab cannot navigate that tab somewhere else afterwards.
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
}

const pageCSP = "frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

// errNotJSON is decodeJSON's refusal of a body not declared as JSON.
var errNotJSON = errors.New("the body must be sent as Content-Type: application/json")
