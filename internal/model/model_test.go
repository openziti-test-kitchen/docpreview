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
