package bitbucket

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// A token long enough to keep its last four characters on the panel, with a distinctive
// middle so a leak anywhere in the result is a substring match away.
const longToken = "ATCTT3xFfGN0-the-middle-of-the-token-9f2c"

// checkServer answers the two requests CheckRepo makes, each with its own status.
func checkServer(t *testing.T, repoStatus, prStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := repoStatus
		if strings.HasSuffix(r.URL.Path, "/pullrequests") {
			status = prStatus
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "error",
				"error": map[string]string{
					"message": "Your credentials lack one or more required privilege scopes.",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"full_name": "acme/docs", "is_private": true, "size": 3,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func projectTokenClient(t *testing.T, base string) *Client {
	t.Helper()
	return clientWithGlobal(t, base, "").WithProjectCredentials(
		func(owner, repo string) ProjectCredential { return ProjectCredential{AccessToken: longToken} })
}

var acmeDocs = model.Repo{Platform: model.PlatformBitbucket, Owner: "acme", Name: "docs"}

// TestTheCredentialCheckReportsEachStep.
//
// The Test panel renders one row per step, so the shape is the contract: three steps in a
// fixed order, each naming its request, and the write step settled as not attempted — never
// as a pass, because nothing checked it.
func TestTheCredentialCheckReportsEachStep(t *testing.T) {
	srv := checkServer(t, http.StatusOK, http.StatusOK)
	got, err := projectTokenClient(t, srv.URL).CheckRepo(t.Context(), acmeDocs)
	if err != nil {
		t.Fatal(err)
	}

	want := []struct{ name, request, outcome string }{
		{"Repository read", "GET " + srv.URL + "/2.0/repositories/acme/docs", scm.CheckPass},
		{"Pull request read", "GET " + srv.URL +
			"/2.0/repositories/acme/docs/pullrequests?state=OPEN&pagelen=1", scm.CheckPass},
		{"Pull request write", "", scm.CheckNotAttempted},
	}
	if len(got.Checks) != len(want) {
		t.Fatalf("%d steps, want %d: %+v", len(got.Checks), len(want), got.Checks)
	}
	for i, w := range want {
		c := got.Checks[i]
		if c.Name != w.name || c.Request != w.request || c.Outcome != w.outcome {
			t.Errorf("step %d = %q %q %q, want %q %q %q",
				i, c.Name, c.Request, c.Outcome, w.name, w.request, w.outcome)
		}
		if c.Message == "" {
			t.Errorf("step %q has no message", c.Name)
		}
	}
	if !got.OK() {
		t.Error("a check whose only unproven step was not attempted reported not OK")
	}
	if got.Credential.Source != "this project's own access token" {
		t.Errorf("source = %q", got.Credential.Source)
	}
}

// TestTheCheckedCredentialIsMasked.
//
// The panel shows which token went out, and must not become the one place in this system
// that reads a secret back. Asserted on the marshalled result, because the wire is what the
// browser sees: a field added later that carried the token would fail here.
func TestTheCheckedCredentialIsMasked(t *testing.T) {
	srv := checkServer(t, http.StatusOK, http.StatusOK)
	got, err := projectTokenClient(t, srv.URL).CheckRepo(t.Context(), acmeDocs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Credential.Header != "Authorization: Bearer ••••••9f2c" {
		t.Errorf("header = %q, want the scheme and the last four characters only",
			got.Credential.Header)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{longToken, "the-middle-of-the-token", "ATCTT3x"} {
		if strings.Contains(string(wire), leak) {
			t.Errorf("the result carries %q: %s", leak, wire)
		}
	}
}

// TestAShortTokenShowsNoCharactersAtAll. Four characters of a sixteen-character secret is a
// quarter of it, so below the floor nothing is kept.
func TestAShortTokenShowsNoCharactersAtAll(t *testing.T) {
	for token, want := range map[string]string{
		"abcd":                 "••••••",
		"fifteen-chars!!":      "••••••",
		"sixteen-chars-ok":     "••••••s-ok",
		"x" + longToken + "yz": "••••••2cyz",
	} {
		if got := maskTail(token); got != want {
			t.Errorf("maskTail(%q) = %q, want %q", token, got, want)
		}
	}
	if got := basic("someone@example.com", longToken).masked(); got != "Basic base64(email:••••••9f2c)" {
		t.Errorf("basic masked = %q", got)
	}
	if strings.Contains(basic("someone@example.com", longToken).masked(), "someone") {
		t.Error("the basic form names the account")
	}
}

// TestAPullRequestReadRefusalIsItsOwnFailedStep — the case the old single string handled
// worst: the repository read worked, so the clone will, and only the second permission is
// missing. The platform's own message has to survive, with the scope it needed after it.
func TestAPullRequestReadRefusalIsItsOwnFailedStep(t *testing.T) {
	srv := checkServer(t, http.StatusOK, http.StatusForbidden)
	got, err := projectTokenClient(t, srv.URL).CheckRepo(t.Context(), acmeDocs)
	if err != nil {
		t.Fatalf("a refused step was returned as an error rather than a result: %v", err)
	}
	if got.OK() {
		t.Error("a refused pull request read reported OK")
	}
	if got.Checks[0].Outcome != scm.CheckPass {
		t.Errorf("repository read = %q, want pass", got.Checks[0].Outcome)
	}
	pr := got.Checks[1]
	if pr.Outcome != scm.CheckFail {
		t.Fatalf("pull request read = %q, want fail", pr.Outcome)
	}
	for _, want := range []string{"403", "lack one or more required privilege scopes", "Pull requests: Read"} {
		if !strings.Contains(pr.Message, want) {
			t.Errorf("the failure does not mention %q: %s", want, pr.Message)
		}
	}
	if got.Checks[2].Outcome != scm.CheckNotAttempted {
		t.Errorf("write = %q, want not attempted", got.Checks[2].Outcome)
	}
}

// TestARepositoryReadFailureSkipsThePullRequestRead. Same token, same repository: the second
// request would fail for the same reason, and two red rows read as two problems.
func TestARepositoryReadFailureSkipsThePullRequestRead(t *testing.T) {
	srv := checkServer(t, http.StatusNotFound, http.StatusOK)
	got, err := projectTokenClient(t, srv.URL).CheckRepo(t.Context(), acmeDocs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Checks[0].Outcome != scm.CheckFail ||
		!strings.Contains(got.Checks[0].Message, "cannot see it") {
		t.Errorf("repository read = %+v, want a failure naming a missing or invisible repository",
			got.Checks[0])
	}
	if got.Checks[1].Outcome != scm.CheckNotAttempted {
		t.Errorf("pull request read = %q, want not attempted after the repository read failed",
			got.Checks[1].Outcome)
	}
	if got.OK() {
		t.Error("a failed repository read reported OK")
	}
}
