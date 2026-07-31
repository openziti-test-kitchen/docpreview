package model

import "testing"

func TestSanitizeNameCleanBranchesPassThrough(t *testing.T) {
	// A branch that is already a valid DNS label must survive untouched. This
	// is the case that decides whether a URL is readable, and it is the reason
	// the hash suffix is conditional rather than unconditional.
	for _, name := range []string{"main", "docs", "v2", "release-1-2-3"} {
		if got := SanitizeName(name); got != name {
			t.Errorf("SanitizeName(%q) = %q, want it unchanged", name, got)
		}
	}
}

func TestSanitizeNameRewritesIllegalCharacters(t *testing.T) {
	tests := []struct {
		branch string
		prefix string
	}{
		{"feature/new-guide", "feature-new-guide-"},
		{"JIRA-123_fix", "jira-123-fix-"},
		{"docs/api v2", "docs-api-v2-"},
		{"--leading--", "leading-"},
	}
	for _, tt := range tests {
		got := SanitizeName(tt.branch)
		if len(got) <= len(tt.prefix) || got[:len(tt.prefix)] != tt.prefix {
			t.Errorf("SanitizeName(%q) = %q, want prefix %q", tt.branch, got, tt.prefix)
		}
	}
}

func TestSanitizeNameDistinguishesBranchesThatCollapseTogether(t *testing.T) {
	// "feature/foo" and "feature_foo" both reduce to "feature-foo". If they
	// produced the same name, two open pull requests would fight over one
	// hostname and each rebuild would silently steal the other's URL.
	a := SanitizeName("feature/foo")
	b := SanitizeName("feature_foo")
	if a == b {
		t.Fatalf("distinct branches produced the same name: %q", a)
	}
}

func TestSanitizeNameIsStable(t *testing.T) {
	for range 10 {
		if got, want := SanitizeName("feature/some thing"), SanitizeName("feature/some thing"); got != want {
			t.Fatalf("SanitizeName is not deterministic: %q vs %q", got, want)
		}
	}
}

func TestSanitizeNameTruncatesLongBranches(t *testing.T) {
	long := "feature/a-really-quite-extraordinarily-long-branch-name-that-nobody-should-have-written"
	got := SanitizeName(long)
	if len(got) > maxLabelLen {
		t.Fatalf("SanitizeName(%q) = %q, length %d exceeds %d", long, got, len(got), maxLabelLen)
	}
}

func TestSanitizeNameHandlesPunctuationOnlyBranch(t *testing.T) {
	got := SanitizeName("///")
	if got == "" {
		t.Fatal("SanitizeName produced an empty label")
	}
}

func TestPreviewIDIgnoresBranchAndCommit(t *testing.T) {
	// The preview ID must survive a force-push and a branch rename, because it
	// is what finds the comment to edit. If it moved, every push would post a
	// new comment.
	base := PullRequest{
		Repo:   Repo{Platform: PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 42, Branch: "feature/a", HeadSHA: "aaaa",
	}
	moved := base
	moved.Branch = "feature/b"
	moved.HeadSHA = "bbbb"

	if base.PreviewID() != moved.PreviewID() {
		t.Fatalf("preview ID changed with the branch: %q vs %q", base.PreviewID(), moved.PreviewID())
	}
}

// TestPreviewIDIsStableForPullRequests is a pin, not a behaviour test.
//
// This id is the primary key of every preview, build and comment row, the tag on every
// remote share, and the directory name of every artifact and log. Branch previews needed
// the branch in the hashed input; folding it in for *numbered* pull requests as well would
// silently orphan all of that — every restored preview reaped as an orphan, every existing
// comment re-posted as a duplicate. The literal is here so that change cannot be made
// quietly.
func TestPreviewIDIsStableForPullRequests(t *testing.T) {
	pr := PullRequest{
		Repo:   Repo{Platform: PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 42, Branch: "feature/a", HeadSHA: "aaaa",
	}
	if got, want := pr.PreviewID(), "03f53fd3e24d"; got != want {
		t.Errorf("preview id for %s is %q, want %q — every stored row and remote share "+
			"is keyed on the old value", pr, got, want)
	}
}

func TestBranchPreviewsAreKeyedOnTheBranch(t *testing.T) {
	repo := Repo{Platform: PlatformGitHub, Owner: "acme", Name: "docs"}
	main := PullRequest{Repo: repo, Branch: "main"}
	release := PullRequest{Repo: repo, Branch: "release-8.2"}

	if !main.IsBranch() {
		t.Fatal("a pull request with no number is not recognised as a branch preview")
	}
	// Without the branch in the seed, every branch of one repository hashes to the same
	// id — so the second branch built would take over the first one's preview, its share
	// and its artifacts.
	if main.PreviewID() == release.PreviewID() {
		t.Error("two branches of one repository share a preview id")
	}
	// And a branch preview must not collide with pull request 0's, which cannot exist, or
	// with any real pull request's.
	pr := PullRequest{Repo: repo, Number: 1, Branch: "main"}
	if main.PreviewID() == pr.PreviewID() {
		t.Error("a branch preview collides with a pull request on the same branch")
	}

	// The string form has to say which it is. "#0" reads as a bug in a log line.
	if got, want := main.String(), "github:acme/docs@main"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	// And the link goes to the branch, since there is no pull request to open.
	if got, want := main.WebURL(), "https://github.com/acme/docs/tree/main"; got != want {
		t.Errorf("WebURL() = %q, want %q", got, want)
	}
}

func TestPreviewIDSeparatesPullRequests(t *testing.T) {
	a := PullRequest{Repo: Repo{Platform: PlatformGitHub, Owner: "acme", Name: "docs"}, Number: 1}
	b := PullRequest{Repo: Repo{Platform: PlatformGitHub, Owner: "acme", Name: "docs"}, Number: 2}
	c := PullRequest{Repo: Repo{Platform: PlatformBitbucket, Owner: "acme", Name: "docs"}, Number: 1}

	if a.PreviewID() == b.PreviewID() {
		t.Error("different pull request numbers produced the same preview ID")
	}
	if a.PreviewID() == c.PreviewID() {
		t.Error("different platforms produced the same preview ID")
	}
}
