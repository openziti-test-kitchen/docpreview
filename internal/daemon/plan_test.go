package daemon

import (
	"log/slog"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// planFixture is an admin with no store and no exposer: planFor touches neither, which is
// the property that lets the form ask for a plan of a row that has never been saved.
func planFixture(t *testing.T) *ProjectsAdmin {
	t.Helper()
	cfg := config.DefaultServer()
	return NewProjectsAdmin(nil, cfg, slog.New(slog.DiscardHandler))
}

// line finds one step of the plan by its key.
func line(t *testing.T, v planView, key string) planLine {
	t.Helper()
	for _, l := range v.Lines {
		if l.Key == key {
			return l
		}
	}
	t.Fatalf("no %q line in the plan: %+v", key, v.Lines)
	return planLine{}
}

// TestThePlanIsWhatTheBuildWouldDo. The point of the panel is that it is not a second
// opinion: both it and a build go through resolveProject, so a preset the form shows is
// the preset the build applies.
func TestThePlanIsWhatTheBuildWouldDo(t *testing.T) {
	a := planFixture(t)
	p := store.Project{
		Platform: "bitbucket", Owner: "netfoundry", Repo: "docs",
		Framework: "docusaurus",
	}

	built := resolveProject(config.DefaultRepoConfig(), p, func(string, ...any) {})
	plan := a.planFor(p)

	for _, c := range []struct{ key, want string }{
		{"cd", built.Build.Dir},
		{"build", built.Build.Command},
		{"serve", built.Build.Output},
	} {
		if got := line(t, plan, c.key).Value; got != c.want {
			t.Errorf("%s: the plan says %q, a build would use %q", c.key, got, c.want)
		}
	}
}

// TestTheProjectsOwnFieldOutranksThePreset, and the badge says so. A value that merely
// matches the preset's must still be attributed to the operator who typed it, or the
// badge is answering a different question than the one it appears to.
func TestTheProjectsOwnFieldOutranksThePreset(t *testing.T) {
	a := planFixture(t)
	preset, ok := config.FrameworkByID("docusaurus")
	if !ok {
		t.Fatal("no docusaurus preset to test against")
	}

	typed := a.planFor(store.Project{
		Platform: "github", Owner: "acme", Repo: "docs",
		Framework: "docusaurus", BuildCommand: "yarn build",
	})
	if got := line(t, typed, "build"); got.Value != "yarn build" || got.From != "project" {
		t.Errorf("a typed command: got %q from %q, want \"yarn build\" from \"project\"",
			got.Value, got.From)
	}

	// The same value the preset would have supplied, typed by hand.
	same := a.planFor(store.Project{
		Platform: "github", Owner: "acme", Repo: "docs",
		Framework: "docusaurus", BuildCommand: preset.BuildCommand,
	})
	if got := line(t, same, "build").From; got != "project" {
		t.Errorf("a command typed to match the preset is attributed to %q, want \"project\"", got)
	}
}

// TestABlankFieldSaysTheRepositoryDecides. The one thing the form could not say before:
// an empty box is not "nothing happens", it is "a file on a machine you are not looking
// at decides", and the value shown is only what it falls back to.
func TestABlankFieldSaysTheRepositoryDecides(t *testing.T) {
	a := planFixture(t)
	plan := a.planFor(store.Project{Platform: "github", Owner: "acme", Repo: "docs"})

	for _, key := range []string{"cd", "build", "serve"} {
		got := line(t, plan, key)
		if got.From != "repo" {
			t.Errorf("%s with no project field and no preset: from %q, want \"repo\"", key, got.From)
		}
		if got.Note == "" {
			t.Errorf("%s: no note saying the repository decides", key)
		}
	}
}

// TestTheServerDefaultsAreNamedAsTheServers. Driver, image and timeout are the three the
// operator cannot see from the form at all when they are blank.
func TestTheServerDefaultsAreNamedAsTheServers(t *testing.T) {
	a := planFixture(t)
	plan := a.planFor(store.Project{Platform: "github", Owner: "acme", Repo: "docs"})

	for _, c := range []struct{ key, want string }{
		{"driver", a.cfg.Build.Driver},
		{"image", a.cfg.Build.Image},
		{"timeout", a.cfg.Build.Timeout.String()},
	} {
		got := line(t, plan, c.key)
		if got.Value != c.want {
			t.Errorf("%s: the plan says %q, the server config says %q", c.key, got.Value, c.want)
		}
		if got.From != "server" {
			t.Errorf("%s: from %q, want \"server\"", c.key, got.From)
		}
	}
}

// TestADriverThatWouldBeRefusedIsSaidSoOnTheForm. The local driver fails at the last
// moment before a build, which is an hour after the form was submitted.
func TestADriverThatWouldBeRefusedIsSaidSoOnTheForm(t *testing.T) {
	a := planFixture(t)
	p := store.Project{Platform: "github", Owner: "acme", Repo: "docs", Driver: config.DriverLocal}

	if got := a.planFor(p).Warn; got == "" {
		t.Error("the local driver is not enabled, and the plan said nothing about it")
	}

	a.cfg.Build.AllowLocalDriver = true
	if got := a.planFor(p).Warn; got != "" {
		t.Errorf("the local driver is enabled, so there is nothing to warn about: %q", got)
	}
}

// TestAnUnusableTimeoutFallsBackRatherThanBreakingThePlan. The form refuses one on save,
// so the only way to reach this is mid-typing — and a panel that blanked out on every
// half-typed duration would be a panel nobody could use while typing a duration.
func TestAnUnusableTimeoutFallsBackRatherThanBreakingThePlan(t *testing.T) {
	a := planFixture(t)
	plan := a.planFor(store.Project{
		Platform: "github", Owner: "acme", Repo: "docs", Timeout: "4",
	})

	got := line(t, plan, "timeout")
	if got.Value != a.cfg.Build.Timeout.String() || got.From != "server" {
		t.Errorf("a half-typed timeout: got %q from %q, want the server default from \"server\"",
			got.Value, got.From)
	}
}

// TestThePlanNamesThePreviewThroughTheRealTemplate. A name composed in the page would be
// a second copy of the template and the prefix, and the one it would get wrong is the
// installation that set either.
func TestThePlanNamesThePreviewThroughTheRealTemplate(t *testing.T) {
	a := planFixture(t)
	var asked model.PullRequest
	a.WithPlanNamer(func(pr model.PullRequest) (string, error) {
		asked = pr
		return "a-docs-main", nil
	})

	plan := a.planFor(store.Project{Platform: "github", Owner: "acme", Repo: "docs"})
	if plan.Name != "a-docs-main" {
		t.Errorf("name %q, want the renderer's answer", plan.Name)
	}
	if asked.Repo.Name != "docs" || asked.Branch != planBranch {
		t.Errorf("rendered for %+v, want the form's repository on %q", asked.Repo, planBranch)
	}
}

// TestNoNamerLeavesTheLineOut rather than inventing one. Every test that builds this
// admin directly has no exposer behind it, and so does a daemon mid-startup.
func TestNoNamerLeavesTheLineOut(t *testing.T) {
	a := planFixture(t)
	if got := a.planFor(store.Project{Platform: "github", Owner: "acme", Repo: "docs"}); got.Name != "" {
		t.Errorf("name %q with no renderer wired, want none", got.Name)
	}
}
