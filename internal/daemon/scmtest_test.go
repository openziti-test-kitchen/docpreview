package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

func scmTestFixture(t *testing.T, fn func(context.Context, model.Repo) (scm.RepoCheck, error)) http.Handler {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}
	return NewProjectsAdmin(st, cfg, slog.New(slog.DiscardHandler)).WithSCMChecker(fn).Handler()
}

// TestTheCredentialTestAnswersWithItsSteps.
//
// The page renders one row per step, so a refused step has to come back as a result with a
// failed row in it — 200, the steps, and ok:false — and not as the 400-with-a-string it used
// to be, which could only say that something somewhere had failed.
func TestTheCredentialTestAnswersWithItsSteps(t *testing.T) {
	var asked model.Repo
	h := scmTestFixture(t, func(_ context.Context, repo model.Repo) (scm.RepoCheck, error) {
		asked = repo
		return scm.RepoCheck{
			Credential: scm.CheckedCredential{
				Source: "this project's own access token",
				Header: "Authorization: Bearer ••••••9f2c",
			},
			Checks: []scm.CheckStep{
				{Name: "Repository read", Request: "GET x", Outcome: scm.CheckPass, Message: "ok"},
				{Name: "Pull request read", Request: "GET y", Outcome: scm.CheckFail, Message: "403"},
				{Name: "Pull request write", Outcome: scm.CheckNotAttempted, Message: "not attempted"},
			},
		}, nil
	})

	rec := secretCall(t, h, "POST", "/api/projects/bitbucket/acme/docs/scm-test", "", localCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a check that ran: %s", rec.Code, rec.Body)
	}
	if asked != (model.Repo{Platform: model.PlatformBitbucket, Owner: "acme", Name: "docs"}) {
		t.Errorf("checked %+v", asked)
	}

	var body struct {
		OK         bool                  `json:"ok"`
		Credential scm.CheckedCredential `json:"credential"`
		Checks     []scm.CheckStep       `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	if body.OK {
		t.Error("ok = true with a failed step")
	}
	if len(body.Checks) != 3 || body.Checks[1].Outcome != scm.CheckFail {
		t.Errorf("checks = %+v", body.Checks)
	}
	if body.Credential.Header != "Authorization: Bearer ••••••9f2c" ||
		body.Credential.Source != "this project's own access token" {
		t.Errorf("credential = %+v", body.Credential)
	}
}

// TestACredentialTestThatCannotStartIsAnError. No credential anywhere means no request was
// made, so there are no steps to show — the page says so in its first row.
func TestACredentialTestThatCannotStartIsAnError(t *testing.T) {
	h := scmTestFixture(t, func(context.Context, model.Repo) (scm.RepoCheck, error) {
		return scm.RepoCheck{}, errors.New("no Bitbucket credential for acme/docs")
	})
	rec := secretCall(t, h, "POST", "/api/projects/bitbucket/acme/docs/scm-test", "", localCaller)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "no Bitbucket credential for acme/docs" {
		t.Errorf("body = %s", rec.Body)
	}
}
