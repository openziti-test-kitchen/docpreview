package daemon

import (
	"net/http"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// repoConfigWith is a repository's own .docpreview.yml, as the detector would have read it.
func repoConfigWith(command, output string) config.RepoConfig {
	cfg := config.DefaultRepoConfig()
	cfg.Build.Command = command
	cfg.Build.Output = output
	return cfg
}

func bitbucketPR() model.PullRequest {
	return model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7,
	}
}

// TestFrameworkPresetPrecedence is the whole contract of a preset, in one table.
//
// Precedence runs top down: the project's explicit field, then its preset, then the repository's own
// .docpreview.yml. A preset outranks the repository's file because an operator who chose "Docusaurus" in
// the dashboard has said something more recent and more specific. The blank preset defers to the
// repository, and it is the default.
func TestFrameworkPresetPrecedence(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	pr := bitbucketPR()

	cases := []struct {
		name        string
		project     store.Project
		wantCommand string
		wantOutput  string
	}{{
		name:        "no preset defers to the repository",
		project:     store.Project{Platform: "github", Owner: "acme", Repo: "docs"},
		wantCommand: "make docs",
		wantOutput:  "site",
	}, {
		name: "a preset beats the repository",
		project: store.Project{Platform: "github", Owner: "acme", Repo: "docs",
			Framework: "docusaurus"},
		wantCommand: "npm run build",
		wantOutput:  "build",
	}, {
		name: "an explicit field beats the preset",
		project: store.Project{Platform: "github", Owner: "acme", Repo: "docs",
			Framework: "docusaurus", BuildCommand: "./build-docs.sh -l"},
		wantCommand: "./build-docs.sh -l",
		// Still the preset's, because only the command was overridden.
		wantOutput: "build",
	}, {
		name: "both fields explicit",
		project: store.Project{Platform: "github", Owner: "acme", Repo: "docs",
			Framework: "docusaurus", BuildCommand: "yarn docs", BuildOutput: "public"},
		wantCommand: "yarn docs",
		wantOutput:  "public",
	}, {
		// A downgrade, or a table entry since removed. Falling back to the repository is
		// better than refusing to build, and refusing is what a validation error here
		// would amount to.
		name: "an unknown preset is ignored",
		project: store.Project{Platform: "github", Owner: "acme", Repo: "docs",
			Framework: "a-generator-nobody-has-heard-of"},
		wantCommand: "make docs",
		wantOutput:  "site",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.SaveProject(t.Context(), tc.project); err != nil {
				t.Fatal(err)
			}
			got := d.applyProject(t.Context(), pr, repoConfigWith("make docs", "site"))
			if got.Build.Command != tc.wantCommand {
				t.Errorf("command = %q, want %q", got.Build.Command, tc.wantCommand)
			}
			if got.Build.Output != tc.wantOutput {
				t.Errorf("output = %q, want %q", got.Build.Output, tc.wantOutput)
			}
		})
	}
}

// TestEveryPresetHasBothValues. A preset that supplies only one of the two is worse than
// none: the other silently comes from the repository, so the form shows one greyed default
// and the build uses a mixture nobody chose.
func TestEveryPresetHasBothValues(t *testing.T) {
	for _, f := range config.Frameworks() {
		if f.ID == config.FrameworkNone {
			if f.BuildCommand != "" || f.Output != "" {
				t.Errorf("the blank preset carries values: %+v", f)
			}
			continue
		}
		if f.Label == "" {
			t.Errorf("%s has no label", f.ID)
		}
		if f.BuildCommand == "" || f.Output == "" {
			t.Errorf("%s supplies command=%q output=%q; a preset needs both",
				f.ID, f.BuildCommand, f.Output)
		}
	}
}

// TestFrameworkIDsAreUnique — two entries with one id makes FrameworkByID's answer depend
// on table order, and the dropdown would show the same value twice.
func TestFrameworkIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range config.Frameworks() {
		if seen[f.ID] {
			t.Errorf("duplicate framework id %q", f.ID)
		}
		seen[f.ID] = true
	}
}

// TestSavingAnUnknownFrameworkIsRefused. Stored as typed, it would fall back to the
// repository at build time — a project that looks configured and is not. The form can only
// send an id from the table, so this is about the API being reachable without a browser.
func TestSavingAnUnknownFrameworkIsRefused(t *testing.T) {
	h, st := projectFormFixture(t)

	rec := secretCall(t, h, "PUT", "/api/projects/github/acme/docs",
		`{"framework":"not-a-framework"}`, localCaller)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown preset was accepted: %d %s", rec.Code, rec.Body)
	}
	if _, err := st.ProjectFor(t.Context(), "github", "acme", "docs"); err == nil {
		t.Error("the project was saved despite the refused preset")
	}

	// And a known one round-trips.
	rec = secretCall(t, h, "PUT", "/api/projects/github/acme/docs",
		`{"framework":"docusaurus"}`, localCaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("a known preset was refused: %d %s", rec.Code, rec.Body)
	}
	p, err := st.ProjectFor(t.Context(), "github", "acme", "docs")
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "docusaurus" {
		t.Errorf("framework = %q", p.Framework)
	}
}
