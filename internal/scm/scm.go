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
	"errors"
	"fmt"
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

// Marker returns the hidden HTML comment that identifies docpreview's own
// comment on a pull request.
//
// This is how the comment gets edited instead of duplicated. There is no
// platform API for "the comment I made earlier", and storing comment IDs in our
// database means a restored backup or a fresh install starts spamming. A marker
// in the body makes the comment self-identifying: list the comments, find ours,
// edit it. The preview ID is included so that two docpreview instances watching
// the same repository — staging and production, say — do not fight over one
// comment.
func Marker(previewID string) string {
	return fmt.Sprintf("<!-- docpreview:%s -->", previewID)
}
