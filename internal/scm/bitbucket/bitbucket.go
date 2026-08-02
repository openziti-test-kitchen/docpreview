// Package bitbucket talks to Bitbucket Cloud.
//
// The second implementation of scm.Client, and the one that made the interface
// honest. Read docs/design/15-bitbucket.md before changing anything here: almost
// every decision in this package was made against a live workspace rather than
// against documentation, and several of the differences from GitHub are not the
// ones you would guess.
//
// The three that cost the most if forgotten:
//
//   - A pull request payload's commit hash is **twelve characters**, not forty.
//     Git resolves an abbreviation, so the clone and the build work and nothing
//     visibly fails — while every comparison against a full hash silently
//     disagrees. Resolved to the full SHA at parse time; see resolveCommit.
//
//   - The signature header is `X-Hub-Signature`, which on GitHub is the *legacy
//     SHA-1* header. Same name, different algorithm, so the two clients must
//     never share a "find the signature header" helper.
//
//   - There is no installation token. Whatever CloneURL returns is as long-lived
//     as the stored credential, which is why a repository access token is
//     recommended over an account API token: the blast radius of a leak is one
//     repository rather than an account.
//
//     **Two different mechanisms keep it out of the open.** It is *not* registered with
//     `internal/redact`, the value-based redactor compiled from `build.secrets` — that
//     one only ever sees shell-shaped keys (`vault.IsBuildEnvKey`), and every credential
//     here is dotted, so it is excluded by construction and deliberately never reaches a
//     build. What actually covers it is `pipeline.scrub`, which strips RFC 3986 userinfo
//     from git's own output, plus `vault.Secret`'s redacting String, Format, GoString and
//     MarshalJSON.
//
//     The distinction matters for anything added later: a new code path that
//     printed CloneURL's result on the assumption that "the redactor has this
//     value" would be a real leak, because the redactor does not.
package bitbucket

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/vault"
)

// statusKey identifies docpreview's commit build status. Like GitHub's check name
// it doubles as the update key, so it must not vary.
const statusKey = "docpreview"

// Client talks to Bitbucket Cloud with a stored credential.
//
// No authenticator, no token cache, no invalidate-on-401 — because nothing here
// expires on a schedule. That is genuinely simpler than the GitHub client, and the
// simplicity is bought with a credential that never expires on its own.
type Client struct {
	cfg  config.BitbucketConfig
	log  *slog.Logger
	http *http.Client

	// creds is the credential this client falls back to: the global vault keys, read
	// once at construction. Empty when none are stored, which is valid — every project
	// may carry its own.
	creds credential

	// perRepo resolves one repository's own credential, or the zero value when it has
	// none. A function rather than a map, and consulted per call rather than captured,
	// for the same reasons Daemon.SetProjectSecrets is a resolver: the vault may be
	// locked when this client is built, a token added from the projects page has to
	// apply without a restart, and the answer differs per repository.
	//
	// Bitbucket is the reason this exists at all. An access token there is scoped to a
	// repository, a project or a workspace, and an administrator can refuse the wider
	// two — so on a real workspace there is no single token that reaches every
	// repository, and one global credential cannot be the whole story.
	perRepo func(owner, repo string) ProjectCredential

	webhookSecret vault.Secret

	// comments maps a preview ID to the comment carrying its marker, for the life of
	// the process. Same reasoning as the GitHub client: a list endpoint is not
	// read-your-writes consistent, so two reports in quick succession would each
	// conclude there was no comment and create one. Never persisted — a stored ID
	// outlives the comment it names and turns every later update into a 404.
	commentsMu sync.Mutex
	comments   map[string]int64
}

// ProjectCredential is one project's own Bitbucket credential, as the resolver reports
// it. All fields empty means "this project has none, use the global one".
//
// Both modes are carried rather than a mode being chosen per project, because which one
// an operator filled in is the answer: a repository access token needs no email, and an
// account API token is useless without one.
type ProjectCredential struct {
	AccessToken string
	Email       string
	APIToken    string
}

// credential is a resolved Authorization header plus the two halves of a clone URL's
// userinfo. Built from either a ProjectCredential or the global vault keys.
type credential struct {
	// authz is the Authorization header value. A secret: it holds the token verbatim.
	authz vault.Secret

	// user and pass are the clone URL's userinfo. For a bearer token the user is the
	// literal "x-token-auth"; for basic auth it is the account email.
	user string
	pass vault.Secret
}

func (c credential) empty() bool { return len(c.authz.Reveal()) == 0 }

// bearer builds a credential from an access token: a repository, project or workspace
// token, which is the recommended shape.
func bearer(token string) credential {
	return credential{
		authz: vault.NewSecretString("Bearer " + token),
		// The literal string, not the token and not the workspace. Bitbucket's
		// convention, and getting it wrong is a 403 on clone with nothing to say why.
		user: "x-token-auth",
		pass: vault.NewSecretString(token),
	}
}

// basic builds a credential from an account email and API token — the fallback mode,
// whose blast radius is everything that account can see.
func basic(email, token string) credential {
	return credential{
		authz: vault.NewSecretString("Basic " +
			base64.StdEncoding.EncodeToString([]byte(email+":"+token))),
		user: email,
		pass: vault.NewSecretString(token),
	}
}

// WithProjectCredentials installs the per-project credential resolver.
func (c *Client) WithProjectCredentials(fn func(owner, repo string) ProjectCredential) *Client {
	c.perRepo = fn
	return c
}

// credentialFor picks the credential to use for one repository.
//
// A project's own wins outright rather than being merged with the global one. Merging
// would mean an operator who stored a repository token for one project could still be
// authenticating as the workspace-wide account for half of the calls, which is the kind
// of thing that works until the day the wider credential is revoked.
//
// An access token wins over an email-and-API-token pair when a project somehow has both,
// because it is the narrower of the two.
func (c *Client) credentialFor(owner, repo string) (credential, error) {
	if c.perRepo != nil {
		p := c.perRepo(owner, repo)
		switch {
		case p.AccessToken != "":
			return bearer(p.AccessToken), nil
		case p.Email != "" && p.APIToken != "":
			return basic(p.Email, p.APIToken), nil
		}
	}
	if c.creds.empty() {
		return credential{}, fmt.Errorf(
			"no Bitbucket credential for %s/%s: add an access token to that project on "+
				"/projects, or store a workspace-wide one with "+
				"'docpreview vault set %s'", owner, repo, vault.KeyBitbucketAccessToken)
	}
	return c.creds, nil
}

// New builds a Bitbucket client from configuration and vault contents.
//
// The auth mode comes from the config and is never inferred from which keys are
// present. Both modes need the webhook secret, and its absence is fatal here
// rather than at the first delivery: a missing secret means Bitbucket sends no
// signature at all, so the endpoint would accept unauthenticated build triggers.
func New(cfg config.BitbucketConfig, v *vault.Vault, log *slog.Logger) (*Client, error) {
	if cfg.APIBase == "" {
		cfg.APIBase = config.BitbucketAPIBase
	}
	cfg.APIBase = strings.TrimRight(cfg.APIBase, "/")
	if err := checkAPIBase(cfg.APIBase); err != nil {
		return nil, err
	}

	if cfg.Auth == "" {
		cfg.Auth = config.BitbucketAuthAccessToken
	}

	secret, err := v.MustGet(vault.KeyBitbucketHookSec)
	if err != nil {
		return nil, err
	}

	c := &Client{
		cfg:           cfg,
		log:           log.With("scm", "bitbucket"),
		http:          &http.Client{Timeout: 30 * time.Second, Transport: ipv4First()},
		webhookSecret: secret,
	}

	// The global credential is optional: true of a *repository*, not of the client.
	// Bitbucket access tokens are scoped to a repository unless an administrator permits
	// the wider kinds, and one who does not leaves an operator with a token per project
	// and nothing global to store. Requiring one here would refuse to build a Bitbucket
	// client at all in exactly that case, so the projects page could not be used to supply
	// the tokens that would make it work.
	//
	// So: absent is fine, and a repository with neither its own credential nor a global
	// one fails at the point of use, naming both places it could come from.
	switch cfg.Auth {
	case config.BitbucketAuthAccessToken:
		if token, err := v.Get(vault.KeyBitbucketAccessToken); err == nil {
			c.creds = bearer(string(token.Reveal()))
		}

	case config.BitbucketAuthAPIToken:
		email, emailErr := v.Get(vault.KeyBitbucketEmail)
		token, tokenErr := v.Get(vault.KeyBitbucketAPIToken)
		if emailErr == nil && tokenErr == nil {
			c.creds = basic(string(email.Reveal()), string(token.Reveal()))
		}

	default:
		return nil, fmt.Errorf("bitbucket.auth is %q; use %q (a repository access token, recommended) "+
			"or %q (an account email and API token)",
			cfg.Auth, config.BitbucketAuthAccessToken, config.BitbucketAuthAPIToken)
	}

	return c, nil
}

// ipv4First is a transport that dials IPv4 before IPv6.
//
// A connection to api.bitbucket.org over IPv6 can be accepted, get through the TLS
// handshake, and then be reset mid-response —
//
//	read tcp [2603:…]:65086->[2401:1d80:…]:443: wsarecv: An existing connection was
//	forcibly closed by the remote host
//
// Go's dialer prefers IPv6 where an address exists, and its Happy Eyeballs fallback only
// covers a connection that fails to *establish* — this one establishes and then dies, so
// nothing retries it and every call fails with a transport error that looks like the token
// being wrong.
//
// IPv4 first, not IPv4 only: the fallback keeps an IPv6-only network working, which a hard
// "tcp4" would break with an error that names the wrong problem.
func ipv4First() http.RoundTripper {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network == "tcp" {
			if conn, err := d.DialContext(ctx, "tcp4", addr); err == nil {
				return conn, nil
			}
			// No IPv4 route, or none that answered. Fall through to whatever the resolver
			// offers rather than reporting a network failure that is really a preference.
		}
		return d.DialContext(ctx, network, addr)
	}
	return t
}

// checkAPIBase refuses api calls aimed at bitbucket.org itself.
//
// Encoded rather than discovered: since 4 May 2026 authenticated REST calls must go
// to api.bitbucket.org, and a call to bitbucket.org/api answers 403 with a body that
// does not say so.
func checkAPIBase(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("bitbucket.api_base %q is not a URL: %w", base, err)
	}
	if strings.EqualFold(u.Host, "bitbucket.org") || strings.EqualFold(u.Host, "www.bitbucket.org") {
		return fmt.Errorf("bitbucket.api_base is %q; authenticated REST calls must go to %s "+
			"(bitbucket.org/api answers 403)", base, config.BitbucketAPIBase)
	}
	return nil
}

func (c *Client) Platform() model.Platform { return model.PlatformBitbucket }

// Validate confirms the credential works, by asking who we are.
//
// /user answers for an account credential and 403s for a repository access token,
// which is not a failure — that token cannot see a user and is not supposed to. So a
// 403 here is reported as "the credential works but is scoped", and only an
// authentication failure is fatal.
func (c *Client) Validate(ctx context.Context) error {
	var who struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		AccountID   string `json:"account_id"`
	}
	// No repository, so this checks the *global* credential. A daemon whose tokens are
	// all per-project has none, and that is not a failure — reported below.
	if c.creds.empty() {
		c.log.Info("no workspace-wide bitbucket credential stored; "+
			"each project supplies its own", "auth", c.cfg.Auth)
		return nil
	}
	err := c.do(ctx, "", "", http.MethodGet, "/2.0/user", nil, &who)
	if err == nil {
		c.log.Info("bitbucket credential validated",
			"auth", c.cfg.Auth, "account", who.DisplayName, "username", who.Username)
		return nil
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
		// Expected for a scoped token. Confirmed by the request being answered at
		// all: an invalid credential is 401, not 403.
		c.log.Info("bitbucket credential validated", "auth", c.cfg.Auth,
			"note", "scoped token; /user is not visible to it")
		return nil
	}
	return fmt.Errorf("bitbucket rejected the stored credential "+
		"(check 'docpreview vault set %s' and that the token has repository and pullrequest scopes): %w",
		c.tokenKey(), err)
}

// tokenKey names the vault key for the active mode, for error messages that tell an
// operator which one to fix.
func (c *Client) tokenKey() string {
	if c.cfg.Auth == config.BitbucketAuthAPIToken {
		return vault.KeyBitbucketAPIToken
	}
	return vault.KeyBitbucketAccessToken
}

// CloneURL builds a URL git can clone.
//
// Built rather than decorated, and that is the whole point of this function.
// Bitbucket's own HTTPS clone link already carries a username —
// `https://dovholuknf@bitbucket.org/ws/repo.git`, whose username depends on who
// asked — so inserting credentials into it produces two `@` in the authority, a git
// failure whose message contains the token, and a scrubber doing work it should not
// have to. The workspace and slug are all that is taken from the repository.
//
// The result is a credential. The interface says so and the cloner treats it that
// way; unlike GitHub there is no short-lived form to reach for, so this string is as
// long-lived as what is in the vault.
func (c *Client) CloneURL(_ context.Context, pr model.PullRequest) (string, error) {
	cred, err := c.credentialFor(pr.Repo.Owner, pr.Repo.Name)
	if err != nil {
		return "", err
	}
	// Both halves escaped. An unescaped email contains an `@`, which would defeat
	// pipeline.scrubLine's log scrubber outright; escaping here is the difference
	// between a defence and a convention.
	return fmt.Sprintf("https://%s:%s@bitbucket.org/%s/%s.git",
		url.QueryEscape(cred.user), url.QueryEscape(string(cred.pass.Reveal())),
		pr.Repo.Owner, pr.Repo.Name), nil
}

// CheckRepo confirms the credential for one repository can actually reach it.
//
// Behind the projects page's Test button. A token is pasted once, shown once, and
// otherwise unverifiable — and the failure without this is a build that clones for twenty
// seconds and then reports an authentication error, or worse, a comment that never
// appears because the token has read access and not write.
//
// Two calls, because two permissions are needed and only one of them is exercised by
// reading: the repository read that a clone needs, and the pull request read that
// ChangedFiles needs. Write cannot be checked without writing something, so the message
// says which scope is still unproven rather than implying everything is fine.
func (c *Client) CheckRepo(ctx context.Context, repo model.Repo) (string, error) {
	var meta struct {
		FullName  string `json:"full_name"`
		IsPrivate bool   `json:"is_private"`
		Project   struct {
			Key string `json:"key"`
		} `json:"project"`
	}
	if err := c.do(ctx, repo.Owner, repo.Name, http.MethodGet,
		fmt.Sprintf("/2.0/repositories/%s/%s", repo.Owner, repo.Name), nil, &meta); err != nil {
		return "", err
	}

	var prs struct {
		Size int `json:"size"`
	}
	if err := c.do(ctx, repo.Owner, repo.Name, http.MethodGet,
		fmt.Sprintf("/2.0/repositories/%s/%s/pullrequests?state=OPEN&pagelen=1", repo.Owner, repo.Name),
		nil, &prs); err != nil {
		return "", fmt.Errorf("the token can read the repository but not its pull requests "+
			"(add the pullrequest scope): %w", err)
	}

	visibility := "public"
	if meta.IsPrivate {
		visibility = "private"
	}
	// Two lines separated by a newline: a headline and the detail behind it.
	//
	// This was one sentence carrying five facts, which is a paragraph to say "it worked".
	// The caller shows the first line and keeps the rest for a tooltip — the detail is worth
	// having when a workspace-wide token answers for a project somebody thought they had
	// overridden, and worth hiding every other time.
	return fmt.Sprintf("Works — read access confirmed.\n%s is %s with %d open pull "+
		"request(s), reached with %s. Commenting needs pullrequest:write, which cannot be "+
		"checked without writing something.",
		meta.FullName, visibility, prs.Size, c.credentialSource(repo.Owner, repo.Name)), nil
}

// credentialSource names where the credential for one repository came from, for messages.
//
// Its own or the workspace's is the distinction that matters: a project whose token was
// stored but not resolved authenticates as the wider account, which works right up until
// that account is revoked or its access is narrowed.
func (c *Client) credentialSource(owner, repo string) string {
	if c.perRepo != nil {
		p := c.perRepo(owner, repo)
		switch {
		case p.AccessToken != "":
			return "this project's own access token"
		case p.Email != "" && p.APIToken != "":
			return "this project's own account email and API token"
		}
	}
	if c.cfg.Auth == config.BitbucketAuthAPIToken {
		return "the workspace-wide account email and API token"
	}
	return "the workspace-wide access token"
}

// maxChangedFilePages bounds the diffstat walk, with the same reasoning as the
// GitHub client: past thirty pages the answer is "yes, build it".
const maxChangedFilePages = 30

// ChangedFiles lists the paths the pull request touches, from the diffstat endpoint.
//
// Cheaper than /diff and exactly the shape wanted. Three things about the response
// are easy to get wrong and all three have bitten somebody:
//
//   - `old` and `new` are both nilable. There is no `old` on an added file and no
//     `new` on a removed one, so a naive entry.Old.Path panics on the first pull
//     request that adds a file.
//   - A rename contributes *both* paths. A file moved out of docs/ is a
//     documentation change even though its new path matches no doc glob.
//   - `pagelen` defaults to 500, so the pagination loop almost never runs twice —
//     which means it is almost never exercised. It is tested against a fake server
//     that returns two short pages rather than trusted because real pull requests
//     pass.
func (c *Client) ChangedFiles(ctx context.Context, pr model.PullRequest) ([]string, error) {
	type diffstatEntry struct {
		Status string `json:"status"`
		Old    *struct {
			Path string `json:"path"`
		} `json:"old"`
		New *struct {
			Path string `json:"path"`
		} `json:"new"`
	}

	path := fmt.Sprintf("/2.0/repositories/%s/%s/pullrequests/%d/diffstat?pagelen=100",
		pr.Repo.Owner, pr.Repo.Name, pr.Number)

	var files []string
	var size int
	for page := 0; page < maxChangedFilePages && path != ""; page++ {
		var envelope struct {
			Values []diffstatEntry `json:"values"`
			Size   int             `json:"size"`
			Next   string          `json:"next"`
		}
		if err := c.do(ctx, pr.Repo.Owner, pr.Repo.Name,
			http.MethodGet, path, nil, &envelope); err != nil {
			return nil, fmt.Errorf("listing changed files on %s: %w", pr, err)
		}
		if page == 0 {
			size = envelope.Size
		}
		for _, e := range envelope.Values {
			if e.New != nil && e.New.Path != "" {
				files = append(files, e.New.Path)
			}
			if e.Old != nil && e.Old.Path != "" && (e.New == nil || e.Old.Path != e.New.Path) {
				files = append(files, e.Old.Path)
			}
		}

		next, err := c.samePage(envelope.Next)
		if err != nil {
			// A `next` pointing somewhere else is not followed. Blindly following a
			// server-supplied URL is how an API response becomes a request to
			// wherever it likes.
			c.log.Warn("ignoring a paginated next link that leaves the API host",
				"pr", pr.String(), "error", err)
			break
		}
		path = next
	}

	// The envelope carries a total, so the loop has a cross-check: paths short of
	// `size` means something was dropped silently, which is worth a line in the log
	// rather than a wrong skip decision made in quiet.
	if size > 0 && len(files) < size {
		c.log.Warn("fewer changed paths than the diffstat total",
			"pr", pr.String(), "collected", len(files), "size", size)
	}
	return files, nil
}

// samePage validates a pagination link and returns it as a path.
//
// Empty in, empty out: the last page has no `next` field at all, which is the loop's
// termination condition.
func (c *Client) samePage(next string) (string, error) {
	if next == "" {
		return "", nil
	}
	u, err := url.Parse(next)
	if err != nil {
		return "", err
	}
	base, err := url.Parse(c.cfg.APIBase)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(u.Host, base.Host) {
		return "", fmt.Errorf("next link host %q is not %q", u.Host, base.Host)
	}
	if u.RawQuery == "" {
		return u.Path, nil
	}
	return u.Path + "?" + u.RawQuery, nil
}

// Publish writes the report to the pull request.
//
// The comment is the durable artifact; the build status is a convenience, so a
// failure to write it is logged rather than returned.
func (c *Client) Publish(ctx context.Context, r scm.Report) error {
	if err := c.upsertComment(ctx, r); err != nil {
		return err
	}
	if err := c.postBuildStatus(ctx, r); err != nil {
		c.log.Warn("could not write build status", "pr", r.PR.String(), "error", err)
	}
	return nil
}

func (c *Client) upsertComment(ctx context.Context, r scm.Report) error {
	body := scm.RenderComment(r)

	existing, ok := c.knownComment(r.PreviewID)
	if !ok {
		var err error
		existing, err = c.findComment(ctx, r.PR, r.PreviewID)
		if err != nil {
			return err
		}
		if existing != 0 {
			c.rememberComment(r.PreviewID, existing)
		}
	}

	payload := map[string]any{"content": map[string]string{"raw": body}}

	if existing == 0 {
		path := fmt.Sprintf("/2.0/repositories/%s/%s/pullrequests/%d/comments",
			r.PR.Repo.Owner, r.PR.Repo.Name, r.PR.Number)
		var created struct {
			ID int64 `json:"id"`
		}
		if err := c.do(ctx, r.PR.Repo.Owner, r.PR.Repo.Name,
			http.MethodPost, path, payload, &created); err != nil {
			return fmt.Errorf("creating preview comment on %s: %w", r.PR, err)
		}
		c.rememberComment(r.PreviewID, created.ID)
		c.log.Info("created preview comment", "pr", r.PR.String(), "comment", created.ID, "state", r.State)
		return nil
	}

	// PUT, not PATCH. Bitbucket replaces the comment body outright.
	path := fmt.Sprintf("/2.0/repositories/%s/%s/pullrequests/%d/comments/%d",
		r.PR.Repo.Owner, r.PR.Repo.Name, r.PR.Number, existing)
	err := c.do(ctx, r.PR.Repo.Owner, r.PR.Repo.Name, http.MethodPut, path, payload, nil)
	if IsNotFound(err) {
		// Somebody deleted it. Post a new one rather than 404ing on this and every
		// later report for the rest of the process.
		c.log.Warn("the preview comment was deleted; posting a new one",
			"pr", r.PR.String(), "comment", existing)
		c.forgetComment(r.PreviewID)
		return c.upsertComment(ctx, r)
	}
	if err != nil {
		return fmt.Errorf("updating preview comment on %s: %w", r.PR, err)
	}
	c.log.Debug("updated preview comment", "pr", r.PR.String(), "comment", existing, "state", r.State)
	return nil
}

// findComment locates docpreview's comment by its marker.
//
// Two Bitbucket-specific filters, and the first one is a "must not" with a test.
// The list endpoint returns soft-deleted comments *with their bodies intact*, so a
// marker match against one produces an update to a comment nobody can see — the
// preview would report success while showing nothing. `pending` comments are
// unpublished review drafts; docpreview never creates one, and skipping them costs
// nothing and removes one more way to match something invisible.
func (c *Client) findComment(ctx context.Context, pr model.PullRequest, previewID string) (int64, error) {
	path := fmt.Sprintf("/2.0/repositories/%s/%s/pullrequests/%d/comments?pagelen=100",
		pr.Repo.Owner, pr.Repo.Name, pr.Number)

	for page := 0; page < maxChangedFilePages && path != ""; page++ {
		var envelope struct {
			Values []struct {
				ID      int64 `json:"id"`
				Deleted bool  `json:"deleted"`
				Pending bool  `json:"pending"`
				Content struct {
					Raw string `json:"raw"`
				} `json:"content"`
			} `json:"values"`
			Next string `json:"next"`
		}
		if err := c.do(ctx, pr.Repo.Owner, pr.Repo.Name,
			http.MethodGet, path, nil, &envelope); err != nil {
			return 0, fmt.Errorf("listing comments on %s: %w", pr, err)
		}
		for _, cm := range envelope.Values {
			if cm.Deleted || cm.Pending {
				continue
			}
			if scm.HasMarker(cm.Content.Raw, previewID) {
				return cm.ID, nil
			}
		}

		next, err := c.samePage(envelope.Next)
		if err != nil {
			return 0, nil
		}
		path = next
	}
	return 0, nil
}

// postBuildStatus records the build against the commit.
//
// Bitbucket's equivalent of a check run, and narrower in three ways worth knowing:
//
//   - There is no `neutral`. A skipped build has to be either SUCCESSFUL, which
//     claims a preview exists, or STOPPED, which reads as a problem. STOPPED with a
//     description saying why is the lesser lie.
//   - There is no `queued`. A preview waiting in the queue is reported INPROGRESS;
//     the dashboard and the comment are where "waiting" and "running" are
//     distinguishable.
//   - `url` is **required**, so this cannot be called at all without a reachable
//     address. Before the preview exists the only candidate is the dashboard, which
//     under a loopback-only daemon has no address a reviewer could open — hence
//     skipping entirely rather than sending a placeholder.
func (c *Client) postBuildStatus(ctx context.Context, r scm.Report) error {
	target := r.URL
	if target == "" {
		c.log.Debug("skipping build status: no reachable URL yet",
			"pr", r.PR.String(), "state", r.State)
		return nil
	}
	if r.Commit == "" {
		return nil
	}

	state, description := buildStatus(r)
	path := fmt.Sprintf("/2.0/repositories/%s/%s/commit/%s/statuses/build",
		r.PR.Repo.Owner, r.PR.Repo.Name, r.Commit)

	// POST creates or replaces: Bitbucket keys a build status on (commit, key), so
	// re-posting the same key is the update. No find-then-patch, which is one fewer
	// request than the GitHub check run needs.
	return c.do(ctx, r.PR.Repo.Owner, r.PR.Repo.Name, http.MethodPost, path, map[string]string{
		"key":         statusKey,
		"state":       state,
		"name":        "Documentation preview",
		"description": description,
		"url":         target,
	}, nil)
}

func buildStatus(r scm.Report) (state, description string) {
	switch r.State {
	case scm.StateReady:
		return "SUCCESSFUL", "The preview is published."
	case scm.StateFailed:
		return "FAILED", "The build failed. See the docpreview comment."
	case scm.StateSkipped:
		// Bitbucket has no neutral state; see postBuildStatus.
		return "STOPPED", "No documentation changed, so nothing was built."
	default:
		return "INPROGRESS", "Building the documentation preview."
	}
}

// Retract removes docpreview's comment.
//
// The build status is deliberately left alone, for the same reason the GitHub client
// leaves the check run: erasing what happened to a specific commit is revisionist.
func (c *Client) Retract(ctx context.Context, pr model.PullRequest) error {
	previewID := pr.PreviewID()

	id, ok := c.knownComment(previewID)
	if !ok {
		var err error
		id, err = c.findComment(ctx, pr, previewID)
		if err != nil {
			return err
		}
	}
	if id == 0 {
		return nil
	}

	path := fmt.Sprintf("/2.0/repositories/%s/%s/pullrequests/%d/comments/%d",
		pr.Repo.Owner, pr.Repo.Name, pr.Number, id)
	if err := c.do(ctx, pr.Repo.Owner, pr.Repo.Name,
		http.MethodDelete, path, nil, nil); err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting preview comment on %s: %w", pr, err)
	}
	c.forgetComment(previewID)
	return nil
}

// OpenPullRequests lists what is open on a repository.
//
// The optional scm.PullRequestLister, so adding a project from the dashboard can
// queue what already exists instead of waiting for somebody to push.
func (c *Client) OpenPullRequests(ctx context.Context, repo model.Repo) ([]model.PullRequest, error) {
	path := fmt.Sprintf("/2.0/repositories/%s/%s/pullrequests?state=OPEN&pagelen=50",
		repo.Owner, repo.Name)

	var out []model.PullRequest
	for page := 0; page < maxChangedFilePages && path != ""; page++ {
		var envelope struct {
			Values []pullRequestObject `json:"values"`
			Next   string              `json:"next"`
		}
		if err := c.do(ctx, repo.Owner, repo.Name,
			http.MethodGet, path, nil, &envelope); err != nil {
			return nil, fmt.Errorf("listing open pull requests on %s: %w", repo.String(), err)
		}
		for _, p := range envelope.Values {
			pr, ok := c.pullRequestFrom(ctx, repo, p)
			if !ok {
				continue
			}
			out = append(out, pr)
		}

		next, err := c.samePage(envelope.Next)
		if err != nil {
			break
		}
		path = next
	}
	return out, nil
}

// DefaultBranch names the repository's main branch and the commit at its tip.
//
// Bitbucket calls it `mainbranch`, not `default_branch`, and reports only its name — so the
// tip is a second call, as it is on GitHub.
func (c *Client) DefaultBranch(ctx context.Context, repo model.Repo) (string, string, error) {
	var out struct {
		MainBranch *struct {
			Name string `json:"name"`
		} `json:"mainbranch"`
	}
	path := fmt.Sprintf("/2.0/repositories/%s/%s", repo.Owner, repo.Name)
	if err := c.do(ctx, repo.Owner, repo.Name, http.MethodGet, path, nil, &out); err != nil {
		return "", "", fmt.Errorf("reading %s: %w", repo.String(), err)
	}
	// Null for a repository with no commits, rather than absent — so this is a nil check
	// and not an empty-string one.
	if out.MainBranch == nil || out.MainBranch.Name == "" {
		return "", "", fmt.Errorf("%s reports no main branch; is it empty?", repo.String())
	}

	commit, err := c.BranchTip(ctx, repo, out.MainBranch.Name)
	if err != nil {
		return "", "", err
	}
	return out.MainBranch.Name, commit, nil
}

// BranchTip is the commit a branch currently points at.
//
// The hash here is already 40 characters — `refs/branches` reports the full one, unlike the
// 12-character abbreviation in a webhook payload — so nothing needs resolving.
func (c *Client) BranchTip(ctx context.Context, repo model.Repo, branch string) (string, error) {
	var out struct {
		Target struct {
			Hash string `json:"hash"`
		} `json:"target"`
	}
	// Escaped: a branch name contains slashes often enough that `feature/pricing` would
	// otherwise address a path one segment deeper and 404.
	path := fmt.Sprintf("/2.0/repositories/%s/%s/refs/branches/%s",
		repo.Owner, repo.Name, url.PathEscape(branch))
	if err := c.do(ctx, repo.Owner, repo.Name, http.MethodGet, path, nil, &out); err != nil {
		return "", fmt.Errorf("reading branch %s on %s: %w", branch, repo.String(), err)
	}
	if out.Target.Hash == "" {
		return "", fmt.Errorf("branch %s on %s reports no commit", branch, repo.String())
	}
	return out.Target.Hash, nil
}

// pullRequestFrom converts an API pull request object, resolving its abbreviated
// commit hash. Reports false for anything that must not be built.
func (c *Client) pullRequestFrom(ctx context.Context, repo model.Repo, p pullRequestObject) (model.PullRequest, bool) {
	if p.Source.Repository == nil ||
		!strings.EqualFold(p.Source.Repository.FullName, repo.Slug()) {
		// A fork, or a source repository the payload did not name. Absent counts as
		// untrusted: building it would run a stranger's build scripts under our
		// credential.
		return model.PullRequest{}, false
	}
	sha, err := c.resolveCommit(ctx, repo, p.Source.Commit.Hash)
	if err != nil {
		c.log.Warn("could not resolve the head commit",
			"repo", repo.String(), "pr", p.ID, "hash", p.Source.Commit.Hash, "error", err)
		return model.PullRequest{}, false
	}
	return model.PullRequest{
		Repo:       repo,
		Number:     p.ID,
		Branch:     p.Source.Branch.Name,
		HeadSHA:    sha,
		BaseBranch: p.Destination.Branch.Name,
	}, true
}

// resolveCommit turns Bitbucket's twelve-character hash into the full forty.
//
// This is the finding most likely to cost a day if it is met during implementation
// rather than read about here. A pull request object serializes
// `source.commit.hash` abbreviated:
//
//	"source": { "commit": { "hash": "a4fd6c9db194" } }
//
// Git resolves an unambiguous abbreviation, so the clone and the checkout work and
// nothing appears wrong. What breaks is every comparison: supersede logic keyed on
// HeadSHA compares twelve characters against forty and finds them unequal, a build
// status posted against the short hash and one against the long hash look like two
// statuses on one commit, and the comment shows a reviewer a hash that does not
// match their own `git log`.
//
// So it is resolved here, at parse time, and a failure is an error rather than a
// fallback to the abbreviation. One extra request on the path that must answer 202
// quickly is a real cost, and still the right trade against a preview identity that
// disagrees with itself.
func (c *Client) resolveCommit(ctx context.Context, repo model.Repo, hash string) (string, error) {
	if hash == "" {
		return "", errors.New("the payload carried no commit hash")
	}
	// Already full. Cheap to check, and it makes the day Bitbucket stops abbreviating
	// a no-op rather than a wasted request per delivery.
	if len(hash) == 40 {
		return hash, nil
	}

	var commit struct {
		Hash string `json:"hash"`
	}
	path := fmt.Sprintf("/2.0/repositories/%s/%s/commit/%s", repo.Owner, repo.Name, hash)
	if err := c.do(ctx, repo.Owner, repo.Name, http.MethodGet, path, nil, &commit); err != nil {
		return "", err
	}
	if len(commit.Hash) != 40 {
		return "", fmt.Errorf("bitbucket returned %q for commit %s, which is not a full hash",
			commit.Hash, hash)
	}
	return commit.Hash, nil
}

func (c *Client) knownComment(previewID string) (int64, bool) {
	c.commentsMu.Lock()
	defer c.commentsMu.Unlock()
	id, ok := c.comments[previewID]
	return id, ok
}

func (c *Client) rememberComment(previewID string, id int64) {
	c.commentsMu.Lock()
	defer c.commentsMu.Unlock()
	if c.comments == nil {
		c.comments = map[string]int64{}
	}
	c.comments[previewID] = id
}

func (c *Client) forgetComment(previewID string) {
	c.commentsMu.Lock()
	defer c.commentsMu.Unlock()
	delete(c.comments, previewID)
}

// do issues a request as the credential belonging to one repository, retrying what is
// worth retrying.
//
// owner and repo are parameters rather than being parsed back out of the path because the
// credential depends on them and a path is not a reliable place to find them — /2.0/user
// has none at all, and a mis-parse would silently authenticate as the wrong identity.
// Empty owner means "no repository", which uses the global credential.
//
// Written fresh rather than shared with the GitHub client: the error envelope and the
// rate-limit signalling are different enough that a shared implementation would be a
// switch on platform wearing a trenchcoat.
func (c *Client) do(ctx context.Context, owner, repo, method, path string, in, out any) error {
	const attempts = 3

	cred := c.creds
	if owner != "" {
		var err error
		cred, err = c.credentialFor(owner, repo)
		if err != nil {
			return err
		}
	}
	if cred.empty() {
		return fmt.Errorf("no Bitbucket credential is stored; " +
			"add one to the project on /projects, or store a workspace-wide one on /secrets")
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := c.doOnce(ctx, cred, method, path, in, out)
		if err == nil {
			return nil
		}
		lastErr = err

		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() || attempt == attempts {
			return err
		}

		wait := time.Duration(attempt) * time.Second
		if apiErr.RetryAfter > 0 {
			wait = apiErr.RetryAfter
		}
		c.log.Warn("retrying bitbucket request",
			"method", method, "path", path, "status", apiErr.Status, "in", wait)
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
	return lastErr
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) doOnce(ctx context.Context, cred credential, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.cfg.APIBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", string(cred.authz.Reveal()))
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return errorFromResponse(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// APIError is a non-2xx answer from Bitbucket.
type APIError struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("bitbucket returned %d", e.Status)
	}
	return fmt.Sprintf("bitbucket returned %d: %s", e.Status, e.Message)
}

// Retryable reports whether trying again could help.
//
// 429 is the rate limit and 5xx is Bitbucket having a moment. Everything else is a
// decision — a wrong scope, a missing repository, a malformed body — and repeating
// it three times just makes three identical refusals.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// errorFromResponse reads Bitbucket's error envelope.
//
// Its shape is `{"type": "error", "error": {"message": "...", "detail": "..."}}`,
// which is neither GitHub's `{"message": ...}` nor a bare string — so this is
// written fresh rather than shared. A body that does not parse is not an error in
// itself: the status is the fact worth keeping.
func errorFromResponse(resp *http.Response) error {
	out := &APIError{Status: resp.StatusCode}

	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			out.RetryAfter = time.Duration(secs) * time.Second
		}
	}

	// Capped. An error body is a sentence; anything larger is a page of HTML from
	// something in front of the API, and reading all of it into a log line is how a
	// 502 fills a disk.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		out.Message = envelope.Error.Message
		if envelope.Error.Detail != "" {
			out.Message += ": " + envelope.Error.Detail
		}
		return out
	}

	out.Message = strings.TrimSpace(string(raw))
	if len(out.Message) > 300 {
		out.Message = out.Message[:300] + "…"
	}
	return out
}

// IsNotFound reports whether err is a 404, which several callers treat as an
// ordinary answer rather than a failure.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}
