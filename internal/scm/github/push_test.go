package github

import (
	"context"
	"fmt"
	"testing"

	"github.com/netfoundry/docpreview/internal/scm"
)

// pushBody builds a GitHub push delivery.
func pushBody(ref, after, defaultBranch string, deleted bool) []byte {
	return fmt.Appendf(nil, `{
	  "ref": %q,
	  "after": %q,
	  "deleted": %t,
	  "repository": {
	    "name": "docs",
	    "default_branch": %q,
	    "owner": {"name": "acme", "login": "acme"},
	    "clone_url": "https://github.com/acme/docs.git"
	  },
	  "installation": {"id": 42}
	}`, ref, after, deleted, defaultBranch)
}

func pushEvents(t *testing.T, body []byte) []scm.Event {
	t.Helper()
	evs, err := testClient().VerifyWebhook(context.Background(),
		headers("push", sign(body)), body)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	return evs
}

// A push to the default branch rebuilds its preview.
//
// Without this the permanent `main` preview was a claim about what `main` looked like the last
// time somebody opened a pull request or restarted the daemon — which is the difference between
// "always current" and "current as of whenever somebody last did something else".
func TestAPushToTheDefaultBranchRebuildsIt(t *testing.T) {
	evs := pushEvents(t, pushBody("refs/heads/main", "abc123", "main", false))

	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	e := evs[0]
	if e.Kind != scm.EventBuild {
		t.Errorf("kind = %q, want build", e.Kind)
	}
	// Zero is what makes this the branch preview rather than a pull request's: no comment is
	// posted, the TTL reaper skips it, and no closed pull request can tear it down.
	if !e.PR.IsBranch() {
		t.Errorf("the event is for pull request #%d, want the branch preview", e.PR.Number)
	}
	if e.PR.Branch != "main" || e.PR.HeadSHA != "abc123" {
		t.Errorf("built %s at %s, want main at abc123", e.PR.Branch, e.PR.HeadSHA)
	}
	if e.PR.Repo.Owner != "acme" || e.PR.Repo.Name != "docs" {
		t.Errorf("repo = %s", e.PR.Repo.Slug())
	}
	// Without it, every build of this preview fails with "the webhook payload was missing
	// installation.id" — which is what the branch previews did before installationOf existed.
	if e.PR.InstallationID != 42 {
		t.Errorf("installation = %d, want 42", e.PR.InstallationID)
	}
}

// Everything else a push can be is ignored, and each for its own reason.
func TestPushesThatMustNotBuild(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		why  string
	}{
		{
			name: "a feature branch",
			body: pushBody("refs/heads/feature/x", "abc123", "main", false),
			// It already arrives as `synchronize` if a pull request is open, so building
			// here too would build it twice; and with no pull request open it is somebody's
			// work in progress, which would publish a URL for every branch anybody pushes.
			why: "a branch that is not the default has its own event or no preview at all",
		},
		{
			name: "a tag",
			body: pushBody("refs/tags/v1.0.0", "abc123", "main", false),
			why:  "a tag is not a branch",
		},
		{
			name: "the default branch deleted",
			body: pushBody("refs/heads/main", "0000000000000000000000000000000000000000",
				"main", true),
			// The all-zero sha arrives with it, and cloning that fails in a way that reads
			// as a broken repository rather than as a deleted branch.
			why: "there is nothing to build",
		},
		{
			name: "a payload naming no default branch",
			body: pushBody("refs/heads/main", "abc123", "", false),
			why:  "guessing which branch is permanent is worse than doing nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if evs := pushEvents(t, tc.body); len(evs) != 0 {
				t.Errorf("built %d thing(s) for %s: %s", len(evs), tc.name, tc.why)
			}
		})
	}
}

// A push delivery with a bad signature is refused before the body is parsed, like every other
// event. Asserted for this one too because it is a new entry point into the same switch.
func TestAPushWithABadSignatureIsRefused(t *testing.T) {
	body := pushBody("refs/heads/main", "abc123", "main", false)
	_, err := testClient().VerifyWebhook(context.Background(),
		headers("push", "sha256=deadbeef"), body)
	if err == nil {
		t.Fatal("a push with a forged signature was accepted")
	}
}
