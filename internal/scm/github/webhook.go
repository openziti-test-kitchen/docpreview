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

	case "converted_to_draft":
		// A draft is still worth previewing — drafts are where docs get
		// written — so this is deliberately not a teardown.
		return nil, nil

	default:
		c.log.Debug("ignoring pull_request action", "action", p.Action, "delivery", delivery)
		return nil, nil
	}
}
