package pipeline

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
)

func secretsPR() model.PullRequest {
	return model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "netfoundry", Name: "unified-doc"},
		Number:  42,
		Branch:  "add-guide",
		HeadSHA: "4f0c2a1deadbeef",
	}
}

// TestProjectSecretsReachTheBuildEnvironment — the whole point. A documentation build
// that assembles several private repositories dispatches on an environment variable
// per source, so a token that does not arrive in the environment is a clone that
// falls back to SSH and a build that fails on a host with no key.
func TestProjectSecretsReachTheBuildEnvironment(t *testing.T) {
	b := NewBuilderWithSecrets(config.BuildDefaults{},
		map[string]string{"GLOBAL_TOKEN": "global-value-long-enough"},
		slog.New(slog.DiscardHandler)).
		WithSecrets(map[string]string{"BB_REPO_TOKEN_ONPREM": "bb-project-value-long-enough"})

	env := b.buildEnv(secretsPR(), config.RepoConfig{}, nil)

	if !slices.Contains(env, "BB_REPO_TOKEN_ONPREM=bb-project-value-long-enough") {
		t.Error("the project's own secret is not in the build environment")
	}
	// The global one still applies. A project secret adds to what every build gets;
	// it does not replace it, or naming one would silently drop the rest.
	if !slices.Contains(env, "GLOBAL_TOKEN=global-value-long-enough") {
		t.Error("adding a project secret dropped the server-wide ones")
	}
}

// TestProjectSecretsAreRedacted is the one that must never regress.
//
// The redactor is compiled from the *values*, so a builder copied with a larger
// secrets map and the original redactor would inject a credential the scrubber had
// never been told about. npm prints its environment on failure and any script under
// `set -x` prints it always, and the build log goes into a pull request comment — so
// the leak would be public, permanent and attributed to the operator.
func TestProjectSecretsAreRedacted(t *testing.T) {
	const token = "bb-project-value-long-enough"

	b := NewBuilder(config.BuildDefaults{}, slog.New(slog.DiscardHandler)).
		WithSecrets(map[string]string{"BB_REPO_TOKEN_ONPREM": token})

	if !b.Redactor().Active() {
		t.Fatal("the redactor is inactive after adding a project secret")
	}
	log := "cloning with BB_REPO_TOKEN_ONPREM=" + token + " ...\n"
	if got := b.Redactor().Scrub(log); got == log {
		t.Errorf("a project secret survives the redactor verbatim: %q", got)
	}
}

// TestProjectSecretsDoNotLeakBetweenProjects — WithSecrets copies rather than
// mutating, because two projects build concurrently and the daemon holds one base
// builder. Mutation would give whichever project built second the other's tokens,
// which is the failure this scoping exists to prevent in the first place.
func TestProjectSecretsDoNotLeakBetweenProjects(t *testing.T) {
	base := NewBuilder(config.BuildDefaults{}, slog.New(slog.DiscardHandler))

	one := base.WithSecrets(map[string]string{"TOKEN_ONE": "value-one-long-enough"})
	two := base.WithSecrets(map[string]string{"TOKEN_TWO": "value-two-long-enough"})

	oneEnv := one.buildEnv(secretsPR(), config.RepoConfig{}, nil)
	twoEnv := two.buildEnv(secretsPR(), config.RepoConfig{}, nil)

	if slices.Contains(oneEnv, "TOKEN_TWO=value-two-long-enough") {
		t.Error("one project's build can see another project's secret")
	}
	if slices.Contains(twoEnv, "TOKEN_ONE=value-one-long-enough") {
		t.Error("one project's build can see another project's secret")
	}
	// And the builder they were both derived from has neither, so a project with no
	// secrets of its own does not inherit the last project that built.
	baseEnv := base.buildEnv(secretsPR(), config.RepoConfig{}, nil)
	for _, kv := range baseEnv {
		if kv == "TOKEN_ONE=value-one-long-enough" || kv == "TOKEN_TWO=value-two-long-enough" {
			t.Errorf("WithSecrets mutated the base builder: %s", kv)
		}
	}
}

// TestTheLogNamesTheInjectedVariables.
//
// A variable stored under a name the build script does not read fails several steps removed
// from the cause: the script falls back to SSH, the container has no key, and the log says
// "Host key verification failed" — naming neither the missing variable nor the fallback.
//
// So the log says which variables were supplied. Names only: a name is a lookup key the
// operator chose and has to be able to check, and the values are what the redactor is for.
func TestTheLogNamesTheInjectedVariables(t *testing.T) {
	b := NewBuilder(config.BuildDefaults{}, slog.New(slog.DiscardHandler)).
		WithSecrets(map[string]string{
			"GH_ZITI_CI_REPO_ACCESS_PAT_NF": "github_pat_not-a-real-token",
			"BB_REPO_TOKEN_ONPREM":          "ATCTT3-not-a-real-token",
		})

	var out strings.Builder
	b.writeInjectedNames(&out)
	line := out.String()

	for _, want := range []string{"BB_REPO_TOKEN_ONPREM", "GH_ZITI_CI_REPO_ACCESS_PAT_NF"} {
		if !strings.Contains(line, want) {
			t.Errorf("the log does not name %s: %q", want, line)
		}
	}
	// Sorted, so two builds of the same project produce the same line and a diff between
	// logs shows a real change rather than map iteration order.
	if strings.Index(line, "BB_REPO") > strings.Index(line, "GH_ZITI") {
		t.Errorf("the names are not sorted: %q", line)
	}
	for _, secret := range []string{"github_pat_not-a-real-token", "ATCTT3-not-a-real-token"} {
		if strings.Contains(line, secret) {
			t.Fatalf("a value is in the log line: %q", line)
		}
	}

	// And no variables at all says so, because an absent line reads as a build whose
	// variables were fine.
	var none strings.Builder
	NewBuilder(config.BuildDefaults{}, slog.New(slog.DiscardHandler)).writeInjectedNames(&none)
	if !strings.Contains(none.String(), "none") {
		t.Errorf("a build with no variables says %q", none.String())
	}
}

// TestARepositoryCannotShadowAProjectSecret.
//
// buildEnv reserves every name in the secrets map against `.docpreview.yml`, and
// WithSecrets has to land the project's variables in that same map for the protection
// to extend to them. If it did not, a pull request could set BB_REPO_TOKEN_ONPREM to a
// string of its own — watching what the build did differently, and, worse, putting a
// value the redactor has never been told about into the log.
func TestARepositoryCannotShadowAProjectSecret(t *testing.T) {
	b := NewBuilder(config.BuildDefaults{}, slog.New(slog.DiscardHandler)).
		WithSecrets(map[string]string{"BB_REPO_TOKEN_ONPREM": "the-real-token-value"})

	// The repository's own config, which arrives in the pull request.
	cfg := config.RepoConfig{Build: config.RepoBuild{Env: map[string]string{
		"BB_REPO_TOKEN_ONPREM": "attacker-chosen",
	}}}

	env := b.buildEnv(secretsPR(), cfg, nil)
	if slices.Contains(env, "BB_REPO_TOKEN_ONPREM=attacker-chosen") {
		t.Error("a pull request set a project's credential to a value of its own")
	}
	if !slices.Contains(env, "BB_REPO_TOKEN_ONPREM=the-real-token-value") {
		t.Error("the project's own value is not what the build gets")
	}
}

// TestWithSecretsOfNothingKeepsTheBuilder — a project with no secrets must not pay a
// redactor recompile, and must not lose the redactor it already had.
func TestWithSecretsOfNothingKeepsTheBuilder(t *testing.T) {
	b := NewBuilderWithSecrets(config.BuildDefaults{},
		map[string]string{"GLOBAL_TOKEN": "global-value-long-enough"},
		slog.New(slog.DiscardHandler))

	if got := b.WithSecrets(nil); got != b {
		t.Error("WithSecrets(nil) copied the builder")
	}
	if !b.WithSecrets(map[string]string{}).Redactor().Active() {
		t.Error("an empty project secret set disarmed the redactor")
	}
}
