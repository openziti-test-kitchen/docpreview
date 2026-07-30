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

	// A publication of this same preview under this same name is this preview's
	// earlier build, and the newer build takes the name from it — see
	// expose.Collides. Collected under the lock and withdrawn after it, because
	// withdraw takes the lock itself.
	var superseded []string
	z.mu.Lock()
	for id, entry := range z.live {
		if entry.name != spec.Name || id == spec.Key() {
			continue
		}
		if Collides(id, spec) {
			z.mu.Unlock()
			return nil, fmt.Errorf("the name %q is already serving a different preview (%s); "+
				"two previews render to the same name under this name_template — "+
				"use \"{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}\" to separate them",
				spec.Name, id)
		}
		superseded = append(superseded, id)
	}
	z.mu.Unlock()

	for _, id := range superseded {
		z.log.Info("replacing an earlier build's share of this name",
			"exposer", "zrok2", "name", spec.Name, "superseded", id, "publication", spec.Key())
		z.withdraw(id)
	}

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

	// Retried on a controller timeout, and this one is not free.
	//
	// The deadline is the SDK client's, so a timed-out create may well have succeeded on
	// the controller — in which case the retry creates a second share and the first is an
	// orphan holding nothing. That is recoverable: it carries the `docpreview:` target
	// prefix, so the next startup's Reap collects it, and the newer share is the one
	// bound to the name. The alternative is what this replaced — a preview that built
	// successfully, has its artifacts on disk, and is not served because one HTTP request
	// was slow. A leaked share until the next restart is the cheaper failure.
	shr, err := func() (*sdk.Share, error) {
		var out *sdk.Share
		err := z.retryTransient(ctx, "create share "+spec.Name, func() error {
			var attempt error
			out, attempt = sdk.CreateShare(z.root, req)
			return attempt
		})
		return out, err
	}()
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

// Adoptable lists the shares this environment already owns, keyed by publication key.
//
// One ListShares call, which is the same call Reap makes — so a startup that reaps and
// then adopts pays for two listings rather than one per candidate. Worth keeping them
// separate anyway: Reap decides what to delete and this decides what to keep, and
// merging them into one pass that does both is how a keep-set bug deletes live shares.
func (z *Zrok) Adoptable(ctx context.Context) (map[string]Adoptable, error) {
	client, err := z.root.Client()
	if err != nil {
		return nil, fmt.Errorf("building zrok client: %w", err)
	}

	envZID := z.root.Environment().ZitiIdentity
	prefix := targetPrefix

	params := metadata.NewListSharesParamsWithContext(ctx)
	params.EnvZID = &envZID
	params.Target = &prefix

	resp, err := client.Metadata.ListShares(params, z.auth())
	if err != nil {
		return nil, fmt.Errorf("listing zrok shares: %w", err)
	}
	if resp.Payload == nil {
		return map[string]Adoptable{}, nil
	}

	out := make(map[string]Adoptable, len(resp.Payload.Shares))
	for _, shr := range resp.Payload.Shares {
		if shr == nil || !strings.HasPrefix(shr.Target, prefix) {
			continue
		}
		// No endpoint, no adoption. The URL goes into a pull request comment, so a
		// reconstructed one — guessing the frontend's DNS suffix — is a link that
		// works until the day it does not.
		if len(shr.FrontendEndpoints) == 0 {
			continue
		}
		origin := shr.FrontendEndpoints[0]
		if !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
			origin = "https://" + origin
		}
		out[strings.TrimPrefix(shr.Target, prefix)] = Adoptable{Handle: shr.ShareToken, Origin: origin}
	}
	return out, nil
}

// Adopt binds an overlay listener to a share that already exists and serves h on it.
//
// Everything Publish does except the two controller calls: no name to register — the
// share already holds it — and no share to create. What is left is the listener and the
// HTTP server, which are the parts that genuinely died with the previous process.
func (z *Zrok) Adopt(ctx context.Context, spec Spec, a Adoptable, h http.Handler) (*Publication, error) {
	if a.Handle == "" || a.Origin == "" {
		return nil, fmt.Errorf("adopting %s: incomplete candidate", spec.Key())
	}

	// Same guard as Publish, for the same reason: two previews rendering to one name
	// must be refused rather than resolved, and adoption is not an exception — the
	// share being adopted holds the name either way. Two builds of one preview under
	// one name is not that case, and the newer one takes it.
	var superseded []string
	z.mu.Lock()
	for id, entry := range z.live {
		if entry.name != spec.Name || id == spec.Key() {
			continue
		}
		if Collides(id, spec) {
			z.mu.Unlock()
			return nil, fmt.Errorf("the name %q is already serving a different preview (%s)", spec.Name, id)
		}
		superseded = append(superseded, id)
	}
	z.mu.Unlock()

	for _, id := range superseded {
		z.withdraw(id)
	}

	listener, err := sdk.NewListener(a.Handle, z.root)
	if err != nil {
		// Deliberately not deleted. Publish deletes a share it cannot serve because it
		// had just created it; this one predates the process and something else may yet
		// serve it. The caller falls back to Publish, which replaces it by name.
		return nil, fmt.Errorf("opening zrok listener for existing share %s: %w", a.Handle, err)
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
		token:     a.Handle,
		previewID: spec.PreviewID,
		name:      spec.Name,
		listener:  listener,
		server:    srv,
	}
	z.mu.Lock()
	z.live[spec.Key()] = entry
	z.mu.Unlock()

	url := JoinURL(a.Origin, spec.BaseURL)
	z.log.Info("adopted preview",
		"preview", spec.PreviewID, "build", spec.BuildID,
		"name", spec.Name, "url", url, "token", a.Handle)

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

// close stops serving a share and deletes it from the controller.
//
// It must never touch the *name* bound to the share. Three callers reach here and
// only one of them is a teardown: Publish's own withdraw on a rebuild, the
// Publication's Close when the daemon supersedes a build, and daemon shutdown. In
// two of the three the name is the reviewer's stable URL and has to survive — so
// releasing the name here would silently rehost every rebuilt preview while the
// pull request comment went on advertising the old address. That is worse than the
// leak it would fix, and much harder to notice.
//
// Releasing is therefore the daemon's decision, made once, in teardown, through
// ReleaseName. See NameReleaser.
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

	// The doomed set, decided before anything is deleted.
	var doomed []string
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
		doomed = append(doomed, shr.ShareToken)
	}

	// Deleted concurrently, bounded.
	//
	// Serially this was the slowest thing the daemon does: each DeleteShare is a client
	// construction, a version check and the call itself — five to fifteen seconds against
	// the hosted controller — and startup does not finish, so no worker starts and no
	// queued build runs, until every one of them has returned. Nine leftover build shares
	// meant minutes of a daemon that looked started and would not build anything, which
	// read as a stuck queue every time.
	//
	// Safe to parallelise because each deletion is independent: a share token is only ever
	// in this list once, nothing here reads shared state after the list is built, and the
	// controller is the serialisation point. What is *not* safe is overlapping this with
	// the republish that follows — see the reap-before-republish rule in
	// docs/design/02-exposers.md — and that ordering is unchanged.
	//
	// Eight at a time: enough to turn minutes into seconds, few enough not to look like an
	// attack to a controller that rate-limits.
	const parallel = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sem  = make(chan struct{}, parallel)
	)
	for _, token := range doomed {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Retried, because a share this fails to delete is an orphan that survives
			// into the next run: its name stays bound, so the next publish under that
			// name has to reclaim it, and it counts against the account either way.
			// Deleting an already-deleted share is harmless, which is what makes the
			// retry safe here and delicate on the create path.
			var attempts int
			err := z.retryTransient(ctx, "unshare "+token, func() error {
				attempts++
				err := sdk.DeleteShare(z.root, &sdk.Share{Token: token})
				// A 404 on a retry means the attempt that timed out actually worked.
				//
				// The deadline is the client's, not the controller's, so a timed-out
				// delete has usually already happened — measured live: every retried
				// unshare came back "unshareNotFound", and reporting those as failures
				// turned four successful deletions into four startup errors. Only on a
				// retry: a 404 on the *first* attempt means the share was already gone
				// when the reap listed it, which is worth knowing about.
				if attempts > 1 && isNotFound(err) {
					return nil
				}
				return err
			})
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("deleting orphaned share %s: %w", token, err))
				mu.Unlock()
			}
		}(token)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// transient reports whether a zrok controller error is worth trying again.
//
// The hosted controller times a request out under load — `Post
// "https://api-v2.zrok.io/api/v2/share": context deadline exceeded` — and the SDK's
// CreateShare takes no context, so the deadline is its own HTTP client's and cannot be
// lengthened from here. Retrying is the only lever available.
//
// Matched on the message because that is what the SDK returns: these arrive as a
// url.Error wrapping the client's deadline, not as a typed controller error, and the
// 4xx answers that must *not* be retried are typed. So an unrecognised error is not
// retried, which is the safe default — a permission failure or a quota refusal retried
// three times is three times the same refusal.
func transient(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{
		"context deadline exceeded",
		"Client.Timeout",
		"connection reset",
		"unexpected EOF",
		"TLS handshake timeout",
		"i/o timeout",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// isNotFound recognises the controller's answer for a share that is not there.
//
// Matched on the rendered error for the same reason `transient` is: the SDK's
// DeleteShare returns the runtime's formatted "[DELETE /unshare][404] unshareNotFound"
// rather than a typed value that can be asserted on.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "[404]")
}

// zrokBackoff is the wait before each retry. Three attempts, and the gaps are seconds
// rather than milliseconds: the failure being retried is a controller that did not
// answer in time, so retrying immediately asks the same overloaded thing the same
// question.
var zrokBackoff = []time.Duration{2 * time.Second, 6 * time.Second}

// retryTransient runs fn, trying again while it fails in a way that looks like the
// controller rather than the request.
//
// ctx is honoured between attempts, so a shutdown does not sit through the backoff.
func (z *Zrok) retryTransient(ctx context.Context, what string, fn func() error) error {
	err := fn()
	for i, wait := range zrokBackoff {
		if !transient(err) {
			return err
		}
		z.log.Warn("zrok call timed out, retrying", "call", what,
			"attempt", i+1, "in", wait, "error", err)
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
		err = fn()
	}
	return err
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

// isNameAlreadyExists recognises the one conflict that means the name is present.
//
// Four situations answer 409 from POST /share/name and only one of them is the
// success case being looked for. Reading the controller, the discriminator is
// whether the response carries a payload:
//
//	empty      the name already exists                     <- success
//	non-empty  "names limit reached; cannot reserve additional names"
//	non-empty  "… is not a valid share name; failed profanity or DNS check"
//	non-empty  a stale-frontend-mapping conflict
//
// All four arrive as *share.CreateShareNameConflict, so matching the type alone —
// which this did — reported a registered name to an account that had hit its
// reserved-name quota. The CreateShare that followed then failed for a reason that
// never mentioned quotas, which is a bad afternoon.
//
// One name per commit reaches a name limit far sooner than one per branch did, so
// this went from theoretical to likely the moment builds got their own shares.
//
// It still cannot tell a name this account owns from one another account holds:
// nothing can, given the empty body. Treating both as success is safe because the
// CreateShare that follows binds the name and fails on its own if it is not ours.
func isNameAlreadyExists(err error) bool {
	var conflict *share.CreateShareNameConflict
	return errors.As(err, &conflict) && conflict.Payload == ""
}

// ReleaseName gives up a reserved name, which is the other half of ensureName and
// was missing for long enough to leak one name per branch — and, once builds got
// their own shares, one per commit.
//
// # Why this is not part of withdrawing a share
//
// A name deliberately outlives the share bound to it: that is what keeps a preview's
// URL stable across rebuilds and restarts, and every republish depends on it. So this
// is called from teardown, when the pull request is gone and the URL is never coming
// back — never from the withdraw that replaces a preview's share on a rebuild. See
// the comment on close.
//
// # Two steps, in this order, and the order is what makes it safe
//
// Clearing the reserved flag first is the load-bearing part. The controller's unshare
// already deletes any name bound to the share it is deleting whose reserved flag is
// false (controller/unshare.go, cleanupShareNameMappings) — createShareName hardcodes
// Reserved: true, and that one line is the whole leak. So a de-reserved name is
// something the platform will collect on its own.
//
// That makes this self-healing. Crash between de-reserving and withdrawing and the
// next startup's Reap deletes the share, and the name goes with it. An explicit
// delete that never runs leaks silently and forever.
//
// The DELETE that follows is the mop-up for a name with no live share to reap it: a
// preview whose share was already gone, or one restored after a restart whose
// publish failed. When a share *is* still bound the controller answers 409 and that
// is the expected, successful path — unshare will finish the job.
//
// Both orders therefore work, so callers may release before or after withdrawing. It
// is called before, so the flag is already clear if the withdraw is the thing that
// fails.
func (z *Zrok) ReleaseName(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}

	client, err := z.root.Client()
	if err != nil {
		return fmt.Errorf("building zrok client: %w", err)
	}

	// Reserved is deliberately left at its zero value rather than written out: the
	// generated body tags it `omitempty`, so false is not serialized at all, and the
	// controller reads an absent flag as false. Setting it explicitly would be the
	// same request. Do not "fix" this by inventing a pointer field.
	upd := share.NewUpdateShareNameParamsWithContext(ctx)
	upd.Body = share.UpdateShareNameBody{Name: name, NamespaceToken: z.namespace}

	if _, err := client.Share.UpdateShareName(upd, z.auth()); err != nil {
		var gone *share.UpdateShareNameNotFound
		if errors.As(err, &gone) {
			// Already deleted, which is what was wanted. Treating it as a failure
			// would log one on every second teardown.
			z.log.Debug("zrok name was already gone", "name", name, "namespace", z.namespace)
			return nil
		}
		return fmt.Errorf("de-reserving zrok name %q in namespace %q "+
			"(it stays counted against the account's name limit until it is): %w",
			name, z.namespace, err)
	}

	del := share.NewDeleteShareNameParamsWithContext(ctx)
	del.Body = share.DeleteShareNameBody{Name: name, NamespaceToken: z.namespace}

	if _, err := client.Share.DeleteShareName(del, z.auth()); err != nil {
		var bound *share.DeleteShareNameConflict
		if errors.As(err, &bound) {
			z.log.Debug("zrok name is no longer reserved and still has a share; "+
				"the controller deletes it when that share is withdrawn",
				"name", name, "namespace", z.namespace)
			return nil
		}
		var gone *share.DeleteShareNameNotFound
		if errors.As(err, &gone) {
			return nil
		}
		return fmt.Errorf("deleting zrok name %q in namespace %q: %w", name, z.namespace, err)
	}

	z.log.Info("released zrok name", "name", name, "namespace", z.namespace)
	return nil
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
