package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
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
	"/login",                           // the login form, when the daemon asks for one
}

// dashboardPostPaths are the routes that need POST as well.
//
// Two, and both are the login. Without them a daemon with a viewer password set would serve the
// form through this tunnel and 404 the submission — a sign-in page that cannot sign anybody in,
// which is a worse failure than having no login at all because it looks like the password is
// wrong.
//
// Deliberately still an allowlist. The alternative — forwarding every POST — would hand the
// tunnel the credential and project APIs, which is exactly what this command exists to prevent.
var dashboardPostPaths = []string{
	"/login",
	"/logout",
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
	// The dashboard's own sign-in, done by zrok's frontend rather than by docpreview. Previews
	// deliberately have no equivalent: a preview URL goes in a pull request comment for anybody
	// reviewing to open, and a sign-in in front of it defeats the tool.
	oauth := fs.String("oauth", "",
		"gate the share at the zrok frontend: google or github (needs -oauth-domain)")
	oauthDomain := fs.String("oauth-domain", "",
		"comma-separated email domains allowed through -oauth, e.g. example.com")
	zrokHome := fs.String("zrok-home", "",
		"the zrok environment directory; blank uses the machine's ~/.zrok2")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// See the note in webhookonly.go: this process reads no config, so the zrok directory has to
	// be passed in whenever the daemon is not using the machine's.
	if err := useZrokHome(*zrokHome); err != nil {
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
	for _, p := range dashboardPostPaths {
		mux.HandleFunc("POST "+p, func(w http.ResponseWriter, r *http.Request) {
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
		auth := zrokFrontendAuth{Provider: strings.ToLower(strings.TrimSpace(*oauth))}
		for _, d := range strings.Split(*oauthDomain, ",") {
			if d = strings.TrimSpace(d); d != "" {
				// Written as a domain and used as a glob, because a domain is what an
				// operator has in mind and `*@example.com` is what zrok matches against.
				// Accepting a pattern verbatim as well, for the case where the intended set
				// is not one whole domain.
				if strings.ContainsAny(d, "*@") {
					auth.EmailPatterns = append(auth.EmailPatterns, d)
				} else {
					auth.EmailPatterns = append(auth.EmailPatterns, "*@"+d)
				}
			}
		}
		// A provider with no domain authenticates every account that provider will
		// authenticate — every Google account in existence. That is almost certainly not what
		// somebody typing -oauth meant, and it fails closed here rather than being discovered
		// from the access log.
		if auth.Provider != "" && len(auth.EmailPatterns) == 0 {
			return errors.New("-oauth needs -oauth-domain: a provider with no domain lets in " +
				"every account that provider will authenticate")
		}
		if auth.Provider == "" && len(auth.EmailPatterns) > 0 {
			return errors.New("-oauth-domain needs -oauth: there is no provider to check it against")
		}

		ln, shareURL, cleanup, err := zrokListener(*zrokName, *zrokNamespace, auth, log)
		if err != nil {
			return err
		}
		defer cleanup()

		log.Info("dashboard serving over zrok", "url", shareURL, "to", target.String())
		switch {
		case auth.Provider != "":
			log.Info("the share is gated at the zrok frontend",
				"provider", auth.Provider, "emails", strings.Join(auth.EmailPatterns, ", "))
		default:
			// Still worth shouting about, and now with two answers rather than one: gate the
			// share at the frontend, or give the daemon a viewer password so it asks for one
			// itself.
			log.Warn("this publishes every open documentation pull request and its build logs, unauthenticated",
				"fix", "-oauth google -oauth-domain example.com, or: docpreview console password -role viewer")
		}

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
