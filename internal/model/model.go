// Package model holds the types shared across every layer of docpreview: the
// identity of a repository, the pull request being previewed, and the stable
// identifiers derived from them.
//
// The identifiers here are the backbone of the whole system. A preview must
// keep the same ID and the same public URL across every rebuild of a branch,
// because that is what lets us edit one PR comment in place instead of posting
// a new one on every push.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
)

// Platform identifies a source-control host.
type Platform string

const (
	PlatformGitHub    Platform = "github"
	PlatformBitbucket Platform = "bitbucket"

	// PlatformLocal is a git repository on this machine, standing in for a
	// hosted one. It exists so the whole flow — push, webhook, build, publish,
	// comment, edit the comment — can be exercised without a GitHub App, an
	// account, or an internet connection.
	PlatformLocal Platform = "local"
)

// Repo identifies a repository on a platform. Owner is the GitHub org/user or
// the Bitbucket workspace.
type Repo struct {
	Platform Platform `json:"platform"`
	Owner    string   `json:"owner"`
	Name     string   `json:"name"`
	CloneURL string   `json:"clone_url"`
}

// Slug renders the repository as "owner/name".
func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

// String renders the repository as "platform:owner/name".
func (r Repo) String() string { return string(r.Platform) + ":" + r.Slug() }

// WebURL is where a human reads this pull request.
//
// Composed rather than carried, because the two hosted platforms disagree about the path
// and neither sends a usable one everywhere: GitHub's webhook has `html_url` but a
// project scan does not, and a preview restored from the database has neither. The owner,
// the repository and the number are the three facts every path already has.
//
// Empty for the local platform, which has no web anything — the git simulator is bare
// repositories in a directory. A caller renders a link only when this is non-empty, which
// is the same rule the preview URL follows.
func (pr PullRequest) WebURL() string {
	// A branch preview has no pull request, so this points at the branch instead. The
	// dashboard renders whatever this returns beside the preview, and for `main` the useful
	// destination is the code — the alternative was an empty space where every other row
	// carries a link.
	if pr.IsBranch() {
		switch pr.Repo.Platform {
		case PlatformGitHub:
			return fmt.Sprintf("https://github.com/%s/%s/tree/%s",
				pr.Repo.Owner, pr.Repo.Name, pr.Branch)
		case PlatformBitbucket:
			return fmt.Sprintf("https://bitbucket.org/%s/%s/src/%s",
				pr.Repo.Owner, pr.Repo.Name, pr.Branch)
		default:
			return ""
		}
	}

	switch pr.Repo.Platform {
	case PlatformGitHub:
		return fmt.Sprintf("https://github.com/%s/%s/pull/%d", pr.Repo.Owner, pr.Repo.Name, pr.Number)
	case PlatformBitbucket:
		// Bitbucket's is `pull-requests`, plural and hyphenated. Getting this wrong
		// produces a 404 that looks like a deleted pull request.
		return fmt.Sprintf("https://bitbucket.org/%s/%s/pull-requests/%d",
			pr.Repo.Owner, pr.Repo.Name, pr.Number)
	default:
		return ""
	}
}

// PullRequest is the unit of work. Every build, preview, and comment is scoped
// to exactly one of these.
type PullRequest struct {
	Repo Repo `json:"repo"`

	// Number is the PR number on GitHub, or the pull request ID on Bitbucket.
	Number int `json:"number"`

	// Branch is the head branch, e.g. "feature/new-install-guide". This is the
	// source of the public preview hostname.
	Branch string `json:"branch"`

	// HeadSHA is the commit being built.
	HeadSHA string `json:"head_sha"`

	// BaseBranch is the merge target, used to compute the changed-file set.
	BaseBranch string `json:"base_branch"`

	// InstallationID is the GitHub App installation that delivered the event.
	// Unused on Bitbucket.
	InstallationID int64 `json:"installation_id,omitempty"`
}

// IsBranch reports whether this is a branch preview rather than a pull request's.
//
// Number 0 means it: no platform numbers a pull request zero, so the zero value of the
// field is free to mean "there is no pull request here". A branch preview is the current
// state of a branch — `main`, usually — and it exists because the thing an operator looks
// at most often is not under review.
//
// Three behaviours turn on this, and each is stated where it happens: nothing is reported
// to the platform (there is no pull request to comment on), the changed-file gate is
// skipped (there is no diff to take), and the pull-request-closed teardown cannot reach it.
func (pr PullRequest) IsBranch() bool { return pr.Number == 0 }

// PreviewID is a stable, collision-resistant identifier for the preview
// belonging to this pull request. It deliberately excludes the branch name and
// the commit SHA: a PR keeps one preview for its whole life, even if the head
// branch is force-pushed or renamed.
//
// A branch preview is the exception, and has to be: it has no number, so every branch of
// one repository would otherwise hash to the same id. There the branch *is* the identity,
// which is also why a branch preview does not survive a rename — unlike a pull request,
// there is nothing else to call it.
//
// **The pull request form must not change.** This id is the primary key of every preview,
// build and comment row, the tag on every remote share, and the directory name of every
// artifact and log. Adding the branch to the hashed input for numbered pull requests would
// silently orphan all of it — see TestPreviewIDIsStableForPullRequests.
func (pr PullRequest) PreviewID() string {
	seed := fmt.Sprintf("%s|%s|%s|%d",
		pr.Repo.Platform, pr.Repo.Owner, pr.Repo.Name, pr.Number)
	if pr.IsBranch() {
		seed += "|" + pr.Branch
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:12]
}

// String renders the pull request as "platform:owner/name#42", or
// "platform:owner/name@main" for a branch preview.
//
// Distinguishable on sight, because these go into every log line about a build and "#0"
// reads as a bug rather than as a branch.
func (pr PullRequest) String() string {
	if pr.IsBranch() {
		return fmt.Sprintf("%s@%s", pr.Repo, pr.Branch)
	}
	return fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
}

// ShortSHA abbreviates a commit for display.
//
// One definition, because three places render the same commit and they have to
// agree: the pull request comment, the dashboard's activity feed, and the build
// log's filename. Two copies of this meant that changing the length would have
// made the comment and the dashboard disagree about the same build.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// MaxLabelLen is the longest DNS label we will emit. RFC 1035 allows 63, but we
// leave headroom for the collision suffix and for the namespace's own domain.
//
// Exported because a caller adding a suffix of its own has to subtract from the same
// budget — see expose.RenderName, which reserves the instance suffix out of it rather than
// appending past it.
const MaxLabelLen = 48

// MaxPrefixLen bounds the per-installation name prefix.
//
// It is subtracted from every name an installation publishes, so a long one spends the
// label on itself. Twelve fits "aws-staging" and leaves a branch name recognisable.
const MaxPrefixLen = 12

// NamePrefix normalizes a configured prefix to the bare label.
//
// A trailing hyphen is accepted and dropped, because "a-" is the natural way to write a
// prefix and refusing it would be pedantry — the hyphen that joins it to the name is added
// where the name is composed, so carrying one here would produce "a--docs-main".
func NamePrefix(s string) string { return strings.TrimRight(strings.TrimSpace(s), "-") }

// ValidPrefix reports why a name prefix is unusable, or "" when it is fine.
//
// Lives here rather than in `expose`, which is where it is used, because `config` validates
// it at load and cannot import `expose` — the dependency runs the other way. Two copies of
// the rule would be one copy too many for something that decides public hostnames.
//
// Checked at load rather than at publish, because the cost of getting it wrong is paid
// remotely and one build at a time: an illegal prefix produces a name the exposer refuses,
// with the reason arriving as somebody else's 400 in the middle of a build log.
func ValidPrefix(s string) string {
	s = NamePrefix(s)
	if s == "" {
		return ""
	}
	if len(s) > MaxPrefixLen {
		return fmt.Sprintf("the name prefix is %d characters; keep it to %d, "+
			"since every preview hostname starts with it", len(s), MaxPrefixLen)
	}
	// Refused rather than sanitized. An operator who wrote `AWS_prod` should be told, not
	// silently given `aws-prod`: this string is part of the URL a reviewer bookmarks, and
	// quietly changing it is how two installations end up disagreeing about which is which.
	if s != SanitizeName(s) {
		return fmt.Sprintf("the name prefix %q is not a hostname label; "+
			"use lower-case letters, digits and hyphens, as in \"aws\"", s)
	}
	return ""
}

// SanitizeName converts an arbitrary git branch name into a single DNS label
// suitable for use as a public hostname component.
//
// Branch names routinely contain characters that are illegal in a hostname:
// "feature/JIRA-123_fix things" is a perfectly ordinary branch. We lowercase,
// replace every run of non-alphanumeric characters with a single hyphen, trim
// hyphens from the ends, and truncate.
//
// The mapping is lossy, so it is not injective: "feature/foo" and "feature_foo"
// both become "feature-foo". Truncation makes that worse for long names. To
// keep the result stable *and* unique, any input that does not survive the
// transformation unchanged gets a short hash of the original appended. Callers
// therefore get a name that is deterministic for a given branch and distinct
// from any other branch, without paying the hash cost on the common case of an
// already-clean name like "main".
func SanitizeName(branch string) string { return SanitizeNameTo(branch, MaxLabelLen) }

// SanitizeNameTo is SanitizeName with a caller-chosen budget.
//
// It exists so an instance suffix can be reserved out of the label *before* the name is
// truncated. Appending a suffix to an already-truncated name is the obvious approach and it
// is wrong twice over: the label can exceed the limit, and the collision hash that makes
// truncated names unique would be followed by the suffix rather than ending the label — so
// two long branches that collapsed to the same 48 characters would still collide, with the
// suffix doing nothing about it. See expose.RenderName.
func SanitizeNameTo(branch string, max int) string {
	if max < 8 {
		// Below this there is no room for the six-character collision hash plus a hyphen,
		// and a "name" that is only a truncation is not one. Callers validate their own
		// inputs; this is the floor that keeps the arithmetic below honest.
		max = 8
	}
	var b strings.Builder
	lastHyphen := true // suppresses a leading hyphen
	for _, r := range strings.ToLower(branch) {
		switch {
		case unicode.IsLetter(r) && r < unicode.MaxASCII, unicode.IsDigit(r) && r < unicode.MaxASCII:
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	clean := strings.Trim(b.String(), "-")

	if clean == branch && len(clean) <= max {
		return clean
	}

	sum := sha256.Sum256([]byte(branch))
	suffix := "-" + hex.EncodeToString(sum[:])[:6]

	if clean == "" {
		// A branch made entirely of punctuation. Unlikely, but the hash alone
		// is still a valid, stable label.
		return strings.TrimPrefix(suffix, "-")
	}
	if len(clean)+len(suffix) > max {
		clean = strings.Trim(clean[:max-len(suffix)], "-")
	}
	return clean + suffix
}
