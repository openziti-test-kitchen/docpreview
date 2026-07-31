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
	"slices"
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
// # What is known about the wire format, and what is not
//
// The create payload's field names are read off NetFoundry's own documented
// example (https://netfoundry.io/docs/frontdoor/learn/shares/http-shares/) and
// the route shape off the API guides — see shareRequest and
// checkFrontdoorAPIBase. They were previously inferred, and the inference was
// wrong in four places at once: the path omitted the required frontdoor ID
// segment, the target was sent as `targetUrl` rather than `target`, the frontend
// as a single name rather than a `frontendIds` array, and `type` was not sent at
// all. Nothing here has still been exercised against a live tenant, because
// Frontdoor is not installed on this machine — so "documented" is the strongest
// claim available, and two documented-but-unobserved things (the response field
// names and the listing envelope) are read through alternates rather than
// trusted.
//
// The response shapes are therefore guarded rather than assumed. A create
// response this code cannot read fails the publish naming the structs to fix (see
// Publish), and a share listing this code cannot read fails Validate naming the
// keys (see listShares) — the listing gets its own guard because its failure is
// silent and unbounded rather than merely wrong.
//
// One documented fact is load-bearing and good news: a standard frontend serves a
// share at "https://{share-name}.shares.netfoundry.io"
// (https://netfoundry.io/docs/frontdoor/learn/frontends/), so the public URL is
// derived from the name docpreview chose. Recreating a share under the same name
// returns the same URL, which is what makes a restart quiet instead of editing
// every open pull request's comment. See shareResponse.PublicURL.
//
// Everything above the wire format — lifecycle, reaping, naming, idempotency — is
// exercised by the same tests as the other exposers.
//
// # The one thing that stops a first publish
//
// The API requires `envZId`, the ziti identity of the agent that will host the
// share, and FrontdoorConfig has no field for it. Until it does, every publish is
// refused by the gateway; Validate says so once per boot. See shareRequest.EnvZID.
//
// This type deliberately does not implement Adopter, and that is the largest
// functional gap against zrok. Adoption is what took a zrok restart from four and
// a half minutes of 404ing previews to three seconds, and the trick is that only
// the overlay listener dies with the process while the share survives. Here the
// share survives too — but it points at an ephemeral local port that the next
// process will not be given again, so taking it over means rewriting the share's
// target, and there is no verified endpoint for that. See
// docs/design/17-exposer-frontdoor.md.
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
	if err := checkFrontdoorAPIBase(cfg.APIBase); err != nil {
		return nil, err
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

// checkFrontdoorAPIBase refuses an api_base with no frontdoor ID in it.
//
// Every documented Frontdoor route is scoped to a frontdoor instance:
// `POST /frontdoor/{frontdoorId}/shares`,
// `GET /frontdoor/{frontdoorId}/auth-providers/{id}`, and so on — the paths are
// not versioned, the ID segment is what they carry instead
// (https://netfoundry.io/docs/frontdoor/learn/shares/http-shares/,
// https://netfoundry.io/docs/frontdoor/reference/api-guides/health-checks/).
//
// This exposer builds requests as api_base + "/shares", so the ID has to be the
// tail of api_base. config's default is
// "https://gateway.production.netfoundry.io/frontdoor", which produces
// `POST /frontdoor/shares` — a route that does not exist, so every call 404s and
// every publish fails with a message about a path rather than about the setting
// that is wrong.
//
// Checked here rather than in config.validate because this is the only place that
// knows how the URL is assembled, and because config.validate checks nothing
// inside FrontdoorConfig at all today.
func checkFrontdoorAPIBase(base string) error {
	trimmed := strings.TrimRight(base, "/")
	if last := trimmed[strings.LastIndexByte(trimmed, '/')+1:]; last == "frontdoor" {
		return fmt.Errorf("exposer.frontdoor.api_base is %q, which has no frontdoor ID in it; "+
			"Frontdoor's routes are scoped to one — POST /frontdoor/{frontdoorId}/shares — so "+
			"append yours, as in "+
			"api_base: https://gateway.production.netfoundry.io/frontdoor/<your-frontdoor-id>", base)
	}
	return nil
}

// Validate confirms the gateway accepts our token and answers with a share
// listing this code can read.
//
// It does *not* confirm the configured frontend exists — it used to claim it
// did. Nothing here knows how to ask: `frontend` is a name docpreview sends and
// never reads back, so a typo in it is discovered by the first publish at the
// earliest and by a reviewer opening a dead link at the latest. Checking it needs
// whatever the API's frontend-listing endpoint turns out to be, which is one of
// the questions a live tenant has to answer.
//
// The listing shape is checked, and that is the part worth doing at startup: see
// listShares for what goes wrong when it is wrong.
func (f *Frontdoor) Validate(ctx context.Context) error {
	shares, err := f.listShares(ctx)
	if err != nil {
		return fmt.Errorf("frontdoor gateway rejected this token, or answered in a shape "+
			"this exposer cannot read: %w", err)
	}

	// Warned once per boot rather than refused, because refusing would mean this
	// exposer cannot be constructed at all and the daemon would not start — which
	// is the boot-order bug that was already fixed once here. A warning at startup
	// plus a failed publish is the same shape the GitHub client uses for a
	// credential that is not there yet.
	//
	// Conditional on the field being empty, now that there is a field. A warning that
	// cannot be silenced by fixing what it complains about is one operators learn to
	// scroll past — which is what this was for as long as env_z_id did not exist.
	if f.cfg.EnvZID == "" {
		f.log.Warn("frontdoor cannot publish yet: every share needs envZId, the ziti identity " +
			"of the agent that will host it; set exposer.frontdoor.env_z_id " +
			"(the enrolled agent's identity, from the Frontdoor console)")
	}

	f.log.Info("frontdoor validated",
		"api", f.cfg.APIBase, "frontend", f.cfg.Frontend, "shares", len(shares))
	return nil
}

// shareCollectionKeys are the object keys a share listing might carry its array
// under, in the order they are tried.
//
// "content" is the documented one and is therefore first: the gateway answers
// Spring Data REST envelopes —
// `{"content":[…],"pageable":{…},"totalElements":2,"totalPages":1}` — when asked
// for `application/json`, which is what this client's Accept header asks for
// (https://netfoundry.io/docs/frontdoor/reference/api-guides/auth-providers/).
// The same guide documents `Accept: application/hal+json or application/json`, and
// under hal+json the collection moves to `_embedded.shareList` — hence
// "shareList", and hence the "_embedded" descent in decodeShareListing. A proxy
// or a gateway upgrade that changes which representation comes back should not
// break orphan collection.
//
// The rest are fallbacks for shapes this API family plausibly uses. Accepting
// several is not indecision: one of them is right, guessing which costs a leak
// that nothing reports (see listShares), and the cost of accepting the others is
// that a listing arrives under an unexpected key and works anyway.
var shareCollectionKeys = []string{"content", "shareList", "shares", "data", "items", "results"}

// listShares fetches every share the tenant holds.
//
// # Why this is stricter than it looks
//
// Publish already refuses a create response it cannot read, because a wrong field
// name there produces a pull request comment linking to "/". The listing had no
// such guard, and its failure is quieter and worse: if the collection does not
// arrive under `_embedded.shares` — which is what this decoded, and which was a
// guess — then every listing decodes to zero shares and reports success. Validate
// passes. Reap finds nothing to reap, every restart, forever, while each restart
// creates a fresh share per preview. The tenant fills with orphans that nothing
// claims and nothing looks for, and the first symptom is a quota refusal on a
// publish months later, which reads as a Frontdoor problem rather than a field
// name in this file.
//
// So an object that carries keys but none of the recognised ones is an error
// rather than an empty list. An empty object is not: a tenant with no shares is
// the ordinary state on day one, and failing Validate for a correctly configured
// tenant would be a worse trade than the one being made.
// # Why this pages
//
// The listing is paginated, Spring Data REST style: `GET …?page=0&size=20&sort=…`
// answering `{"content":[…],"pageable":{…},"totalElements":2,"totalPages":1}`
// (https://netfoundry.io/docs/frontdoor/reference/api-guides/auth-providers/).
// The documented default size is 20. This asked for no page at all, so it saw at
// most the first page — and Reap deletes what the listing does not contain, so
// every orphan past the twentieth share was invisible and permanent. Frontdoor is
// the exposer most likely to exceed twenty: a preview holds one share for the
// branch plus one per kept build, so `keep_builds: 10` reaches twenty shares at
// the second pull request.
//
// Paged forward until a page comes back *empty* — not merely shorter than the
// size asked for — and never using totalPages. Two reasons, both about not
// trusting this API more than it has earned. totalPages is only present in the
// non-HAL envelope, and which envelope arrives is a function of content
// negotiation. And a gateway is free to cap the page size below what was asked
// for, so a short page does not prove it is the last one; stopping there would
// silently truncate the listing, and a truncated listing means an orphan Reap can
// never see. The cost is one extra round trip per sweep, against a leak that
// nothing reports.
func (f *Frontdoor) listShares(ctx context.Context) ([]shareResponse, error) {
	const pageSize = 200

	var shares []shareResponse
	for page := 0; ; page++ {
		var raw json.RawMessage
		path := fmt.Sprintf("/shares?page=%d&size=%d", page, pageSize)
		if err := f.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, err
		}

		batch, err := decodeShareListing(raw)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		shares = append(shares, batch...)

		// A gateway that ignores the page parameter would otherwise return the
		// same full page forever. Bounded rather than trusted: forty thousand
		// shares is far past anything real, and looping until the process is
		// killed is not a failure mode worth leaving available.
		if page > 200 {
			return nil, fmt.Errorf("the frontdoor share listing did not terminate after %d pages of %d; "+
				"the gateway is ignoring the page parameter", page, pageSize)
		}
	}

	// A share with no id is a share Reap cannot delete, and every share has one.
	// Checked across the whole listing rather than per entry because one entry
	// missing an id would be strange and all of them missing one is a field name:
	// naming the struct is only useful advice in the second case.
	if len(shares) > 0 {
		withID := 0
		for _, shr := range shares {
			if shr.ID() != "" {
				withID++
			}
		}
		if withID == 0 {
			return nil, fmt.Errorf("the frontdoor gateway listed %d shares and none of them had an id "+
				"(check shareResponse's field names in internal/expose/frontdoor.go "+
				"against the Frontdoor API reference)", len(shares))
		}
	}
	return shares, nil
}

// decodeShareListing pulls the share array out of whichever envelope it arrived
// in. See shareCollectionKeys.
func decodeShareListing(raw []byte) ([]shareResponse, error) {
	body := bytes.TrimSpace(raw)
	if len(body) == 0 {
		return nil, errors.New("the frontdoor gateway answered GET /shares with an empty body; " +
			"either the path is wrong or exposer.frontdoor.api_base points at something " +
			"that is not the Frontdoor gateway")
	}

	if body[0] == '[' {
		var out []shareResponse
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("decoding the frontdoor share listing: %w", err)
		}
		return out, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("the frontdoor share listing was neither an array nor an object: %w", err)
	}

	// HAL nests the collection one level down. Descend before looking for a key,
	// so "_embedded" plus a pagination sibling behaves the same as "_embedded"
	// alone.
	if inner, ok := obj["_embedded"]; ok {
		var embedded map[string]json.RawMessage
		if err := json.Unmarshal(inner, &embedded); err != nil {
			return nil, fmt.Errorf(`the frontdoor share listing's "_embedded" was not an object: %w`, err)
		}
		obj = embedded
	}

	for _, key := range shareCollectionKeys {
		if v, ok := obj[key]; ok {
			var out []shareResponse
			if err := json.Unmarshal(v, &out); err != nil {
				return nil, fmt.Errorf("the frontdoor share listing's %q was not an array of shares: %w", key, err)
			}
			return out, nil
		}
	}

	// No recognised key and nothing else either: a tenant with no shares.
	if len(obj) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return nil, fmt.Errorf("the frontdoor share listing carried none of the share arrays this exposer "+
		"recognises (%s) — it had %s. Add the right key to shareCollectionKeys in "+
		"internal/expose/frontdoor.go; until then Reap cannot see an orphaned share and the "+
		"tenant will accumulate one per preview per restart",
		strings.Join(shareCollectionKeys, ", "), strings.Join(keys, ", "))
}

// shareRequest is the create-share payload.
//
// The field names come from the documented example on
// https://netfoundry.io/docs/frontdoor/learn/shares/http-shares/, which is:
//
//	curl -s -X POST -H "Authorization: Bearer $TOKEN" \
//	  -H "Content-Type: application/json" \
//	  -d '{"type":"http","name":"publicdemo","envZId":"ijcrWb-ZOq",
//	       "frontendIds":["bMTHPrtQ"],"target":"http://backend.svc.cluster.local:8080"}' \
//	  "https://gateway.production.netfoundry.io/frontdoor/$FRONTDOOR_ID/shares"
//
// Three of these were wrong before that example was read: the target was sent as
// `targetUrl`, the frontend as a single `frontend` name, and `type` was not sent
// at all. The first two would have been rejected or silently ignored, which for
// `target` means a share pointing nowhere.
type shareRequest struct {
	// Type is "http" for a share fronting an HTTP target, which is all docpreview
	// publishes. Not defaulted server-side as far as the documentation says, and
	// omitting it was one of the three original wire-format defects.
	Type string `json:"type"`

	Name string `json:"name"`

	// EnvZID is the ziti identity of the enrolled agent that will host this share
	// — the agent that dials `Target`. Required by the API.
	//
	// THERE IS NO CONFIG FIELD FOR IT, so it is always empty and every publish
	// will be refused. This is the one thing standing between this exposer and a
	// first successful publish, and it cannot be fixed from inside this file:
	// FrontdoorConfig needs an `env_z_id` setting, and Validate warns about the
	// gap once per boot until it exists. See docs/design/17-exposer-frontdoor.md.
	EnvZID string `json:"envZId,omitempty"`

	// FrontendIDs names the frontends that will serve this share, by ID — values
	// like "bMTHPrtQ", not names. config.FrontdoorConfig.Frontend is documented as
	// a name and defaults to "public", which is a name and not an ID; that default
	// cannot work and is the second half of the same config problem as EnvZID.
	FrontendIDs []string `json:"frontendIds,omitempty"`

	// Target is the private URL the agent dials. Named `targetUrl` here until the
	// documented example was read.
	Target string `json:"target"`

	// Tag would carry the publication key so Reap can recognise its own work,
	// mirroring the Target-prefix trick used against zrok.
	//
	// No such field is documented on a Frontdoor share. The documented object has
	// no tags, labels, description or metadata — the only free-form field that
	// round-trips is `name`. So this is sent hopefully and probably discarded, and
	// Reap's tag check then matches nothing, which fails safe: it deletes no share
	// rather than deleting the wrong one. The consequence is that orphans are never
	// collected and the tenant accumulates one share per preview per restart.
	//
	// Kept, rather than deleted, because Reap reports the truth either way — a
	// share this process created that comes back without its tag is proof the field
	// does not exist, logged as an error naming this struct. Removing it would
	// silence that and leave the same leak undiagnosed. The real fix is to move
	// ownership into the name, which changes every preview's public hostname and so
	// is a design decision rather than a field rename.
	Tag string `json:"tag,omitempty"`
}

// shareResponse is one share as the gateway reports it, on create and in a
// listing.
//
// Two fields are read through alternates, because the documentation shows the
// share object's field list without showing a response body. `frontendEndpoint`
// is what the object is documented to carry for its public address; `url` was the
// original guess. The identifier is `id` in every documented path
// (`DELETE /frontdoor/{frontdoorId}/{resource}/{id}`) while the share object's
// documented field list shows `zId` — so both are decoded and ID() prefers the
// one the paths use. Accepting an alternate costs a struct field; guessing wrong
// costs a leaked share per preview that nothing can delete.
type shareResponse struct {
	RawID string `json:"id"`
	ZID   string `json:"zId"`

	FrontendEndpoint string `json:"frontendEndpoint"`
	RawURL           string `json:"url"`

	Name string `json:"name"`
	Tag  string `json:"tag"`
}

// ID is the identifier to address this share at in a URL path.
func (s shareResponse) ID() string {
	if s.RawID != "" {
		return s.RawID
	}
	return s.ZID
}

// PublicURL is the address a reviewer opens.
//
// Per https://netfoundry.io/docs/frontdoor/learn/frontends/ a standard frontend
// serves a share at "https://{share-name}.shares.netfoundry.io", so this is
// derived from the name docpreview chose rather than assigned independently. That
// answers the question docs/design/17-exposer-frontdoor.md flagged as the first
// thing to check: recreating a share under the same name should return the same
// URL, so a restart is quiet rather than editing every open pull request's
// comment. It is still read from the response rather than constructed, because a
// custom frontend serves the same share at a different domain and only the
// gateway knows which frontend the ID pointed at.
func (s shareResponse) PublicURL() string {
	if s.FrontendEndpoint != "" {
		return s.FrontendEndpoint
	}
	return s.RawURL
}

// Publish binds a local port, then asks Frontdoor to route a public URL to it.
func (f *Frontdoor) Publish(ctx context.Context, spec Spec, h http.Handler) (*Publication, error) {
	// The name is the public hostname, so a second preview taking it would
	// point somebody else's URL at these artifacts. Refuse rather than
	// overwrite: a name_template that collides is a configuration mistake, and
	// silently serving the wrong site is the worst way to report one.
	// Two builds of one preview under one name is this preview's earlier build of the
	// same commit, and the newer one takes the name — collected here and withdrawn
	// below, because withdraw takes the lock itself.
	//
	// Checked *before* withdrawing this publication's own share, unlike zrok, which
	// withdraws first and can therefore refuse a publish having already destroyed the
	// working preview it was replacing. Under Frontdoor that costs more than a 404:
	// the share is deleted on the tenant, the database row still says `ready`, and the
	// pull request comment goes on linking to it until the next push. Nothing in the
	// collision loop looks at this publication's own entry — it skips spec.Key() — so
	// the order is free.
	var superseded []string
	f.mu.Lock()
	for id, entry := range f.live {
		if entry.name != spec.Name || id == spec.Key() {
			continue
		}
		if Collides(id, spec) {
			f.mu.Unlock()
			// The suggested template used to be "{{.Repo.Name}}-{{.Name}}", which is
			// config.DefaultNameTemplate — so the fix named in the error was already
			// in effect whenever the error appeared, and the operator's next move was
			// to change nothing. Two previews colliding under repo-plus-branch means
			// two repositories of the same name in different accounts, and the owner
			// is what separates those.
			return nil, fmt.Errorf("the name %q is already serving a different preview (%s); "+
				"two previews render to the same name under this name_template — "+
				"use \"{{.Repo.Owner}}-{{.Repo.Name}}-{{.Name}}\" to separate them", spec.Name, id)
		}
		superseded = append(superseded, id)
	}
	f.mu.Unlock()

	// Replaces this publication's own share and nothing else. Before creating the
	// new one, so the name is free — see the note on the ordering above.
	f.withdraw(ctx, spec.Key())

	for _, id := range superseded {
		f.log.Info("replacing an earlier build's share of this name",
			"name", spec.Name, "superseded", id, "publication", spec.Key())
		f.withdraw(ctx, id)
	}

	local, err := f.ports.serve(h)
	if err != nil {
		return nil, err
	}

	body := shareRequest{
		Type:        "http",
		Name:        spec.Name,
		EnvZID:      f.cfg.EnvZID,
		FrontendIDs: []string{f.cfg.Frontend},
		Target:      fmt.Sprintf("http://%s:%d", f.ports.host, local.port),
		Tag:         targetPrefix + spec.Key(),
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
	if created.ID() == "" || created.PublicURL() == "" {
		if cerr := closeLocal(local); cerr != nil {
			f.log.Error("cleanup after malformed share response", "error", cerr)
		}
		// Best effort: if an ID did come back, the share exists remotely and
		// would otherwise be unreachable and unreapable.
		if created.ID() != "" {
			if derr := f.deleteShare(ctx, created.ID()); derr != nil {
				f.log.Error("could not delete the half-created share",
					"id", created.ID(), "error", derr)
			}
		}
		return nil, fmt.Errorf("creating frontdoor share %q: response had no id or url "+
			"(check shareRequest/shareResponse in internal/expose/frontdoor.go "+
			"against the Frontdoor OpenAPI reference)", spec.Name)
	}

	entry := &frontdoorShare{id: created.ID(), name: spec.Name, local: local}
	f.mu.Lock()
	f.live[spec.Key()] = entry
	f.mu.Unlock()

	url := JoinURL(frontdoorOrigin(created.PublicURL()), spec.BaseURL)
	f.log.Info("published preview",
		"preview", spec.PreviewID, "build", spec.BuildID,
		"name", spec.Name, "url", url, "share", created.ID())

	return NewPublication(url, spec.Name, func() error {
		f.withdrawEntry(context.Background(), spec.Key(), entry)
		return nil
	}), nil
}

// frontdoorOrigin gives a scheme to whatever the gateway called the share's
// public address.
//
// The field is named `url` here and might well carry a bare hostname, which is
// what zrok's controller does — `Share.FrontendEndpoints` reports
// "name.share.zrok.io" and not a URL. That cost a round of broken pull request
// comments there: without a scheme, "x.example.com/docs/" is a *relative* path,
// so every preview link rendered by GitHub resolves against github.com and 404s
// on their side, where it looks like docpreview published a bad URL and cannot be
// debugged from this end. Cheap to defend against and expensive to discover, so
// it is defended against on a field whose contents nobody has seen.
//
// https rather than http: a hardened global frontend that terminates plain HTTP
// is not a thing worth guessing at, and an http:// link to an https-only frontend
// fails visibly rather than silently.
func frontdoorOrigin(s string) string {
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://" + s
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
	path := "/shares/" + url.PathEscape(id)
	err := f.retry(ctx, "DELETE "+path, func(attempt int) error {
		err := f.send(ctx, http.MethodDelete, path, nil, nil)

		// A 404 on a retry means the attempt that timed out actually worked.
		//
		// The deadline is this client's, not the gateway's, so a timed-out delete
		// has usually already happened — measured against zrok, where reporting
		// those as failures turned four successful deletions into four startup
		// errors. Only on a retry: a 404 on the first attempt means the share was
		// already gone when the listing named it, which is worth knowing about
		// because it means something else is deleting docpreview's shares.
		if attempt > 1 && isFrontdoorNotFound(err) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("deleting frontdoor share %s: %w", id, err)
	}
	return nil
}

// Reap deletes docpreview-tagged shares whose publication keys are not in keep.
//
// The tag prefix is what stops this deleting a share an operator created by hand
// in another terminal, which would be rude. It is also the only thing scoping the
// sweep: the tag carries a publication key and nothing identifying the daemon, so
// two docpreview instances sharing one Frontdoor tenant delete each other's live
// shares — B's startup reap sees every one of A's shares as prefix-matching and
// absent from B's keep-set. zrok escapes this because its listing is filtered by
// the environment's ziti identity; there is no equivalent here. One daemon per
// tenant, until the tag grows an instance identifier — which needs a config field
// and so is written up in docs/design/17-exposer-frontdoor.md rather than done.
func (f *Frontdoor) Reap(ctx context.Context, keep map[string]bool) error {
	shares, err := f.listShares(ctx)
	if err != nil {
		return fmt.Errorf("listing frontdoor shares: %w", err)
	}

	f.mu.Lock()
	liveIDs := make(map[string]bool, len(f.live))
	for _, entry := range f.live {
		liveIDs[entry.id] = true
	}
	f.mu.Unlock()

	// The doomed set, decided before anything is deleted.
	var doomed []string
	for _, shr := range shares {
		if liveIDs[shr.ID()] {
			// This process created this share, so it knows the tag it sent. A tag
			// that does not come back is therefore not "somebody else's share" —
			// it is proof that shareRequest.Tag and shareResponse.Tag do not name
			// the same field on the wire, or that the API drops the field
			// entirely. Either way Reap can never recognise an orphan again, so
			// say so loudly here rather than let the sweep quietly match nothing.
			if !strings.HasPrefix(shr.Tag, targetPrefix) {
				f.log.Error("a share this daemon created came back from the listing without its tag; "+
					"orphan detection is broken and the tenant will accumulate shares — "+
					"check shareRequest.Tag and shareResponse.Tag in internal/expose/frontdoor.go "+
					"against the Frontdoor API reference",
					"id", shr.ID(), "tag", shr.Tag)
			}
			continue
		}
		if !strings.HasPrefix(shr.Tag, targetPrefix) {
			continue
		}
		if keep[strings.TrimPrefix(shr.Tag, targetPrefix)] {
			continue
		}
		f.log.Info("reaping orphaned frontdoor share", "id", shr.ID(), "tag", shr.Tag)
		doomed = append(doomed, shr.ID())
	}

	// Deleted concurrently, bounded, for the reason measured against zrok: startup
	// reaps and only then republishes, so nothing builds and no preview serves
	// until the last deletion returns. Serially that made a daemon that looked
	// started and would not build anything, which reads as a stuck queue every
	// time — and Frontdoor has strictly more to delete than zrok did, because a
	// preview holds one share per kept build rather than one share.
	//
	// Safe to parallelise because each deletion is independent: an id appears in
	// this list once, nothing here reads shared state after the list is built, and
	// the gateway is the serialisation point. What is not safe is overlapping this
	// with the republish that follows — see the reap-before-republish rule in
	// docs/design/02-exposers.md — and that ordering is the daemon's, unchanged.
	const parallel = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sem  = make(chan struct{}, parallel)
	)
	for _, id := range doomed {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := f.deleteShare(ctx, id); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
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

// frontdoorAPIError is a non-2xx answer from the gateway.
//
// Typed rather than a formatted string because two callers have to branch on the
// status: retry has to tell "the gateway is busy" from "the gateway refused",
// and deleteShare has to tell a 404 on a retry — which means the attempt that
// timed out actually worked — from a 404 on the first try. Matching those on
// substrings of a message is what zrok.go has to do because its SDK gives it
// nothing better (see isNotFound there); this client owns its own transport and
// does not have that excuse.
type frontdoorAPIError struct {
	Method string
	Path   string
	Status string
	Code   int

	// Body is capped by the caller. An error page from a misconfigured proxy can
	// be megabytes and none of it belongs in a log line.
	Body string
}

func (e *frontdoorAPIError) Error() string {
	return fmt.Sprintf("%s %s: %s: %s", e.Method, e.Path, e.Status, e.Body)
}

// frontdoorStatus reports the HTTP status behind err, or 0 if err is not an
// answer from the gateway.
func frontdoorStatus(err error) int {
	var apiErr *frontdoorAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return 0
}

// frontdoorRetryable reports whether a failed call is worth trying again.
//
// Two kinds qualify. Transport failures — a timeout, a reset, a half-open
// connection — are the ones `transient` already recognises for zrok, and the
// list is shared deliberately: it was assembled from the hosted controller's
// real behaviour and there is no reason a different NetFoundry gateway behind
// the same kind of load balancer fails differently.
//
// The second kind is the gateway saying so itself. 429 is a rate limit, and
// 502/503/504 are a load balancer that never got an answer from the thing behind
// it. Every other 4xx and 5xx is not retried: a 401, a 403 or a quota refusal
// retried three times is three times the same refusal, plus two backoffs of
// startup the operator waits through before seeing the message.
func frontdoorRetryable(err error) bool {
	switch frontdoorStatus(err) {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	case 0:
		return transient(err)
	}
	return false
}

// isFrontdoorNotFound recognises the gateway's answer for a share that is gone.
func isFrontdoorNotFound(err error) bool {
	return frontdoorStatus(err) == http.StatusNotFound
}

// retry runs fn until it stops failing in a way that looks like the gateway
// rather than the request. fn is told which attempt it is on, counting from 1.
//
// Frontdoor had no retry at all, and every call it makes is one a slow gateway
// can lose: a publish that fails here fails the build, and a reap that fails
// here leaks a share against the tenant until the next restart notices. zrok
// grew this after the hosted controller timed out under load often enough to be
// the normal case; the same failure is not less likely against a global ingress.
//
// ctx is honoured between attempts, so a shutdown does not sit through a backoff.
func (f *Frontdoor) retry(ctx context.Context, what string, fn func(attempt int) error) error {
	attempt := 1
	err := fn(attempt)
	for i, wait := range zrokBackoff {
		if !frontdoorRetryable(err) {
			return err
		}
		f.log.Warn("frontdoor call failed, retrying", "call", what,
			"attempt", attempt, "in", wait, "error", err)
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
		attempt = i + 2
		err = fn(attempt)
	}
	return err
}

// do issues an authenticated request against the Frontdoor gateway, retrying a
// gateway-shaped failure.
//
// Retried for every method, including the POST that creates a share, and that is
// a deliberate trade rather than an oversight. A timed-out create may well have
// succeeded on the far side, so the retry can leave a duplicate — but the
// duplicate carries the `docpreview:` tag, so the next Reap collects it, while
// the alternative is a preview that built successfully, has its artifacts on
// disk, and is not served because one HTTP request was slow. The same call was
// made for zrok, for the same reason, and is written up on Publish there.
func (f *Frontdoor) do(ctx context.Context, method, path string, in, out any) error {
	return f.retry(ctx, method+" "+path, func(int) error {
		return f.send(ctx, method, path, in, out)
	})
}

// send issues one authenticated request, with no retry.
func (f *Frontdoor) send(ctx context.Context, method, path string, in, out any) error {
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
		apiErr := &frontdoorAPIError{
			Method: method,
			Path:   path,
			Status: resp.Status,
			Code:   resp.StatusCode,
			Body:   strings.TrimSpace(string(snippet)),
		}

		// A 401 here is usually not a wrong token but an expired one, and the
		// difference decides what the operator does next.
		//
		// Frontdoor bearers are not static credentials: they come from an OAuth2
		// client-credentials exchange against the organization's token endpoint,
		// with a clientId and password out of the credentials.json downloaded from
		// the console
		// (https://netfoundry.io/docs/platform/api-guides/authentication). So what
		// `docpreview vault set frontdoor.api_token` holds is an access token with
		// a lifetime, and a daemon that runs longer than that lifetime starts
		// failing every publish and every reap with a 401 while nothing about the
		// configuration has changed. Saying so is the difference between rotating
		// the token and re-reading the setup runbook from the top.
		if apiErr.Code == http.StatusUnauthorized {
			return fmt.Errorf("%w — frontdoor.api_token is rejected. These tokens are short-lived: "+
				"they come from an OAuth2 client-credentials exchange, so a token that worked "+
				"at startup expires while the daemon runs. Mint a fresh one and "+
				"'docpreview vault set frontdoor.api_token'; the lasting fix is for docpreview "+
				"to hold the clientId and password and do the exchange itself", apiErr)
		}
		return apiErr
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
