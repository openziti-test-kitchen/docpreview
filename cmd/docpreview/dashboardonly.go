package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// dashboardPaths is every route the dashboard needs, and nothing else.
//
// An allowlist rather than a denylist. A denylist has to be updated every time a
// route is added, and the failure mode of forgetting is that the new route is
// public — so the list that is dangerous to get wrong is the one that has to be
// written out deliberately.
//
// GET only, enforced by the pattern. Every one of these is a read; the daemon's
// only mutating surface is /api/secrets, which is absent here and refused by the
// daemon anyway once this proxy's forwarding header reaches it.
var dashboardPaths = []string{
	"/{$}",                             // the dashboard itself
	"/status",                          // the JSON the page renders
	"/events",                          // the live status stream
	"/pr",                              // the local platform's pull request page
	"/pr/",                             //
	"/logs/{preview}",                  // the build log index for a preview
	"/logs/{preview}/stream",           // a live build log
	"/logs/{preview}/download",         // the whole log
	"/logs/{preview}/download/{build}", // one build's log
}

// cmdDashboardOnly publishes the read-only dashboard and nothing else.
//
// The sibling of webhook-only and the same shape: a reverse proxy that forwards
// an allowlist and 404s everything else, optionally over a named zrok share. Two
// commands rather than one with a mode flag, because they are published at
// different names and stopped independently — sharing one process would mean
// taking the webhook down to stop showing somebody the dashboard.
//
// # What this exposes
//
// Every open documentation pull request across every repository the App is
// installed on: branch names, commit SHAs, build durations, and the full build
// log of each. On a public repository that is all public already. On a private
// one it is not, and there is no authentication here — put `--basic-auth` on the
// zrok share, which works in a browser in a way it cannot for a webhook.
//
// # What it does not expose
//
// /api/secrets and /secrets are absent from the allowlist, so they 404 here. That
// is the first of two layers: this proxy sets X-Forwarded-For, and the daemon
// refuses credential writes from a forwarded request regardless. Neither layer
// alone would be enough — the daemon's own gate cannot see a tunnel, and an
// allowlist is one editing mistake from leaking.
func cmdDashboardOnly(args []string) error {
	fs := flag.NewFlagSet("dashboard-only", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8482", "address to accept tunnelled requests on")
	upstream := fs.String("upstream", "http://127.0.0.1:8471", "the docpreview daemon to forward to")
	zrokName := fs.String("zrok-name", "",
		"serve over a named public zrok share instead of a local port, e.g. -zrok-name docpreview-dash")
	zrokNamespace := fs.String("zrok-namespace", "",
		"namespace for -zrok-name; blank uses the zrok environment's default")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}

	target, err := url.Parse(*upstream)
	if err != nil {
		return fmt.Errorf("parsing -upstream %q: %w", *upstream, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return fmt.Errorf("-upstream must be a full URL like http://127.0.0.1:8471, got %q", *upstream)
	}
	if host, _, err := net.SplitHostPort(target.Host); err == nil && !isLoopbackHost(host) {
		return fmt.Errorf("-upstream %s is not loopback: this proxy exists to publish a loopback "+
			"daemon and forwarding elsewhere would make it a relay", target.Host)
	}

	log := newLogger(*logLevel)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			// The daemon reads this to refuse credential writes. Setting it is
			// load-bearing, not informational.
			r.SetXForwarded()
		},
		// Flush every write immediately. /events and /logs/{preview}/stream are
		// server-sent events, and the default buffering holds a live feed until
		// enough bytes accumulate — which for a status stream that emits one small
		// message per change is indistinguishable from the dashboard being frozen.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Error("forwarding to the daemon failed", "error", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()
	for _, p := range dashboardPaths {
		mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
			log.Debug("forwarding", "path", r.URL.Path, "remote", r.RemoteAddr)
			proxy.ServeHTTP(w, r)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Debug("refused", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		http.NotFound(w, r)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No WriteTimeout. Two of these routes are open-ended event streams, and a
		// write deadline would sever a live build log mid-tail at whatever interval
		// it was set to.
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 16,
	}

	if *zrokName != "" {
		ln, shareURL, cleanup, err := zrokListener(*zrokName, *zrokNamespace, log)
		if err != nil {
			return err
		}
		defer cleanup()

		log.Info("dashboard serving over zrok", "url", shareURL, "to", target.String())
		log.Warn("this publishes every open documentation pull request and its build logs, unauthenticated",
			"fix", "add --basic-auth to the zrok share if that is not acceptable")

		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *listen, err)
	}
	log.Info("dashboard proxy listening", "listen", ln.Addr().String(), "to", target.String())

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
