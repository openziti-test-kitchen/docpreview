package expose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
)

// Frontdoor publishes previews through NetFoundry Frontdoor.
//
// Frontdoor and zrok solve the same problem with the same primitives — an
// enrolled agent on the private side, a hardened global frontend, and a share
// mapping a public route onto a private target — but they invert one detail
// that matters here. zrok's SDK hands us a listener on the overlay and we serve
// into it. Frontdoor's agent dials *out* to a target URL, so a Frontdoor
// preview must bind a real TCP port the agent can reach. That is why this type
// embeds the loopback exposer's port machinery instead of reimplementing it,
// and why AgentReachableHost has to be an address the agent can actually
// connect to rather than 127.0.0.1 — unless the agent runs on this same host,
// which is the easy case and the default.
//
// The publish/withdraw shape is identical to zrok's, which is the point of the
// Exposer interface: switching between them is a one-line config change, and
// nothing upstream of this package can tell the difference.
//
// UNVERIFIED: the share endpoint path and payload field names below are
// modelled on the documented Frontdoor API convention
// (/frontdoor/{frontdoorId}/{resource}, bearer auth, JSON bodies) but have not
// been exercised against a live tenant, because Frontdoor is not yet installed
// here. Everything above the wire format — lifecycle, reaping, naming,
// idempotency — is exercised by the same tests as the other exposers. Check the
// field names against the OpenAPI reference before first use; if they differ,
// the fix is confined to shareRequest and shareResponse.
type Frontdoor struct {
	cfg   config.FrontdoorConfig
	log   *slog.Logger
	token TokenFunc

	// ports supplies the local listeners the Frontdoor agent connects back to.
	ports *Local

	http *http.Client

	// live is keyed by preview id, and name records the label each entry took.
	//
	// It was keyed by name, which is the branch — and branch names are not
	// unique across repositories. Four projects each with a `new-install-guide`
	// branch all asked for the same key, so every publish silently withdrew a
	// different project's live share. The same defect was fixed in the ziti
	// exposer, where the name is a hostname; here it is both a map key and the
	// public URL, so it needs the map fixed and the collision refused.
	mu   sync.Mutex
	live map[string]*frontdoorShare
}

type frontdoorShare struct {
	id    string
	name  string
	local *localShare
}

// NewFrontdoor builds a Frontdoor exposer. frontdoorID identifies the tenant's
// Frontdoor instance and token authenticates against the gateway.
func NewFrontdoor(cfg config.FrontdoorConfig, token TokenFunc, log *slog.Logger) (*Frontdoor, error) {
	if cfg.APIBase == "" {
		return nil, errors.New("exposer.frontdoor.api_base must be set")
	}
	if cfg.Frontend == "" {
		return nil, errors.New("exposer.frontdoor.frontend must be set")
	}
	if token == nil {
		return nil, errors.New("frontdoor needs a token provider")
	}

	ports := NewLocal(log, "")
	if cfg.AgentReachableHost != "" {
		ports.host = cfg.AgentReachableHost
	}

	return &Frontdoor{
		cfg:   cfg,
		log:   log.With("exposer", "frontdoor"),
		token: token,
		ports: ports,
		http:  &http.Client{Timeout: 30 * time.Second},
		live:  map[string]*frontdoorShare{},
	}, nil
}

func (f *Frontdoor) Kind() string { return "frontdoor" }

// Validate confirms the gateway accepts our token and knows the configured
// frontend.
func (f *Frontdoor) Validate(ctx context.Context) error {
	var out struct {
		Embedded struct {
			Shares []shareResponse `json:"shares"`
		} `json:"_embedded"`
	}
	if err := f.do(ctx, http.MethodGet, "/shares", nil, &out); err != nil {
		return fmt.Errorf("frontdoor gateway rejected this token: %w", err)
	}
	f.log.Info("frontdoor validated", "api", f.cfg.APIBase, "frontend", f.cfg.Frontend)
	return nil
}

// shareRequest is the create-share payload. See the UNVERIFIED note on
// Frontdoor.
type shareRequest struct {
	Name      string `json:"name"`
	Frontend  string `json:"frontend"`
	TargetURL string `json:"targetUrl"`

	// Tag carries the preview ID so Reap can recognize its own work, mirroring
	// the Target-prefix trick used against zrok.
	Tag string `json:"tag,omitempty"`
}

type shareResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
	Tag string `json:"tag"`
}

// Publish binds a local port, then asks Frontdoor to route a public URL to it.
func (f *Frontdoor) Publish(ctx context.Context, spec Spec, h http.Handler) (*Publication, error) {
	// Replaces this publication's own share and nothing else.
	f.withdraw(ctx, spec.Key())

	// The name is the public hostname, so a second preview taking it would
	// point somebody else's URL at these artifacts. Refuse rather than
	// overwrite: a name_template that collides is a configuration mistake, and
	// silently serving the wrong site is the worst way to report one.
	// Two builds of one preview under one name is this preview's earlier build of the
	// same commit, and the newer one takes the name — collected here and withdrawn
	// below, because withdraw takes the lock itself.
	var superseded []string
	f.mu.Lock()
	for id, entry := range f.live {
		if entry.name != spec.Name || id == spec.Key() {
			continue
		}
		if Collides(id, spec) {
			f.mu.Unlock()
			return nil, fmt.Errorf("the name %q is already serving a different preview (%s); "+
				"two previews render to the same name under this name_template — "+
				"use \"{{.Repo.Name}}-{{.Name}}\" to separate them", spec.Name, id)
		}
		superseded = append(superseded, id)
	}
	f.mu.Unlock()

	for _, id := range superseded {
		f.withdraw(ctx, id)
	}

	local, err := f.ports.serve(h)
	if err != nil {
		return nil, err
	}

	body := shareRequest{
		Name:      spec.Name,
		Frontend:  f.cfg.Frontend,
		TargetURL: fmt.Sprintf("http://%s:%d", f.ports.host, local.port),
		Tag:       targetPrefix + spec.Key(),
	}

	var created shareResponse
	if err := f.do(ctx, http.MethodPost, "/shares", body, &created); err != nil {
		// Nothing can reach the port we just bound, so give it back.
		if cerr := closeLocal(local); cerr != nil {
			f.log.Error("cleanup after failed share creation", "error", cerr)
		}
		return nil, fmt.Errorf("creating frontdoor share %q: %w", spec.Name, err)
	}

	// Validate the response before treating the publish as successful.
	//
	// This is the guard that makes an unverified wire format safe to ship.
	// encoding/json does not error on fields that are absent from the payload;
	// it leaves them at their zero value and reports success. So if the field
	// names in shareResponse are wrong, every publish "succeeds" with an empty
	// ID and an empty URL: the pull request gets a comment linking to "/", the
	// local listener is orphaned, and cleanup issues DELETE /shares/ with no ID
	// and 404s, leaking the remote share.
	//
	// Failing here instead turns a guessed field name into one clear error that
	// names the two structs to fix, at the first publish rather than three
	// layers downstream disguised as success.
	if created.ID == "" || created.URL == "" {
		if cerr := closeLocal(local); cerr != nil {
			f.log.Error("cleanup after malformed share response", "error", cerr)
		}
		// Best effort: if an ID did come back, the share exists remotely and
		// would otherwise be unreachable and unreapable.
		if created.ID != "" {
			if derr := f.deleteShare(ctx, created.ID); derr != nil {
				f.log.Error("could not delete the half-created share",
					"id", created.ID, "error", derr)
			}
		}
		return nil, fmt.Errorf("creating frontdoor share %q: response had no id or url "+
			"(check shareRequest/shareResponse in internal/expose/frontdoor.go "+
			"against the Frontdoor OpenAPI reference)", spec.Name)
	}

	entry := &frontdoorShare{id: created.ID, name: spec.Name, local: local}
	f.mu.Lock()
	f.live[spec.Key()] = entry
	f.mu.Unlock()

	url := JoinURL(created.URL, spec.BaseURL)
	f.log.Info("published preview",
		"preview", spec.PreviewID, "build", spec.BuildID,
		"name", spec.Name, "url", url, "share", created.ID)

	return NewPublication(url, spec.Name, func() error {
		f.withdrawEntry(context.Background(), spec.Key(), entry)
		return nil
	}), nil
}

// withdraw tears down whatever share this preview currently has.
func (f *Frontdoor) withdraw(ctx context.Context, previewID string) {
	f.withdrawEntry(ctx, previewID, nil)
}

// withdrawEntry tears down a share. If want is non-nil it must still be the
// live one, or nothing happens.
//
// The daemon publishes the replacement before closing the old Publication, so a
// close that deleted by key alone would delete the remote share its own
// replacement had just created.
func (f *Frontdoor) withdrawEntry(ctx context.Context, previewID string, want *frontdoorShare) {
	f.mu.Lock()
	entry, ok := f.live[previewID]
	if ok && (want == nil || entry == want) {
		delete(f.live, previewID)
	} else {
		ok = false
	}
	f.mu.Unlock()
	if !ok {
		return
	}
	if err := f.close(ctx, entry); err != nil {
		f.log.Error("withdrawing preview", "preview", previewID, "error", err)
	}
}

func (f *Frontdoor) close(ctx context.Context, entry *frontdoorShare) error {
	var errs []error
	if err := f.deleteShare(ctx, entry.id); err != nil {
		errs = append(errs, err)
	}
	if err := closeLocal(entry.local); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// deleteShare removes one remote share.
//
// The empty-id check is not paranoia about our own state — Publish refuses to
// record a share without an id. It guards the path where ids arrive from the
// controller's share listing, which is decoded with the same unverified field
// names. Without it, "DELETE /shares/" + "" addresses the *collection*, and on
// an API where that means "delete every share" this would be a very bad
// afternoon.
func (f *Frontdoor) deleteShare(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("refusing to delete a share with no id: " +
			"the controller returned a share without one, which means shareResponse's " +
			"field names do not match the API")
	}
	if err := f.do(ctx, http.MethodDelete, "/shares/"+url.PathEscape(id), nil, nil); err != nil {
		return fmt.Errorf("deleting frontdoor share %s: %w", id, err)
	}
	return nil
}

// Reap deletes docpreview-tagged shares whose preview IDs are not in keep.
func (f *Frontdoor) Reap(ctx context.Context, keep map[string]bool) error {
	var out struct {
		Embedded struct {
			Shares []shareResponse `json:"shares"`
		} `json:"_embedded"`
	}
	if err := f.do(ctx, http.MethodGet, "/shares", nil, &out); err != nil {
		return fmt.Errorf("listing frontdoor shares: %w", err)
	}

	f.mu.Lock()
	liveIDs := make(map[string]bool, len(f.live))
	for _, entry := range f.live {
		liveIDs[entry.id] = true
	}
	f.mu.Unlock()

	var errs []error
	for _, shr := range out.Embedded.Shares {
		if liveIDs[shr.ID] || !strings.HasPrefix(shr.Tag, targetPrefix) {
			continue
		}
		if keep[strings.TrimPrefix(shr.Tag, targetPrefix)] {
			continue
		}
		f.log.Info("reaping orphaned frontdoor share", "id", shr.ID, "tag", shr.Tag)
		if err := f.deleteShare(ctx, shr.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close tears down every live publication.
func (f *Frontdoor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f.mu.Lock()
	entries := make([]*frontdoorShare, 0, len(f.live))
	for name, entry := range f.live {
		entries = append(entries, entry)
		delete(f.live, name)
	}
	f.mu.Unlock()

	var errs []error
	for _, entry := range entries {
		if err := f.close(ctx, entry); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// do issues an authenticated request against the Frontdoor gateway.
func (f *Frontdoor) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		body = bytes.NewReader(raw)
	}

	// Resolved per request, not once at construction. The token lives in a vault
	// that may still be locked when the daemon starts, and fetching it during
	// wiring is what made exposer.kind: frontdoor refuse to boot until somebody
	// had already unlocked the vault from a page this daemon would not serve.
	tok, err := f.token()
	if err != nil {
		return err
	}

	url := strings.TrimRight(f.cfg.APIBase, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.RevealString())
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := f.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// Cap the body: an error page from a misconfigured proxy can be
		// megabytes, and none of it belongs in a log line.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(snippet)))
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}
