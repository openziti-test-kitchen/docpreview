package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// pullRequestObject is the subset of a Bitbucket pull request docpreview reads.
//
// The same object appears in a webhook body under `pullrequest` and in the REST
// response for a pull request, which is why it is one type used by both paths.
type pullRequestObject struct {
	ID     int `json:"id"`
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			// Twelve characters, not forty. See resolveCommit.
			Hash string `json:"hash"`
		} `json:"commit"`

		// Repository is nilable, and absent counts as untrusted. GitHub's client has
		// a known gap here — a null head repo from a deleted fork skips the fork
		// refusal — and reproducing it on a new platform would be choosing to.
		Repository *struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	} `json:"source"`
	Destination struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Repository *struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	} `json:"destination"`

	// Draft exists and is deliberately ignored. Drafts are where documentation gets
	// written, so the GitHub rule transfers: do not filter on it. Named here so that
	// ignoring it is a decision on the record rather than an omission.
	Draft bool `json:"draft"`
}

// webhookPayload is a pull request event delivery.
type webhookPayload struct {
	PullRequest pullRequestObject `json:"pullrequest"`
	Repository  struct {
		// Slug is URL-safe; Name is the display name and can contain spaces, so the
		// slug is what identifies a repository.
		Slug      string `json:"slug"`
		FullName  string `json:"full_name"`
		Workspace struct {
			Slug string `json:"slug"`
		} `json:"workspace"`
		Links struct {
			// An array to search by name, not a scalar. The ssh entry is in here too
			// and its position is not a promise.
			Clone []struct {
				Name string `json:"name"`
				Href string `json:"href"`
			} `json:"clone"`
		} `json:"links"`
	} `json:"repository"`
}

// VerifyWebhook authenticates a delivery and normalizes it into zero or one events.
//
// Verification happens before the body is parsed, and the comparison is constant
// time. This endpoint is internet-facing by design, so the JSON parser must never
// see bytes that have not been authenticated.
//
// The header is `X-Hub-Signature` — GitHub's name *without* the -256 suffix, which
// on GitHub carries a legacy SHA-1 digest. Same name, different algorithm, so the
// two clients must keep reading their own header rather than sharing a lookup.
// Getting this wrong is a 401 on every delivery with no diagnostic, by design.
func (c *Client) VerifyWebhook(ctx context.Context, headers map[string][]string, body []byte) ([]scm.Event, error) {
	h := http.Header(headers)

	sig := h.Get("X-Hub-Signature")
	if sig == "" {
		// A blank Secret field in Bitbucket's webhook form produces deliveries with
		// no signature at all — a webhook that works perfectly and carries no
		// authentication. Accepting those would turn one empty form field into an
		// unauthenticated build trigger, so this is a hard failure that names the
		// field to fill in.
		return nil, fmt.Errorf("%w: no X-Hub-Signature header — the webhook has no secret set "+
			"(add one in Repository settings → Webhooks and store the same value with "+
			"'docpreview vault set bitbucket.webhook_secret')", scm.ErrBadSignature)
	}
	if !scm.VerifyHMACSHA256(c.webhookSecret.Reveal(), body, sig) {
		return nil, scm.ErrBadSignature
	}

	event := h.Get("X-Event-Key")
	delivery := h.Get("X-Request-UUID")

	// Logged, never acted on. A number above 1 means Bitbucket believes earlier
	// attempts failed, which given that a delivery is acknowledged before the work
	// starts means the tunnel dropped or something upstream is slow. It must not be
	// used to deduplicate: store.Enqueue already collapses repeat work for one
	// preview, and discarding attempt 2 would discard the retry of a delivery that
	// genuinely never landed.
	if n := h.Get("X-Attempt-Number"); n != "" && n != "1" {
		c.log.Info("bitbucket is retrying a delivery",
			"attempt", n, "event", event, "delivery", delivery)
	}

	switch event {
	case "pullrequest:created", "pullrequest:updated":
		// `updated` is not GitHub's `synchronize`: it also fires for a title change,
		// a description edit, a reviewer change and a retarget. Mapping it to a build
		// therefore rebuilds a site because somebody fixed a typo in a description.
		//
		// Accepted deliberately. Suppressing it means remembering the last SHA built
		// per pull request, and the client is the wrong place for that state — it
		// would have to survive a restart, which means the store, which means a
		// hosted client that reads the database to decide what a webhook means. The
		// machinery to absorb the churn already exists: Enqueue replaces a pending
		// job for the same preview and a newer build cancels the one in flight. The
		// cost is a wasted npm install, not a wrong answer.
		return c.buildEvent(ctx, body, delivery)

	case "pullrequest:fulfilled", "pullrequest:rejected":
		return c.teardownEvent(body, delivery)

	default:
		// Bitbucket's webhook form makes it easy to over-select, and pushes,
		// approvals and comment events all arrive here. Not an error.
		c.log.Debug("ignoring bitbucket event", "event", event, "delivery", delivery)
		return nil, nil
	}
}

func (c *Client) buildEvent(ctx context.Context, body []byte, delivery string) ([]scm.Event, error) {
	p, repo, err := parsePayload(body)
	if err != nil {
		return nil, err
	}

	// Fork refusal, and this is the one piece of platform-specific logic that must
	// not be got wrong: building a fork means cloning and running a stranger's build
	// scripts under our credential.
	//
	// Compare source and destination full names. Not `repository.parent`, which
	// answers "is this repository a fork" rather than "is this pull request from a
	// fork" — confusing the two refuses every pull request on a forked repository
	// while still building cross-repository ones.
	if p.Source.Repository == nil {
		c.log.Warn("refusing a pull request whose source repository is absent",
			"pr", fmt.Sprintf("%s#%d", repo.String(), p.ID), "delivery", delivery)
		return nil, nil
	}
	if !strings.EqualFold(p.Source.Repository.FullName, repo.Slug()) {
		c.log.Warn("refusing to build a fork pull request",
			"pr", fmt.Sprintf("%s#%d", repo.String(), p.ID),
			"source_repo", p.Source.Repository.FullName, "delivery", delivery)
		return nil, nil
	}

	if p.Source.Branch.Name == "" {
		return nil, fmt.Errorf("bitbucket payload for %s#%d had no source branch", repo.String(), p.ID)
	}

	// One extra request, on the path that must answer quickly. See resolveCommit for
	// why the abbreviation cannot be carried instead.
	sha, err := c.resolveCommit(ctx, repo, p.Source.Commit.Hash)
	if err != nil {
		return nil, fmt.Errorf("resolving the head commit of %s#%d: %w", repo.String(), p.ID, err)
	}

	pr := model.PullRequest{
		Repo:       repo,
		Number:     p.ID,
		Branch:     p.Source.Branch.Name,
		HeadSHA:    sha,
		BaseBranch: p.Destination.Branch.Name,
	}
	return []scm.Event{{Kind: scm.EventBuild, PR: pr, Delivery: delivery}}, nil
}

// teardownEvent needs no commit resolution: nothing is built, and the preview is
// identified by repository and number.
func (c *Client) teardownEvent(body []byte, delivery string) ([]scm.Event, error) {
	p, repo, err := parsePayload(body)
	if err != nil {
		return nil, err
	}
	pr := model.PullRequest{
		Repo:       repo,
		Number:     p.ID,
		Branch:     p.Source.Branch.Name,
		HeadSHA:    p.Source.Commit.Hash,
		BaseBranch: p.Destination.Branch.Name,
	}
	return []scm.Event{{Kind: scm.EventTeardown, PR: pr, Delivery: delivery}}, nil
}

// parsePayload decodes a delivery and derives the repository identity.
func parsePayload(body []byte) (pullRequestObject, model.Repo, error) {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return pullRequestObject{}, model.Repo{}, fmt.Errorf("parsing bitbucket payload: %w", err)
	}

	owner := p.Repository.Workspace.Slug
	name := p.Repository.Slug
	// full_name is the fallback for both, because a captured delivery is the one part
	// of this payload shape not read off a live REST object — and "owner/repo" is
	// carried twice, so one spelling missing does not have to be fatal.
	if (owner == "" || name == "") && strings.Count(p.Repository.FullName, "/") == 1 {
		parts := strings.SplitN(p.Repository.FullName, "/", 2)
		if owner == "" {
			owner = parts[0]
		}
		if name == "" {
			name = parts[1]
		}
	}
	if owner == "" || name == "" {
		return pullRequestObject{}, model.Repo{},
			fmt.Errorf("bitbucket payload named no repository (workspace %q, slug %q, full_name %q)",
				p.Repository.Workspace.Slug, p.Repository.Slug, p.Repository.FullName)
	}
	if p.PullRequest.ID == 0 {
		return pullRequestObject{}, model.Repo{}, fmt.Errorf("bitbucket payload had no pull request id")
	}

	return p.PullRequest, model.Repo{
		Platform: model.PlatformBitbucket,
		Owner:    owner,
		Name:     name,
		// Deliberately not taken from links.clone. That href already carries a
		// username, whose identity depends on who asked, and inserting credentials
		// into it produces two `@` in the authority — a git failure whose message
		// contains the token. CloneURL builds the URL from owner and name instead.
		CloneURL: "https://bitbucket.org/" + owner + "/" + name + ".git",
	}, nil
}
