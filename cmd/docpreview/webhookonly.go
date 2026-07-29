package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/openziti/zrok/v2/environment"
	"github.com/openziti/zrok/v2/environment/env_core"
	"github.com/openziti/zrok/v2/rest_client_zrok/metadata"
	"github.com/openziti/zrok/v2/sdk/golang/sdk"
)

// cmdWebhookOnly reverse-proxies exactly one route to the daemon and refuses
// everything else.
//
// # Why this exists
//
// A tunnel is the only way GitHub can reach a daemon bound to loopback, and
// zrok's proxy backend shares an origin rather than a path — `zrok2 share
// public http://127.0.0.1:8471` publishes every route the daemon serves. That
// includes `/api/secrets`, whose write endpoints are gated only on
// `SecretsAdmin.Available`, which tests whether the daemon's **listeners** are
// loopback. They are. So the gate says yes while the surface is on the internet,
// and an unlocked vault means PUT, DELETE and generate all succeed for anyone
// holding the share URL.
//
// No check inside the daemon can tell the difference. In proxy mode the daemon
// sees the connection from the local zrok process, so RemoteAddr is loopback
// too, and Host is whatever the client sent. The distinction does not exist at
// that layer, which is why the answer is not to put the admin surface and the
// public route on one origin.
//
// So: point the tunnel here instead. This process publishes one method and one
// path, and the dashboard, the previews and the credential API are reachable
// only from the machine itself, which is what they were designed for.
//
// # What it deliberately does not do
//
// Verify the webhook signature. That is the daemon's job and it already does it
// with the secret from the vault; duplicating it here would mean a second copy
// of the secret in a second process. This is a router, not a guard — the guard
// is one hop further in, and a forged payload gets the same 401 it always did.
func cmdWebhookOnly(args []string) error {
	fs := flag.NewFlagSet("webhook-only", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8481", "address to accept tunnelled requests on")
	upstream := fs.String("upstream", "http://127.0.0.1:8471", "the docpreview daemon to forward to")
	path := fs.String("path", "/webhook/github", "the single path to forward")
	zrokName := fs.String("zrok-name", "",
		"serve over a named public zrok share instead of a local port, e.g. -zrok-name docpreview")
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

	// Refuse to forward to something that is not loopback. The point of this
	// process is to be the only publicly reachable thing; pointing it at a
	// remote daemon would make it an open relay for that daemon's webhook
	// endpoint, which is not a use case worth supporting by accident.
	if host, _, err := net.SplitHostPort(target.Host); err == nil && !isLoopbackHost(host) {
		return fmt.Errorf("-upstream %s is not loopback: this proxy exists to publish a loopback "+
			"daemon and forwarding elsewhere would make it a relay", target.Host)
	}

	log := newLogger(*logLevel)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Keep the client's Host out of it. The daemon does not route on
			// Host, and forwarding a tunnel hostname only creates a way for a
			// caller to influence something later code might read.
			r.Out.Host = target.Host
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// The daemon being down is a 502, not a 500: GitHub retries a 5xx
			// delivery and this is exactly the case where a retry helps.
			log.Error("forwarding to the daemon failed", "error", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()

	// One route, method included in the pattern, so a GET to the same path is a
	// 405 from the mux rather than something forwarded.
	mux.HandleFunc("POST "+*path, func(w http.ResponseWriter, r *http.Request) {
		log.Info("forwarding webhook", "remote", r.RemoteAddr, "delivery", r.Header.Get("X-GitHub-Delivery"))
		proxy.ServeHTTP(w, r)
	})

	// A GET on the webhook path gets 405 and a sentence, not a bare 404.
	//
	// The first thing anyone does with a webhook URL is paste it into a browser to
	// check it, and a browser sends GET — so the useful answer is "right URL,
	// wrong method". This leaks nothing that is not already discoverable: a POST
	// to the real path answers 401 and a POST to anything else answers 404, so the
	// path is distinguishable to anyone probing properly regardless.
	mux.HandleFunc("GET "+*path, func(w http.ResponseWriter, r *http.Request) {
		log.Debug("refused a GET on the webhook path", "remote", r.RemoteAddr)
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "this endpoint accepts signed POST deliveries only; "+
			"there is nothing here to open in a browser", http.StatusMethodNotAllowed)
	})

	// Everything else, including "/", answers 404 and says nothing about what is
	// behind it. A tunnel URL gets scanned; there is no reason to confirm that a
	// dashboard exists.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Debug("refused", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		http.NotFound(w, r)
	})

	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// A webhook body is small and GitHub is not slow. These bounds exist
		// because this is the one process deliberately exposed to the internet.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	// A zrok listener, when asked for. This is the better arrangement and not
	// merely a convenience: there is no local TCP port for anything else to find,
	// and the URL is stable because the name is reserved rather than minted per
	// share.
	//
	// It also sidesteps the CLI. `zrok2 share public` has one positional — the
	// target — and its -n flag sets only NameSelection.NamespaceToken, so the
	// rc8 CLI cannot claim a reserved name for a public share at all. The SDK
	// can, and internal/expose/zrok.go already does it the same way.
	if *zrokName != "" {
		ln, url, cleanup, err := zrokListener(*zrokName, *zrokNamespace, log)
		if err != nil {
			return err
		}
		defer cleanup()

		log.Info("webhook-only serving over zrok", "url", url, "forwards", "POST "+*path, "to", target.String())
		log.Info("this is the webhook URL to give GitHub", "webhook", url+*path)

		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *listen, err)
	}

	log.Info("webhook-only proxy listening",
		"listen", ln.Addr().String(), "forwards", "POST "+*path, "to", target.String())
	log.Info("point the tunnel here, not at the daemon",
		"share", fmt.Sprintf("zrok2 share public --headless -b proxy http://%s", ln.Addr().String()))

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// zrokListener creates a named public zrok share and returns a listener for it.
//
// The name must already exist and be reserved — `zrok2 create name <name>`. That
// is deliberate: reserving is an account-level act with a quota behind it, and a
// process that silently created names would leak one per typo.
//
// The returned cleanup deletes the share but not the name. Deleting the share
// releases the binding so the next run can claim it again; deleting the name
// would throw away the stable URL that is the whole reason for this path.
func zrokListener(name, namespace string, log *slog.Logger) (net.Listener, string, func(), error) {
	root, err := environment.LoadRoot()
	if err != nil {
		return nil, "", nil, fmt.Errorf("loading the zrok environment (is zrok2 enabled?): %w", err)
	}
	if !root.IsEnabled() {
		return nil, "", nil, fmt.Errorf("the zrok environment is not enabled; run: zrok2 enable <token>")
	}
	if namespace == "" {
		ns, _ := root.DefaultNamespace()
		if ns == "" {
			return nil, "", nil, fmt.Errorf("no zrok namespace given and the environment has no default; " +
				"pass -zrok-namespace")
		}
		namespace = ns
	}

	req := &sdk.ShareRequest{
		ShareMode:      sdk.PublicShareMode,
		BackendMode:    sdk.ProxyBackendMode,
		NameSelections: []sdk.NameSelection{{NamespaceToken: namespace, Name: name}},
		// Open, because the caller is GitHub and it holds no zrok account. The
		// endpoint's own protection is the webhook signature, which the daemon
		// verifies with the secret from the vault. Access grants here would lock
		// out the only client that matters.
		PermissionMode: sdk.OpenPermissionMode,
	}

	shr, err := sdk.CreateShare(root, req)
	if err != nil {
		// A name still held by a share from a previous run. This is the ordinary
		// case rather than the exceptional one: the share is deleted on graceful
		// shutdown, and a process serving a tunnel is usually ended by a kill,
		// which runs no deferred anything. Without this, every restart needs a
		// manual `zrok2 delete share`.
		//
		// Reclaiming is safe precisely because the name is reserved: it belongs to
		// this account, and the only thing that can be holding it is our own
		// abandoned share. internal/expose/zrok.go does the same for previews.
		if reclaimed := reclaimZrokName(root, namespace, name, log); reclaimed {
			shr, err = sdk.CreateShare(root, req)
		}
		if err != nil {
			return nil, "", nil, fmt.Errorf("creating the zrok share for name %q in namespace %q "+
				"(create it first with: zrok2 create name %s): %w", name, namespace, name, err)
		}
	}

	ln, err := sdk.NewListener(shr.Token, root)
	if err != nil {
		// The share exists but nothing can serve it, and leaving it holds the
		// name against the next attempt.
		if delErr := sdk.DeleteShare(root, shr); delErr != nil {
			log.Error("could not clean up the share after a listener error",
				"token", shr.Token, "error", delErr)
		}
		return nil, "", nil, fmt.Errorf("opening the zrok listener for share %s: %w", shr.Token, err)
	}

	// The controller reports where the share is reachable, so nothing here has to
	// guess at a DNS suffix. It is a list because a namespace can front a share
	// at more than one endpoint; the first is the one to hand out.
	url := ""
	if len(shr.FrontendEndpoints) > 0 {
		url = shr.FrontendEndpoints[0]
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			url = "https://" + url
		}
	}
	url = strings.TrimRight(url, "/")

	cleanup := func() {
		if err := sdk.DeleteShare(root, shr); err != nil {
			log.Error("could not delete the zrok share on shutdown",
				"token", shr.Token, "error", err,
				"fix", "zrok2 delete share "+shr.Token)
			return
		}
		log.Info("deleted the zrok share", "token", shr.Token)
	}
	return ln, url, cleanup, nil
}

// isLoopbackHost is the same test the secrets admin applies to listeners, kept
// separate because that one lives in another package and this one guards a
// different thing.
// reclaimZrokName deletes any share in this environment still holding name.
//
// Scoped to this environment, and matched on the share's own frontend endpoints
// rather than on a guessed URL, so it can only delete a share this account
// created under this identity for this name.
func reclaimZrokName(root env_core.Root, namespace, name string, log *slog.Logger) bool {
	client, err := root.Client()
	if err != nil {
		return false
	}

	envZID := root.Environment().ZitiIdentity
	params := metadata.NewListSharesParams()
	params.EnvZID = &envZID

	resp, err := client.Metadata.ListShares(params, zrokAuth(root))
	if err != nil || resp.Payload == nil {
		return false
	}

	deleted := false
	for _, shr := range resp.Payload.Shares {
		if shr == nil || !endpointLabelMatches(shr.FrontendEndpoints, name) {
			continue
		}
		if err := sdk.DeleteShare(root, &sdk.Share{Token: shr.ShareToken}); err != nil {
			log.Warn("could not reclaim the abandoned share holding this name",
				"token", shr.ShareToken, "name", name, "error", err)
			continue
		}
		log.Warn("reclaimed an abandoned zrok share that still held the name",
			"token", shr.ShareToken, "name", name, "namespace", namespace)
		deleted = true
	}
	return deleted
}

func zrokAuth(root env_core.Root) runtime.ClientAuthInfoWriter {
	return httptransport.APIKeyAuth("X-TOKEN", "header", root.Environment().AccountToken)
}

// endpointLabelMatches reports whether any endpoint's leftmost DNS label is name.
//
// The label, not a substring: a name of "docs" must not match a share published
// at "docs-internal.shares.zrok.io", because reclaiming deletes, and a loose
// match here deletes somebody else's share.
func endpointLabelMatches(endpoints []string, name string) bool {
	for _, ep := range endpoints {
		host := ep
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		host, _, _ = strings.Cut(host, "/")
		label, _, _ := strings.Cut(host, ".")
		if strings.EqualFold(label, name) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
