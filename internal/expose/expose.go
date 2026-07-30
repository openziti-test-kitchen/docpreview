// Package expose turns a locally-built documentation preview into something a
// reviewer can open in a browser.
//
// This is the seam that makes docpreview portable. Everything upstream — clone,
// detect, build, serve — produces an http.Handler. Everything downstream — the
// PR comment — consumes a URL. The Exposer in between is the only part that
// knows whether that URL is backed by zrok, by NetFoundry Frontdoor, or by
// nothing at all because you are debugging on a laptop.
//
// The interface is handler-based rather than port-based on purpose. zrok's Go
// SDK hands back a net.Listener on the OpenZiti overlay, so a zrok preview
// never binds a local TCP port and never appears in netstat; asking the caller
// for a port would force zrok into a worse shape to satisfy an abstraction that
// only Frontdoor needs. Frontdoor, which reaches previews over the network from
// an agent, binds a real port internally where the zrok implementation does
// not.
package expose

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/vault"
)

// TokenFunc resolves an exposer's API credential at the moment it is needed.
//
// A function rather than the value, because the value lives in a vault that may
// be locked when the daemon starts. Fetching it during wiring meant an exposer
// that needs a credential could not boot until the vault was already open —
// while the page that opens the vault is served by the daemon that would not
// start. The same shape as the GitHub client's deferred construction, for the
// same reason.
//
// It is called on every request rather than cached, so rotating the credential
// from the setup page takes effect without a restart. The cost is a map lookup
// under a mutex per API call, against a network round trip.
type TokenFunc func() (vault.Secret, error)

// Spec describes the preview to be published.
type Spec struct {
	// PreviewID is the stable per-pull-request identifier. Implementations use
	// it to tag their remote objects so orphans can be found and reaped.
	PreviewID string

	// Name is the DNS label the preview should be reachable at, already
	// sanitized. This is the branch name in the default configuration.
	Name string

	// BaseURL is the path prefix the site is served under, normalized to the
	// "/" or "/foo/" form. It is appended to the host to form the URL shown to
	// reviewers, because a Docusaurus site built with baseUrl "/docs/" has
	// nothing at "/".
	BaseURL string

	// BuildID names one build of this preview, empty for the preview's own share.
	//
	// A preview has one share that follows its newest successful build — the branch
	// share — and, when per-build publishing is on, one more per build that stays
	// pinned to the commit it was built from. Both are publications of the same
	// preview, so PreviewID alone can no longer identify one.
	BuildID string

	// PR is carried for logging and for name templating.
	PR model.PullRequest
}

// Key identifies one publication.
//
// Every exposer keys its live publications and tags its remote objects with this,
// and Reap's keep-set is expressed in it. It used to be PreviewID, which made a
// second share for the same preview impossible by construction: Publish withdraws
// whatever holds the key before taking it, so publishing a build share tore down
// the branch share it was meant to sit beside.
//
// The preview id alone for the branch share, so the tag of an existing share is
// unchanged and a daemon upgraded in place does not reap every preview it restored
// as an orphan on the first sweep.
func (s Spec) Key() string {
	if s.BuildID == "" {
		return s.PreviewID
	}
	return s.PreviewID + "/" + s.BuildID
}

// NameReleaser is implemented by exposers whose names are objects with a lifetime
// of their own, separate from the shares bound to them.
//
// Only zrok, today. A reserved zrok name survives the share it was created for —
// which is the point, since that is what keeps a preview's URL stable across
// rebuilds and restarts — and is also the object an account's quota counts. So
// something has to delete it when the preview is gone for good, and nothing did:
// docpreview leaked one name per branch, and would have leaked one per commit once
// builds got their own shares.
//
// Optional rather than part of Exposer, because for the other three there is no such
// object. `local` mounts a path, `frontdoor` names the share itself, `ziti` derives a
// hostname — none of them has anything left over to release, and giving them all a
// method that returns nil would suggest they do.
//
// Called from teardown only, and once per name the preview ever published — its own
// and one per build share. See Zrok.ReleaseName for why a rebuild must not, and why
// releasing before withdrawing the share is the order that survives a crash.
//
// An error here does not fail a teardown. The pull request is gone either way; what
// is left is one name against a quota, which is worth a log and not worth keeping a
// dead preview's artifacts on disk for.
type NameReleaser interface {
	ReleaseName(ctx context.Context, name string) error
}

// PathExposer is implemented by exposers that serve previews under a path on
// the daemon's own listener instead of giving each one its own host.
//
// The daemon has to ask for the path before the build rather than discovering
// it at publish, because Docusaurus bakes baseUrl in at build time: a site
// built for "/" and served under "/preview/docs-main/" returns its HTML and
// 404s every asset in it. MountPath is therefore pure — a function of the name,
// callable before anything is published.
type PathExposer interface {
	// MountPath is the path prefix a preview with this name is served under,
	// with a leading and trailing slash.
	MountPath(name string) string

	// Handler serves every published preview, routed by path.
	Handler() http.Handler
}

// Publication is a live preview. Closing it withdraws the public URL and stops
// serving.
type Publication struct {
	// URL is the address to put in the pull request comment.
	URL string

	// Name is the label actually used, which may differ from the requested one
	// if the implementation had to disambiguate.
	Name string

	closeFn func() error
}

// NewPublication is a constructor for implementations in this package's
// subpackages and tests.
func NewPublication(url, name string, closeFn func() error) *Publication {
	return &Publication{URL: url, Name: name, closeFn: closeFn}
}

// Close withdraws the publication. It is safe to call more than once.
func (p *Publication) Close() error {
	if p == nil || p.closeFn == nil {
		return nil
	}
	fn := p.closeFn
	p.closeFn = nil
	return fn()
}

// Exposer publishes preview handlers at public URLs.
//
// Implementations must be idempotent with respect to Spec.Name: publishing the
// same name twice must either reuse or replace the earlier publication rather
// than failing, because a reviewer pushing three commits in a minute is the
// normal case, not the exceptional one.
type Exposer interface {
	// Kind names the implementation, for logs and for the /healthz payload.
	Kind() string

	// Validate checks that the exposer can actually do its job, and is called
	// once at startup. It exists so that a misconfigured zrok environment
	// produces one clear error at boot rather than a mystifying failure on the
	// first pull request of the day.
	Validate(ctx context.Context) error

	// Publish serves h at a public URL derived from spec.
	Publish(ctx context.Context, spec Spec, h http.Handler) (*Publication, error)

	// Reap removes any published resources this exposer owns that are not in
	// keep. It runs at startup and periodically thereafter.
	//
	// It used to run at startup with a nil keep-set, on the reasoning that
	// everything it found was an orphan from a previous process. That is true of
	// the *listener* and false of the published resource: a zrok share survives the
	// process that made it, so deleting them all meant paying to delete thirteen
	// shares and then paying again to create thirteen identical ones. Startup now
	// passes what the database claims — see Adopter.
	Reap(ctx context.Context, keep map[string]bool) error

	// Close shuts down every live publication.
	Close() error
}

// Adoptable is a publication a previous process created that this one can take over.
//
// Handle is exposer-private — a zrok share token — and is passed back to Adopt
// unchanged rather than interpreted by the caller.
type Adoptable struct {
	Handle string

	// Origin is the scheme and host the publication is already reachable at, as the
	// exposer reported it. Empty means the exposer cannot say, and the candidate is
	// then not adoptable: a publication whose URL has to be guessed is worse than one
	// that is recreated, because the guess ends up in a pull request comment.
	Origin string
}

// Adopter is an exposer that can serve a publication a previous process created,
// instead of deleting it and making an identical one.
//
// This exists because delete-then-recreate was measured: thirteen shares for two pull
// requests took 85 seconds to delete and 183 seconds to recreate, and for that four and
// a half minutes every preview URL 404s and no queued build can start. What actually
// dies with the process is the overlay listener, and a listener can be bound to an
// existing share by its token — so the controller work is avoidable in the common case,
// which is a restart with an unchanged database.
//
// Optional. An exposer that does not implement it keeps the old behaviour, which is
// correct for `local` (its URLs are paths on a listener that no longer exists) and
// unimplemented for the others.
type Adopter interface {
	// Adoptable lists what could be taken over, keyed by publication key — the same
	// Spec.Key() the daemon uses. One call rather than one per candidate, because the
	// round trip being avoided is the whole point.
	Adoptable(ctx context.Context) (map[string]Adoptable, error)

	// Adopt serves h on the existing publication described by a.
	//
	// A failure here is not fatal to the caller: the publication can still be created
	// from scratch. It is fatal to *this* publication, so anything half-bound must be
	// cleaned up before returning.
	Adopt(ctx context.Context, spec Spec, a Adoptable, h http.Handler) (*Publication, error)
}

// nameData is the template context for a name template.
type nameData struct {
	model.PullRequest

	// Name is the sanitized branch name. Templates that just want "the branch,
	// as a hostname" use {{.Name}} and never touch the raw Branch field.
	Name string
}

// RenderName evaluates a name template against a pull request and sanitizes the
// result.
//
// The output is sanitized a second time after templating because a template
// like "{{.Repo.Name}}-{{.Name}}" can reintroduce characters that are legal in
// a repository name but not in a hostname.
func RenderName(tmplText string, pr model.PullRequest) (string, error) {
	if strings.TrimSpace(tmplText) == "" {
		// Matches config.DefaultNameTemplate. Not imported from there, because
		// config imports nothing from this package and the dependency should
		// not start running the other way for a string constant.
		tmplText = "{{.Repo.Name}}-{{.Name}}"
	}
	tmpl, err := template.New("name").Option("missingkey=error").Parse(tmplText)
	if err != nil {
		return "", fmt.Errorf("parsing name template %q: %w", tmplText, err)
	}
	var sb strings.Builder
	data := nameData{PullRequest: pr, Name: model.SanitizeName(pr.Branch)}
	if err := tmpl.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("rendering name template %q: %w", tmplText, err)
	}
	name := model.SanitizeName(sb.String())
	if name == "" {
		return "", fmt.Errorf("name template %q produced an empty name for %s", tmplText, pr)
	}
	return name, nil
}

// JoinURL appends a normalized baseURL path to a scheme+host origin.
func JoinURL(origin, baseURL string) string {
	origin = strings.TrimRight(origin, "/")
	if baseURL == "" || baseURL == "/" {
		return origin + "/"
	}
	return origin + baseURL
}
