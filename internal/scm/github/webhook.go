package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// ErrBadSignature means the delivery did not come from GitHub, or the webhook
// secret is wrong.
//
// An alias for the shared sentinel, kept so that existing callers and tests that
// name the GitHub spelling still work. The value is what matters: the ingress
// tests it to choose 401 over 400, and every platform must return the same one.
var ErrBadSignature = scm.ErrBadSignature

// VerifyWebhook authenticates a delivery and normalizes it into zero or one
// events.
//
// Signature verification happens before the body is parsed, and the comparison
// is constant-time. Both details matter: this endpoint is reachable from the
// internet by design — that is the whole point of putting it behind a zrok
// share — so the JSON parser must never see bytes we have not authenticated,
// and the comparison must not leak the expected digest one byte at a time.
func (c *Client) VerifyWebhook(_ context.Context, headers map[string][]string, body []byte) ([]scm.Event, error) {
	h := http.Header(headers)

	sig := h.Get("X-Hub-Signature-256")
	if sig == "" {
		return nil, fmt.Errorf("%w: no X-Hub-Signature-256 header "+
			"(configure a webhook secret on the App and store it with 'docpreview vault set github.webhook_secret')",
			ErrBadSignature)
	}
	if !verifySignature(c.webhookSecret.Reveal(), body, sig) {
		return nil, ErrBadSignature
	}

	event := h.Get("X-GitHub-Event")
	delivery := h.Get("X-GitHub-Delivery")

	switch event {
	case "pull_request":
		return c.pullRequestEvent(body, delivery)
	case "push":
		return c.pushEvent(body, delivery)
	case "ping":
		c.log.Info("received github ping", "delivery", delivery)
		return nil, nil
	default:
		// Subscribing to more events than we handle is normal — GitHub's UI
		// makes it easy to over-select — so an unhandled event is a debug
		// line, not an error.
		c.log.Debug("ignoring github event", "event", event, "delivery", delivery)
		return nil, nil
	}
}

// verifySignature checks the HMAC-SHA256 of body against the sha256= header.
//
// A thin wrapper now that the comparison is shared. Kept as a function here because
// the *header name* is this platform's business — GitHub sends both spellings, and
// only `X-Hub-Signature-256` carries the SHA-256 digest — while the digest check
// itself was written identically in three packages.
func verifySignature(secret, body []byte, header string) bool {
	return scm.VerifyHMACSHA256(secret, body, header)
}

// pullRequestPayload is the subset of GitHub's pull_request event we read.
type pullRequestPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Draft bool `json:"draft"`
	} `json:"pull_request"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

func (c *Client) pullRequestEvent(body []byte, delivery string) ([]scm.Event, error) {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parsing pull_request payload: %w", err)
	}

	number := p.PullRequest.Number
	if number == 0 {
		number = p.Number
	}

	pr := model.PullRequest{
		Repo: model.Repo{
			Platform: model.PlatformGitHub,
			Owner:    p.Repository.Owner.Login,
			Name:     p.Repository.Name,
			CloneURL: p.Repository.CloneURL,
		},
		Number:         number,
		Branch:         p.PullRequest.Head.Ref,
		HeadSHA:        p.PullRequest.Head.SHA,
		BaseBranch:     p.PullRequest.Base.Ref,
		InstallationID: p.Installation.ID,
	}

	switch p.Action {
	case "opened", "synchronize", "reopened", "ready_for_review":
		// A pull request from a fork has a head repo that is not the base repo.
		// Building it would mean cloning and executing code from an untrusted
		// repository under our installation token, which is exactly the supply
		// chain hole this system must not open. Refuse, loudly enough to be
		// found in the logs.
		if head := p.PullRequest.Head.Repo; head != nil && !strings.EqualFold(head.FullName, pr.Repo.Slug()) {
			c.log.Warn("refusing to build a fork pull request",
				"pr", pr.String(), "head_repo", head.FullName, "delivery", delivery)
			return nil, nil
		}
		if pr.Branch == "" || pr.HeadSHA == "" {
			return nil, fmt.Errorf("pull_request payload for %s had no head ref or sha", pr)
		}
		return []scm.Event{{Kind: scm.EventBuild, PR: pr, Delivery: delivery}}, nil

	case "closed":
		return []scm.Event{{Kind: scm.EventTeardown, PR: pr, Delivery: delivery}}, nil

	case "edited":
		// The base branch may have changed — GitHub sends `edited` with a `changes.base`
		// for a retarget — but the head has not, so there is nothing new to build. Named
		// rather than left to the default so it is clear it was considered.
		return nil, nil

	case "converted_to_draft":
		// A draft is still worth previewing — drafts are where docs get
		// written — so this is deliberately not a teardown.
		return nil, nil

	default:
		c.log.Debug("ignoring pull_request action", "action", p.Action, "delivery", delivery)
		return nil, nil
	}
}

// pushPayload is the subset of GitHub's push event we read.
type pushPayload struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	Created bool   `json:"created"`

	Repository struct {
		Name          string `json:"name"`
		DefaultBranch string `json:"default_branch"`
		Owner         struct {
			// A push event's owner carries `name` where a pull request's carries
			// `login`. Both are read, because relying on the one this platform happens
			// to send today is how a payload change becomes a repository nobody can
			// identify.
			Login string `json:"login"`
			Name  string `json:"name"`
		} `json:"owner"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`

	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// pushEvent rebuilds the permanent branch preview when the default branch moves.
//
// # Why only the default branch
//
// The branch preview is a claim about what `main` looks like *now*, and without this it was a
// claim about what `main` looked like the last time somebody opened a pull request or restarted
// the daemon. That gap is the difference between "always current" and "current as of whenever".
//
// Every other branch is ignored, and that is not an omission. A push to a branch with a pull
// request open already arrives as `synchronize`, so building on both would build it twice; and a
// push to a branch with no pull request is somebody's work in progress, which nobody asked for a
// preview of and which would publish a URL for every branch anybody ever pushes.
//
// # What is refused
//
//   - Tags and anything that is not a branch ref. `refs/tags/v1` is not a branch.
//   - A deletion. `deleted: true` arrives with an all-zero `after`, and building it would clone
//     a ref that no longer exists.
//   - A repository whose default branch the payload does not name, which is not a shape GitHub
//     sends and is worth refusing rather than guessing at.
//
// The event carries number 0, which is what makes it a branch preview rather than a pull
// request's — see model.PullRequest.IsBranch. Everything downstream keys off that: no comment is
// posted, the TTL reaper skips it, and a closed pull request cannot tear it down.
func (c *Client) pushEvent(body []byte, delivery string) ([]scm.Event, error) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parsing push payload: %w", err)
	}

	const branchPrefix = "refs/heads/"
	if !strings.HasPrefix(p.Ref, branchPrefix) {
		c.log.Debug("ignoring a push that is not to a branch", "ref", p.Ref, "delivery", delivery)
		return nil, nil
	}
	branch := strings.TrimPrefix(p.Ref, branchPrefix)

	def := p.Repository.DefaultBranch
	if def == "" {
		c.log.Debug("ignoring a push whose payload names no default branch",
			"ref", p.Ref, "delivery", delivery)
		return nil, nil
	}
	if branch != def {
		// The common case by a wide margin, so it is debug rather than info: every push to
		// every feature branch on every watched repository arrives here.
		c.log.Debug("ignoring a push to a branch that is not the default",
			"branch", branch, "default", def, "delivery", delivery)
		return nil, nil
	}

	// A deletion of the default branch is not a thing that happens on purpose, and building
	// the all-zero sha it arrives with would fail in the clone.
	if p.Deleted || p.After == "" || strings.Trim(p.After, "0") == "" {
		c.log.Info("ignoring a delete of the default branch",
			"branch", branch, "delivery", delivery)
		return nil, nil
	}

	owner := p.Repository.Owner.Login
	if owner == "" {
		owner = p.Repository.Owner.Name
	}

	pr := model.PullRequest{
		Repo: model.Repo{
			Platform: model.PlatformGitHub,
			Owner:    owner,
			Name:     p.Repository.Name,
			CloneURL: p.Repository.CloneURL,
		},
		// Zero is the whole point: it is what makes this the branch preview.
		Number:         0,
		Branch:         branch,
		HeadSHA:        p.After,
		BaseBranch:     def,
		InstallationID: p.Installation.ID,
	}

	c.log.Info("the default branch moved; rebuilding its preview if there is one",
		"repo", pr.Repo.Slug(), "branch", branch, "commit", p.After, "delivery", delivery)
	// Refresh, not create. A push says the branch moved; it does not say anybody wants a
	// permanent preview of this repository — and the App can be installed on repositories
	// nobody has asked for one of.
	return []scm.Event{{Kind: scm.EventBuild, PR: pr, Delivery: delivery, Refresh: true}}, nil
}
