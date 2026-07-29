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

	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/openziti/zrok/v2/environment"
	"github.com/openziti/zrok/v2/environment/env_core"
	"github.com/openziti/zrok/v2/rest_client_zrok/metadata"
	"github.com/openziti/zrok/v2/rest_client_zrok/share"
	"github.com/openziti/zrok/v2/sdk/golang/sdk"

	"github.com/netfoundry/docpreview/internal/config"
)

// targetPrefix tags every share docpreview creates.
//
// zrok's Target field is free-form for our purposes — with an SDK listener we
// serve the traffic ourselves, so nothing dials the target — which makes it a
// convenient place to stamp ownership. ListShares supports a substring filter
// on Target, so this prefix is how Reap tells "a share docpreview made" from
// "a share the operator made by hand in another terminal". Deleting the latter
// would be rude.
const targetPrefix = "docpreview:"

// Zrok publishes previews over an OpenZiti overlay using zrok v2.
//
// The v2 model is what makes this work cleanly. In v1 a stable public address
// meant a "reserved share", and moving or rebuilding it churned the URL. In v2
// names live in a namespace independently of any share, so we can attach the
// name "my-feature-branch" to a fresh share on every rebuild and the reviewer's
// bookmark keeps working. That property is the whole reason the PR comment can
// be written once and edited thereafter.
type Zrok struct {
	cfg config.ZrokConfig
	log *slog.Logger

	root      env_core.Root
	namespace string

	mu   sync.Mutex
	live map[string]*zrokShare // keyed by preview id
}

type zrokShare struct {
	token     string
	previewID string
	name      string
	listener  interface{ Close() error }
	server    *http.Server
}

// NewZrok builds a zrok exposer. It loads the on-disk zrok environment
// immediately so that a missing or unenabled environment is reported at
// construction rather than on the first build.
func NewZrok(cfg config.ZrokConfig, log *slog.Logger) (*Zrok, error) {
	root, err := environment.LoadRoot()
	if err != nil {
		return nil, fmt.Errorf("loading zrok environment (is zrok2 installed and enabled?): %w", err)
	}

	z := &Zrok{
		cfg:  cfg,
		log:  log.With("exposer", "zrok2"),
		root: root,
		live: map[string]*zrokShare{},
	}

	z.namespace = cfg.Namespace
	if z.namespace == "" {
		ns, src := root.DefaultNamespace()
		if ns == "" {
			return nil, errors.New("no zrok namespace configured and the environment has no default; " +
				"set exposer.zrok2.namespace or run 'zrok2 config set defaultNamespace <ns>'")
		}
		z.namespace = ns
		z.log.Debug("using zrok default namespace", "namespace", ns, "source", src)
	}

	return z, nil
}

func (z *Zrok) Kind() string { return "zrok2" }

// Validate confirms the local zrok environment is enabled and that the
// controller is reachable and agrees we exist.
//
// Checking IsEnabled alone is not enough: the on-disk environment can be
// present and enabled while the account token has been revoked server-side, in
// which case every share creation fails with an opaque 401. One round trip here
// converts that into a startup error with a fix in it.
func (z *Zrok) Validate(ctx context.Context) error {
	if !z.root.IsEnabled() {
		return errors.New("zrok environment is not enabled; run 'zrok2 enable <account-token>'")
	}

	client, err := z.root.Client()
	if err != nil {
		return fmt.Errorf("building zrok client: %w", err)
	}

	params := metadata.NewGetEnvironmentDetailParamsWithContext(ctx)
	params.EnvZID = z.root.Environment().ZitiIdentity
	if _, err := client.Metadata.GetEnvironmentDetail(params, z.auth()); err != nil {
		return fmt.Errorf("zrok controller rejected this environment "+
			"(token revoked, or environment deleted server-side? try 'zrok2 disable' then 'zrok2 enable'): %w", err)
	}

	apiEndpoint, _ := z.root.ApiEndpoint()
	z.log.Info("zrok environment validated",
		"api", apiEndpoint,
		"namespace", z.namespace,
		"identity", z.root.Environment().ZitiIdentity)
	return nil
}

func (z *Zrok) auth() runtime.ClientAuthInfoWriter {
	return httptransport.APIKeyAuth("X-TOKEN", "header", z.root.Environment().AccountToken)
}

// Publish creates a public zrok share bound to spec.Name, opens an overlay
// listener for it, and serves h there.
func (z *Zrok) Publish(ctx context.Context, spec Spec, h http.Handler) (*Publication, error) {
	// Replacing this preview's own earlier publication is the common path: a
	// second push to the same branch. Do it before creating the new share so
	// the name is free.
	//
	// Keyed by preview id, not by name. Name is the label, and two previews can
	// ask for one label if the name_template does not separate them — keyed by
	// name, this line would tear down a different pull request's live share.
	// The name collision itself is refused below, which is the correct answer
	// for zrok: names are unique per namespace, so the second preview cannot
	// have it and should be told so rather than quietly taking it.
	z.withdraw(spec.Key())

	z.mu.Lock()
	for id, entry := range z.live {
		if entry.name == spec.Name && id != spec.Key() {
			z.mu.Unlock()
			return nil, fmt.Errorf("the name %q is already serving a different preview (%s); "+
				"two previews render to the same name under this name_template — "+
				"use \"{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}\" to separate them",
				spec.Name, id)
		}
	}
	z.mu.Unlock()

	req := &sdk.ShareRequest{
		ShareMode:      sdk.PublicShareMode,
		BackendMode:    sdk.ProxyBackendMode,
		Target:         targetPrefix + spec.Key(),
		NameSelections: []sdk.NameSelection{{NamespaceToken: z.namespace, Name: spec.Name}},
		PermissionMode: sdk.ClosedPermissionMode,
		AccessGrants:   z.cfg.AccessGrants,
	}
	if z.cfg.Open {
		req.PermissionMode = sdk.OpenPermissionMode
	}
	if z.cfg.OauthProvider != "" {
		req.OauthProvider = z.cfg.OauthProvider
		req.OauthEmailAddressPatterns = z.cfg.OauthEmailDomains
		req.OauthRefreshInterval = 3 * time.Hour
	}

	// The name has to exist before a share can bind it. zrok v2 does not create
	// one implicitly for a named share — it answers 409 "error finding name … in
	// namespace" — so every preview needs its name registered first. Naming a
	// share is the whole point of name_template, so the alternative is ephemeral
	// shares whose hostname changes on every rebuild.
	if err := z.ensureName(ctx, spec.Name); err != nil {
		return nil, err
	}

	shr, err := sdk.CreateShare(z.root, req)
	if err != nil {
		// A name held by a share we do not know about — left behind by a
		// previous process that died without cleaning up — is the one failure
		// worth retrying, because it is self-inflicted and recoverable.
		if reaped := z.reapName(ctx, spec.Name); reaped {
			z.log.Warn("reclaimed orphaned zrok name and retrying", "name", spec.Name)
			shr, err = sdk.CreateShare(z.root, req)
		}
		if err != nil {
			return nil, fmt.Errorf("creating zrok share %q in namespace %q: %w", spec.Name, z.namespace, err)
		}
	}

	listener, err := sdk.NewListener(shr.Token, z.root)
	if err != nil {
		// The share exists but nothing can serve it. Leaving it would burn the
		// name and confuse the next attempt.
		if delErr := sdk.DeleteShare(z.root, shr); delErr != nil {
			z.log.Error("failed to clean up share after listener error", "token", shr.Token, "error", delErr)
		}
		return nil, fmt.Errorf("opening zrok listener for share %s: %w", shr.Token, err)
	}

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			z.log.Error("preview server stopped", "name", spec.Name, "error", err)
		}
	}()

	entry := &zrokShare{
		token:     shr.Token,
		previewID: spec.PreviewID,
		name:      spec.Name,
		listener:  listener,
		server:    srv,
	}
	z.mu.Lock()
	z.live[spec.Key()] = entry
	z.mu.Unlock()

	origin := ""
	if len(shr.FrontendEndpoints) > 0 {
		// The controller reports a bare hostname, not a URL. Without the scheme
		// every preview link in a pull request comment renders as a relative
		// path, so it resolves against github.com and 404s there.
		origin = shr.FrontendEndpoints[0]
		if !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
			origin = "https://" + origin
		}
	} else {
		// Should not happen for a public share, but a preview with no URL is
		// useless and silently returning "" would surface as a broken comment.
		if err := z.close(entry); err != nil {
			z.log.Error("cleanup after missing frontend endpoint failed", "error", err)
		}
		z.mu.Lock()
		delete(z.live, spec.Key())
		z.mu.Unlock()
		return nil, fmt.Errorf("zrok share %s returned no frontend endpoints", shr.Token)
	}

	url := JoinURL(origin, spec.BaseURL)
	z.log.Info("published preview",
		"preview", spec.PreviewID, "build", spec.BuildID,
		"name", spec.Name, "url", url, "token", shr.Token)

	return NewPublication(url, spec.Name, func() error {
		z.withdrawEntry(spec.Key(), entry)
		return nil
	}), nil
}

// withdraw tears down whatever publication currently holds this key.
func (z *Zrok) withdraw(key string) { z.withdrawEntry(key, nil) }

// withdrawEntry tears down a publication. If want is non-nil it must still be
// the live one, or nothing happens.
//
// The daemon replaces a preview by publishing the new one and then closing the
// old Publication, in that order. A close that deleted by key alone would tear
// down its own replacement — and here that means deleting the share on the
// zrok controller, so the preview would not merely 404, it would stop existing.
func (z *Zrok) withdrawEntry(key string, want *zrokShare) {
	z.mu.Lock()
	entry, ok := z.live[key]
	if ok && (want == nil || entry == want) {
		delete(z.live, key)
	} else {
		ok = false
	}
	z.mu.Unlock()
	if !ok {
		return
	}
	if err := z.close(entry); err != nil {
		z.log.Error("withdrawing preview", "publication", key, "error", err)
	}
}

func (z *Zrok) close(entry *zrokShare) error {
	var errs []error

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := entry.server.Shutdown(shutdownCtx); err != nil {
		errs = append(errs, fmt.Errorf("shutting down preview server: %w", err))
	}
	if err := entry.listener.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing overlay listener: %w", err))
	}
	if err := sdk.DeleteShare(z.root, &sdk.Share{Token: entry.token}); err != nil {
		errs = append(errs, fmt.Errorf("deleting share %s: %w", entry.token, err))
	}
	return errors.Join(errs...)
}

// Reap deletes shares owned by this environment whose preview IDs are not in
// keep.
//
// At startup keep is the set of previews the database says should exist, and
// since no listener has been opened yet, every matching share found on the
// controller is dead weight from a previous process. Deleting them is not
// merely tidy: zrok accounts have share limits, and a daemon that leaks a share
// per restart eventually stops working.
func (z *Zrok) Reap(ctx context.Context, keep map[string]bool) error {
	client, err := z.root.Client()
	if err != nil {
		return fmt.Errorf("building zrok client: %w", err)
	}

	envZID := z.root.Environment().ZitiIdentity
	prefix := targetPrefix

	params := metadata.NewListSharesParamsWithContext(ctx)
	params.EnvZID = &envZID
	params.Target = &prefix

	resp, err := client.Metadata.ListShares(params, z.auth())
	if err != nil {
		return fmt.Errorf("listing zrok shares: %w", err)
	}

	z.mu.Lock()
	liveTokens := make(map[string]bool, len(z.live))
	for _, entry := range z.live {
		liveTokens[entry.token] = true
	}
	z.mu.Unlock()

	if resp.Payload == nil {
		return nil
	}

	var errs []error
	for _, shr := range resp.Payload.Shares {
		if shr == nil || liveTokens[shr.ShareToken] {
			continue
		}
		// Defend against the substring filter matching something that merely
		// contains our prefix rather than starting with it.
		if !strings.HasPrefix(shr.Target, prefix) {
			continue
		}
		if keep[strings.TrimPrefix(shr.Target, prefix)] {
			continue
		}
		z.log.Info("reaping orphaned zrok share", "token", shr.ShareToken, "target", shr.Target)
		if err := sdk.DeleteShare(z.root, &sdk.Share{Token: shr.ShareToken}); err != nil {
			errs = append(errs, fmt.Errorf("deleting orphaned share %s: %w", shr.ShareToken, err))
		}
	}
	return errors.Join(errs...)
}

// ensureName registers a name in the namespace, tolerating one that is already
// there.
//
// Idempotent by design rather than by check: asking whether the name exists and
// then creating it is a race against another publish, and the create is the
// cheaper of the two calls anyway. An "already exists" answer is the success
// case, so it is matched on rather than returned.
//
// The name outlives the share bound to it. That is what keeps a preview's URL
// stable across rebuilds and restarts — the same reason the webhook tunnel
// reserves its own name — and it means the account accumulates one name per
// preview name ever published. See the note on Withdraw.
func (z *Zrok) ensureName(ctx context.Context, name string) error {
	client, err := z.root.Client()
	if err != nil {
		return fmt.Errorf("building zrok client: %w", err)
	}

	params := share.NewCreateShareNameParamsWithContext(ctx)
	params.Body = share.CreateShareNameBody{Name: name, NamespaceToken: z.namespace}

	if _, err := client.Share.CreateShareName(params, z.auth()); err != nil {
		if isNameAlreadyExists(err) {
			z.log.Debug("zrok name already registered", "name", name, "namespace", z.namespace)
			return nil
		}
		return fmt.Errorf("registering zrok name %q in namespace %q: %w", name, z.namespace, err)
	}
	z.log.Info("registered zrok name", "name", name, "namespace", z.namespace)
	return nil
}

// isNameAlreadyExists recognises the conflict that means the name is present.
//
// The generated type, not the message: the 409 arrives with an empty body, so
// there is no text to match — `[POST /share/name][409] createShareNameConflict ""`
// is the whole of it. The swagger definition names this response "name already
// exists", which makes the type the only signal there is.
//
// It cannot distinguish a name this account owns from one another account holds.
// Nothing here can, given the empty body. Treating both as success is safe
// because the CreateShare that follows binds the name and fails on its own if it
// is not ours — one call later, with its own error.
func isNameAlreadyExists(err error) bool {
	var conflict *share.CreateShareNameConflict
	return errors.As(err, &conflict)
}

// reapName deletes any docpreview-owned share currently bound to name. It
// reports whether it deleted anything, so Publish knows a retry is worthwhile.
func (z *Zrok) reapName(ctx context.Context, name string) bool {
	client, err := z.root.Client()
	if err != nil {
		return false
	}

	envZID := z.root.Environment().ZitiIdentity
	prefix := targetPrefix

	params := metadata.NewListSharesParamsWithContext(ctx)
	params.EnvZID = &envZID
	params.Target = &prefix

	resp, err := client.Metadata.ListShares(params, z.auth())
	if err != nil || resp.Payload == nil {
		return false
	}

	deleted := false
	for _, shr := range resp.Payload.Shares {
		if shr == nil || !strings.HasPrefix(shr.Target, prefix) {
			continue
		}
		if !endpointsMatchName(shr.FrontendEndpoints, name) {
			continue
		}
		if err := sdk.DeleteShare(z.root, &sdk.Share{Token: shr.ShareToken}); err == nil {
			deleted = true
		}
	}
	return deleted
}

// endpointsMatchName reports whether any frontend endpoint is hosted at the
// given name — that is, whether the hostname's first label equals name.
func endpointsMatchName(endpoints []string, name string) bool {
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

// Close tears down every live publication.
func (z *Zrok) Close() error {
	z.mu.Lock()
	entries := make([]*zrokShare, 0, len(z.live))
	for name, entry := range z.live {
		entries = append(entries, entry)
		delete(z.live, name)
	}
	z.mu.Unlock()

	var errs []error
	for _, entry := range entries {
		if err := z.close(entry); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
