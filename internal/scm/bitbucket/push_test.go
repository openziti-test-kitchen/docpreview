package bitbucket

import (
	"encoding/json"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// pushDelivery builds a `repo:push` body.
//
// changes is a list of (branchName, type, hash); a nil hash spells a deletion, which is how
// Bitbucket spells one — there is no `deleted` flag, the `new` object is simply null.
func pushDelivery(mainBranch string, changes ...[3]string) []byte {
	var list []any
	for _, ch := range changes {
		name, kind, hash := ch[0], ch[1], ch[2]
		if hash == "" {
			list = append(list, map[string]any{"new": nil})
			continue
		}
		list = append(list, map[string]any{
			"new": map[string]any{
				"name":   name,
				"type":   kind,
				"target": map[string]string{"hash": hash},
			},
		})
	}

	repo := map[string]any{
		"slug":      "customer-connect-docs",
		"full_name": "netfoundry/customer-connect-docs",
		"workspace": map[string]string{"slug": "netfoundry"},
	}
	if mainBranch != "" {
		repo["mainbranch"] = map[string]string{"name": mainBranch}
	}

	out, _ := json.Marshal(map[string]any{
		"push":       map[string]any{"changes": list},
		"repository": repo,
	})
	return out
}

// A push to the main branch rebuilds its preview.
//
// The Bitbucket half of the same rule GitHub's push handler implements: the permanent preview is
// a claim about what `main` looks like now, and without this it was a claim about what it looked
// like whenever somebody last opened a pull request.
func TestABitbucketPushToMainRebuildsIt(t *testing.T) {
	c, _ := newTestClient(t, commitResolver(""))

	body := pushDelivery("main", [3]string{"main", "branch", "8e769840aa11"})
	events, err := c.VerifyWebhook(t.Context(), headers("repo:push", sign(body)), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	e := events[0]
	if e.Kind != scm.EventBuild {
		t.Errorf("kind = %q, want build", e.Kind)
	}
	if !e.PR.IsBranch() {
		t.Errorf("the event is for pull request #%d, want the branch preview", e.PR.Number)
	}
	if e.PR.Branch != "main" || e.PR.HeadSHA != "8e769840aa11" {
		t.Errorf("built %s at %s", e.PR.Branch, e.PR.HeadSHA)
	}
	if e.PR.Repo.Platform != model.PlatformBitbucket {
		t.Errorf("platform = %q", e.PR.Repo.Platform)
	}
	// Built from owner and name rather than read from links.clone, which carries a username
	// whose identity depends on who asked — inserting credentials into that produces two `@`
	// in the authority and a git failure whose message contains the token.
	if e.PR.Repo.CloneURL != "https://bitbucket.org/netfoundry/customer-connect-docs.git" {
		t.Errorf("clone URL = %q", e.PR.Repo.CloneURL)
	}
}

// One delivery, several branches. Bitbucket sends a list, and only the default branch in it
// matters — a push that moves three feature branches and main must build once.
func TestABitbucketPushPicksTheMainBranchOutOfTheList(t *testing.T) {
	c, _ := newTestClient(t, commitResolver(""))

	body := pushDelivery("main",
		[3]string{"feature/a", "branch", "aaa111"},
		[3]string{"main", "branch", "bbb222"},
		[3]string{"feature/b", "branch", "ccc333"})
	events, err := c.VerifyWebhook(t.Context(), headers("repo:push", sign(body)), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly 1", len(events))
	}
	if events[0].PR.HeadSHA != "bbb222" {
		t.Errorf("built %s, want the main branch's commit", events[0].PR.HeadSHA)
	}
}

// Everything else a push can be is ignored.
func TestBitbucketPushesThatMustNotBuild(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"only feature branches", pushDelivery("main",
			[3]string{"feature/x", "branch", "aaa111"})},
		// A tag push carries type "tag" with a name that could be anything, including one
		// that matches the default branch.
		{"a tag named like the branch", pushDelivery("main",
			[3]string{"main", "tag", "aaa111"})},
		// A deletion is a null `new`, not a flag.
		{"the branch deleted", pushDelivery("main", [3]string{"main", "branch", ""})},
		{"no main branch named", pushDelivery("",
			[3]string{"main", "branch", "aaa111"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, commitResolver(""))
			events, err := c.VerifyWebhook(t.Context(), headers("repo:push", sign(tc.body)), tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Errorf("built %d thing(s)", len(events))
			}
		})
	}
}
