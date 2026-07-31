package github

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
)

// A build that did not come from a webhook must still be able to authenticate.
//
// `PullRequest.InstallationID` is filled in from a webhook payload, and every path that
// starts with an operator instead has a zero there: a project scan, a linked pull request,
// and a branch preview. Branch previews shipped with that unhandled, and every GitHub one
// failed at the clone with "the webhook payload was missing installation.id" — a message
// about a webhook, for a build that never had one, which sends whoever reads it to look at
// delivery logs that do not exist.
//
// Both live GitHub projects failed this way on the first run. The fix is `installationOf`,
// which looks the installation up when the pull request does not carry one, and these tests
// are what stop it being removed as redundant.
func TestCloneURLWorksWithoutAWebhookInstallationID(t *testing.T) {
	f := newAPIFixture(t)
	var lookups int
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		// The App-authenticated "am I installed here" call.
		if r.URL.Path == "/repos/acme/docs/installation" {
			lookups++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 999})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}

	// No InstallationID: this is what BuildBranch, ScanRepo and LinkPR construct.
	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Branch: "main", HeadSHA: "85912e2",
	}

	url, err := f.client.CloneURL(context.Background(), pr)
	if err != nil {
		t.Fatalf("a build with no webhook behind it could not authenticate: %v", err)
	}
	if lookups != 1 {
		t.Errorf("the installation was looked up %d times, want once", lookups)
	}
	// The token is in the URL, which is why the value is a credential — asserted on shape
	// rather than printed.
	if !strings.Contains(url, "x-access-token:") || !strings.Contains(url, "/acme/docs.git") {
		t.Errorf("clone URL has the wrong shape (token redacted from this message)")
	}
}

// A webhook-delivered pull request must not pay for a lookup it does not need.
//
// The id is in the payload, so resolving it again would be one extra App-authenticated
// round trip on the hot path — every push, every repository.
func TestAWebhookDeliveryDoesNotLookUpTheInstallation(t *testing.T) {
	f := newAPIFixture(t)
	var lookups int
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/installation") {
			lookups++
		}
		w.WriteHeader(http.StatusNotFound)
	}

	pr := model.PullRequest{
		Repo:           model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:         42,
		InstallationID: 999,
	}

	if _, err := f.client.CloneURL(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	if lookups != 0 {
		t.Errorf("a delivery that carried its installation id still looked one up %d times", lookups)
	}
}

// And the failure when the App genuinely is not installed still says so.
//
// This is the case the original error message was written for, and it must survive the fix:
// a repository nobody installed the App on is the commonest reason a project added by hand
// never builds.
func TestAnUninstalledRepositoryStillSaysSo(t *testing.T) {
	f := newAPIFixture(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		// What GitHub answers for a repository the App is not installed on.
		w.WriteHeader(http.StatusNotFound)
	}

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Branch: "main",
	}

	_, err := f.client.CloneURL(context.Background(), pr)
	if err == nil {
		t.Fatal("a repository with no installation produced a clone URL")
	}
	// The message has to name the repository and the fix rather than the mechanism. It used
	// to talk about a webhook payload, which is the wrong subject for an operator who never
	// sent one.
	if !strings.Contains(err.Error(), "acme/docs") {
		t.Errorf("the error does not name the repository: %v", err)
	}
	if strings.Contains(err.Error(), "webhook payload") {
		t.Errorf("the error still blames a webhook for a build that had none: %v", err)
	}
}
