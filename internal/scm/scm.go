// Package scm abstracts the source-control host: how a webhook is
// authenticated, how a repository is cloned, and how the result of a build is
// reported back to the pull request.
//
// GitHub and Bitbucket differ in almost every detail — App-installation JWTs
// versus basic auth with an API token, check runs versus build statuses,
// PATCH versus PUT to edit a comment — but they agree on the shape docpreview
// needs, which is what this package captures.
package scm

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
)

// State is the lifecycle stage of a preview, as reported to the pull request.
type State string

const (
	// StateQueued means the change was accepted and a build is pending.
	StateQueued State = "queued"
	// StateBuilding means a build is running.
	StateBuilding State = "building"
	// StateReady means the preview is live at Report.URL.
	StateReady State = "ready"
	// StateSkipped means nothing in the change was documentation.
	StateSkipped State = "skipped"
	// StateFailed means the build or the publish failed.
	StateFailed State = "failed"
)

// Report is everything docpreview knows about a preview at one moment. It is
// rendered into the pull request comment and, where the platform supports it,
// into a commit status.
type Report struct {
	PR        model.PullRequest
	PreviewID string
	State     State

	// URL is the public preview address. Set once State is StateReady.
	URL string

	// Name is the public hostname label, shown so a reviewer can tell at a
	// glance which branch a URL belongs to.
	Name string

	// Reason explains a skip or a failure in one line.
	//
	// Rendered into the comment only for a skip, where it is an explanation
	// written for the pull request author — "no documentation changes". A
	// failure's reason is an internal error string carrying host paths, internal
	// hostnames and API detail, and the comment is public on any public
	// repository. It stays in the daemon's log and the build log. See
	// RenderComment.
	Reason string

	// LogExcerpt is the tail of the build output. Kept out of the comment for
	// the same reason as a failure's Reason: it is the build host's stdout, and
	// what a build prints is not a decision anybody made about what to publish.
	//
	// Still carried on the Report because the dashboard and the local platform's
	// own pull request page render it, and neither is public.
	LogExcerpt string

	// DetailURL points at the build log for this preview, for a comment that
	// declines to quote it. Empty when the operator has configured no address
	// the link would work from, in which case the comment names the command
	// instead.
	DetailURL string

	// Commit is the SHA that produced this state.
	Commit string

	// Duration is how long the build took.
	Duration time.Duration

	// UpdatedAt is when this report was generated.
	UpdatedAt time.Time
}

// ErrBadSignature means a webhook delivery did not come from the platform it
// claims to, or the webhook secret is wrong.
//
// It lives here rather than in one platform's package because the ingress has to
// distinguish "this is not authentic" from "this is malformed" — the first is a
// 401 and gets logged as a rejection, the second is a 400 — and it cannot do
// that by asking whether the error came from GitHub. Before this moved, a bad
// signature on any other platform answered 400, which tells a caller probing for
// a valid secret that its guess was structurally fine.
var ErrBadSignature = errors.New("webhook signature verification failed")

// VerifyHMACSHA256 checks header, of the form "sha256=<hex>", against the
// HMAC-SHA256 of body keyed by secret.
//
// Here rather than in each platform's package because this was written twice
// already — byte for byte, in the github and local clients — and Bitbucket would
// have been the third place somebody could accidentally write `==` instead of
// hmac.Equal. Both hosted platforms spell the *value* the same way; what they
// disagree about is the header *name*, which stays the caller's business:
// GitHub's `X-Hub-Signature` is a legacy SHA-1 digest while Bitbucket's
// `X-Hub-Signature` is SHA-256, so a shared "find the signature header" helper
// would be the bug this one avoids.
//
// A method other than sha256 is a verification failure, not an unknown to wave
// through — Atlassian reserves the right to send something else, and an
// accepted-but-unverified delivery is a build trigger.
func VerifyHMACSHA256(secret, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	// Constant time: a byte-at-a-time comparison leaks the expected digest to a
	// caller willing to make a few thousand requests.
	return hmac.Equal(mac.Sum(nil), want)
}

// Client is everything docpreview needs from one source-control platform.
type Client interface {
	// Platform names the host this client talks to.
	Platform() model.Platform

	// VerifyWebhook authenticates a raw webhook delivery and returns the
	// events it describes. A single delivery yields zero or one events today,
	// but the slice keeps the door open for batched payloads and makes
	// "nothing actionable here" the natural, allocation-free answer.
	VerifyWebhook(ctx context.Context, headers map[string][]string, body []byte) ([]Event, error)

	// CloneURL returns a URL that git can clone without further configuration,
	// including any short-lived credential. The result is a secret: it embeds
	// a token, so it must never be logged or written into a comment.
	CloneURL(ctx context.Context, pr model.PullRequest) (string, error)

	// ChangedFiles lists the repository-relative paths the pull request
	// touches.
	//
	// This comes from the platform rather than from git on purpose. Computing
	// it locally would mean finding the merge base, which means fetching
	// enough history to have one — and the whole appeal of a preview builder
	// is that it can clone at depth 1 and start building in two seconds. Both
	// hosts already know the answer; asking them costs one request.
	ChangedFiles(ctx context.Context, pr model.PullRequest) ([]string, error)

	// Publish writes the report to the pull request, editing the existing
	// docpreview comment rather than adding another one.
	Publish(ctx context.Context, r Report) error

	// Retract removes docpreview's comment and any status it owns. Called when
	// a pull request is closed and its preview torn down.
	Retract(ctx context.Context, pr model.PullRequest) error
}

// EventKind classifies what a webhook delivery is asking docpreview to do.
type EventKind string

const (
	// EventBuild means "this pull request has new code; build it".
	EventBuild EventKind = "build"
	// EventTeardown means "this pull request is over; clean up".
	EventTeardown EventKind = "teardown"
)

// Event is a normalized webhook delivery.
type Event struct {
	Kind EventKind
	PR   model.PullRequest

	// Delivery is the platform's delivery ID, carried into logs so a report of
	// "nothing happened when I pushed" can be traced to a specific request.
	Delivery string
}

// RepoChecker is implemented by clients that can verify their credential reaches one
// repository, returning a sentence describing what they found.
//
// Optional, and needed by exactly one platform for a reason worth stating: a GitHub App is
// installed on repositories, so the installation *is* the check and a token that cannot
// reach a repository does not exist. A Bitbucket access token is scoped to a repository at
// creation and pasted in by hand, so nothing has confirmed it reaches anything until
// something tries — and both ways of getting it wrong are quiet, one failing twenty seconds
// into a clone and the other at the comment after a successful build.
type RepoChecker interface {
	CheckRepo(ctx context.Context, repo model.Repo) (string, error)
}

// PullRequestLister is implemented by clients that can be asked what is open on a
// repository, rather than waiting to be told by a webhook.
//
// Optional rather than part of Client, because every other path into a build starts with
// a delivery and a platform that cannot answer this is still a usable platform. The one
// caller is adding a project from the dashboard: there is no delivery behind that, so
// without this a newly added repository does nothing until somebody happens to push, and
// "I added it and nothing happened" is indistinguishable from a broken install.
//
// Implementations must drop pull requests from forks, for the same reason the webhook
// path does: building a fork's branch runs its author's code on this host.
type PullRequestLister interface {
	OpenPullRequests(ctx context.Context, repo model.Repo) ([]model.PullRequest, error)
}

// BranchResolver is implemented by clients that can name a repository's default branch and
// the commit at its tip.
//
// Optional, like the two above, and needed for branch previews: the permanent preview of
// `main` cannot be built without knowing what "main" is called here and what it currently
// points at. Neither is guessable. A repository's default branch is `master` often enough
// that assuming `main` would silently build nothing on it, and a build needs a commit —
// passing an empty HeadSHA makes the clone take whatever the branch happens to be at, which
// is a different commit from the one the preview would claim to be showing.
//
// Both are returned together because both come from the same one or two API calls, and a
// caller that has one without the other cannot do anything with it.
type BranchResolver interface {
	// DefaultBranch names the repository's default branch and the commit at its tip.
	DefaultBranch(ctx context.Context, repo model.Repo) (branch, commit string, err error)

	// BranchTip is the same question for a branch the caller already knows the name of,
	// so rebuilding a branch preview does not have to assume the tip has not moved.
	BranchTip(ctx context.Context, repo model.Repo, branch string) (commit string, err error)
}

// MarkerStyle selects how the self-identifying marker is embedded in a comment
// body.
//
// The protocol is the same on every platform — list the comments, find ours, edit
// it — but the syntax that renders to nothing is not, and that is not a detail a
// platform can be trusted to get right by copying the other one.
type MarkerStyle int

const (
	// MarkerHTMLComment is `<!-- docpreview:<id> -->`, and is what GitHub gets.
	// Invisible there, because GitHub's renderer honours raw HTML.
	MarkerHTMLComment MarkerStyle = iota

	// MarkerLinkRef is `[docpreview]: #<id>`, a CommonMark link reference
	// definition: every conforming renderer consumes it as a definition and emits
	// nothing for it, whether or not raw HTML is allowed.
	//
	// It exists because Bitbucket Cloud escapes raw HTML, which turns
	// MarkerHTMLComment into a visible paragraph of literal text at the bottom of
	// the comment. That is not a guess — Vercel's own integration ships
	// `<!-- vercel-commit-author-required -->` and Bitbucket renders it as
	// `<p>&lt;!-- … --&gt;</p>` on a public pull request. Their *other* comment
	// uses a link reference definition and the line vanishes from the rendered
	// HTML entirely. See docs/design/15-bitbucket.md.
	MarkerLinkRef
)

// Marker returns the marker that identifies docpreview's own comment on a pull
// request, in the style GitHub wants.
//
// This is how the comment gets edited instead of duplicated. There is no platform
// API for "the comment I made earlier", and storing comment IDs in our database
// means a restored backup or a fresh install starts spamming. A marker in the body
// makes the comment self-identifying: list the comments, find ours, edit it. The
// preview ID is included so that two docpreview instances watching the same
// repository — staging and production, say — do not fight over one comment.
//
// Kept as the one-argument form because every caller that writes a comment today
// writes a GitHub one, and a platform that needs the other style should reach for
// MarkerFor rather than change what this means.
func Marker(previewID string) string {
	return MarkerFor(previewID, MarkerHTMLComment)
}

// MarkerFor renders a marker in the given style.
func MarkerFor(previewID string, s MarkerStyle) string {
	if s == MarkerLinkRef {
		// A `#`-prefixed destination, so that a renderer which does resolve the
		// label produces a same-page anchor rather than a request to somewhere.
		return fmt.Sprintf("[docpreview]: #%s", previewID)
	}
	return fmt.Sprintf("<!-- docpreview:%s -->", previewID)
}

// HasMarker reports whether a comment body is docpreview's, in any style.
//
// **Both styles, forever.** This is the load-bearing part of having two, and the
// reason introducing MarkerLinkRef is not just "change the constant": a daemon
// upgraded across a style change finds comments it wrote in the old one, and a
// matcher that knew only the new style would post a second comment on every open
// pull request at once — the exact failure the marker exists to prevent.
//
// So a style may be added here and none may ever be removed. Deleting a branch of
// this function is the one change that cannot be made safely, however dead the old
// style looks: the comments are on somebody else's server and outlive every release
// of this program.
func HasMarker(body, previewID string) bool {
	return strings.Contains(body, MarkerFor(previewID, MarkerHTMLComment)) ||
		strings.Contains(body, MarkerFor(previewID, MarkerLinkRef))
}
