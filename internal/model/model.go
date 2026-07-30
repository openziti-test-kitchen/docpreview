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

// PreviewID is a stable, collision-resistant identifier for the preview
// belonging to this pull request. It deliberately excludes the branch name and
// the commit SHA: a PR keeps one preview for its whole life, even if the head
// branch is force-pushed or renamed.
func (pr PullRequest) PreviewID() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d",
		pr.Repo.Platform, pr.Repo.Owner, pr.Repo.Name, pr.Number)))
	return hex.EncodeToString(sum[:])[:12]
}

// String renders the pull request as "platform:owner/name#42".
func (pr PullRequest) String() string {
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

// maxLabelLen is the longest DNS label we will emit. RFC 1035 allows 63, but we
// leave headroom for the collision suffix and for the namespace's own domain.
const maxLabelLen = 48

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
func SanitizeName(branch string) string {
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

	if clean == branch && len(clean) <= maxLabelLen {
		return clean
	}

	sum := sha256.Sum256([]byte(branch))
	suffix := "-" + hex.EncodeToString(sum[:])[:6]

	if clean == "" {
		// A branch made entirely of punctuation. Unlikely, but the hash alone
		// is still a valid, stable label.
		return strings.TrimPrefix(suffix, "-")
	}
	if len(clean)+len(suffix) > maxLabelLen {
		clean = strings.Trim(clean[:maxLabelLen-len(suffix)], "-")
	}
	return clean + suffix
}
