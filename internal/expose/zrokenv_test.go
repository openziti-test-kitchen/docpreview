package expose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openziti/zrok/v2/environment"
)

// The registration token must be extractable from the link, because the link is what the
// operator has.
//
// zrok emails `<registrationUrl>/<token>` and nothing tells the reader which part matters. A form
// that only accepted the bare token would be a form that fails for everybody who pastes what they
// were sent, with a "not found" from the service as the only clue.
func TestTheRegistrationTokenIsTakenFromWhateverWasPasted(t *testing.T) {
	const token = "aBcD1234wXyZ"
	for _, in := range []string{
		token,
		"  " + token + "  ",
		"https://zrok.io/register/" + token,
		"https://zrok.io/register/" + token + "/",
		"https://zrok.io/register/" + token + "?utm_source=email",
		"https://zrok.io/register/" + token + "#top",
	} {
		if got := ZrokRegisterToken(in); got != token {
			t.Errorf("ZrokRegisterToken(%q) = %q, want %q", in, got, token)
		}
	}

	if got := ZrokRegisterToken("   "); got != "" {
		t.Errorf("blank input gave %q, want an empty token so the caller can refuse it", got)
	}
}

// A second UseZrokRoot naming a different environment must fail rather than switch.
//
// zrok's root directory is a process-wide global read by every LoadRoot. Rebinding it after the
// exposer has loaded its root leaves the process holding one environment while writing to
// another — and the two are different zrok accounts, so what looks like a configuration change is
// silently a change of which account's shares get reaped.
func TestTheZrokRootCannotBeSwitchedOnceChosen(t *testing.T) {
	restoreZrokRootForTest(t)

	dir := t.TempDir()
	if err := UseZrokRoot(ZrokProject, dir); err != nil {
		t.Fatalf("first UseZrokRoot: %v", err)
	}
	// Idempotent, so wiring can be defensive without knowing whether something already ran.
	if err := UseZrokRoot(ZrokProject, dir); err != nil {
		t.Errorf("repeating the same choice failed: %v", err)
	}
	if err := UseZrokRoot(ZrokSystem, dir); err == nil {
		t.Error("switching to the machine's environment was allowed after the project's was in use")
	}
	if got := ZrokScopeInForce(); got != ZrokProject {
		t.Errorf("the scope in force is %q, want it unchanged by the refused switch", got)
	}
}

// An unknown scope is refused, so a typo in a stored setting cannot silently mean "the machine's".
func TestAnUnknownZrokScopeIsRefused(t *testing.T) {
	restoreZrokRootForTest(t)

	if err := UseZrokRoot("Project", t.TempDir()); err == nil {
		t.Error(`UseZrokRoot("Project") was accepted; the scopes are lower case`)
	}
	if err := UseZrokRoot(ZrokProject, ""); err == nil {
		t.Error("UseZrokRoot with no directory was accepted")
	}
}

// Inspection describes a directory that is not there without creating it or failing.
//
// This is the state of every fresh installation, and it is what the signup panel switches on. An
// error here would render the panel as broken on exactly the installations that need it.
func TestInspectingRootsThatDoNotExist(t *testing.T) {
	restoreZrokRootForTest(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	projectDir := filepath.Join(t.TempDir(), "zrok2")
	st, err := InspectZrokRoots(projectDir, "")
	if err != nil {
		t.Fatalf("InspectZrokRoots: %v", err)
	}

	if st.Project.Exists || st.Project.Enabled {
		t.Errorf("a directory that does not exist reported exists=%v enabled=%v",
			st.Project.Exists, st.Project.Enabled)
	}
	if st.Project.Path != projectDir {
		t.Errorf("reported path %q, want %q", st.Project.Path, projectDir)
	}
	if st.MustChoose {
		t.Error("MustChoose is set with nothing enrolled anywhere")
	}
	if st.Enabled() {
		t.Error("Enabled() is true with no scope in force and nothing enrolled")
	}
	// Inspection must not bring the directory into existence: a `~/.zrok2` created merely by
	// looking would then be reported as a half-finished setup by the next look.
	if _, err := os.Stat(projectDir); !os.IsNotExist(err) {
		t.Errorf("inspecting created %s", projectDir)
	}
}

// Inspection must leave the root that is in force in force.
//
// It has to point the global at a directory that is not the one being used, twice. Getting the
// restore wrong would leave the process publishing from whichever root it happened to look at
// last — which for a daemon means the next publish going to the other account.
func TestInspectingDoesNotChangeWhichRootIsInForce(t *testing.T) {
	restoreZrokRootForTest(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	chosen := filepath.Join(t.TempDir(), "chosen")
	if err := os.MkdirAll(chosen, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := UseZrokRoot(ZrokProject, chosen); err != nil {
		t.Fatal(err)
	}

	before, err := environment.LoadRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectZrokRoots(filepath.Join(t.TempDir(), "other"), ZrokProject); err != nil {
		t.Fatal(err)
	}
	after, err := environment.LoadRoot()
	if err != nil {
		t.Fatal(err)
	}

	if before.Metadata().RootPath != after.Metadata().RootPath {
		t.Errorf("inspecting moved the root from %q to %q",
			before.Metadata().RootPath, after.Metadata().RootPath)
	}
	if after.Metadata().RootPath != chosen {
		t.Errorf("the root in force is %q, want %q", after.Metadata().RootPath, chosen)
	}
}

// ZrokEnable and ZrokDisable refuse the states they cannot act in, rather than reaching the
// network to find out.
//
// The enable case is the one that matters: every enable spends an environment against the
// account's quota and leaves the previous one orphaned, still counted, and still holding whatever
// shares it had.
func TestEnrolmentRefusesTheImpossibleCases(t *testing.T) {
	restoreZrokRootForTest(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	dir := filepath.Join(t.TempDir(), "zrok2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := UseZrokRoot(ZrokProject, dir); err != nil {
		t.Fatal(err)
	}

	// No token, so nothing is attempted. Asserted because the alternative is a request to the
	// controller with an empty credential, whose answer is a 401 the operator has to interpret.
	if err := ZrokEnable(t.Context(), zrokNoSecret(), "docpreview"); err == nil {
		t.Error("ZrokEnable with no account token was attempted")
	}

	// Nothing enrolled, so there is nothing to disable — and no call to make.
	if err := ZrokDisable(t.Context()); err == nil {
		t.Error("ZrokDisable on an environment that is not enabled was attempted")
	}
}

// An environment enrolled against the zrok v1 service must be refused, not published through.
//
// The versions are not interchangeable: v1 has no namespaces and no reserved names, and a reserved
// name in a namespace is the entire mechanism a preview's stable URL depends on. Enrolled against
// v1 everything reads as configured — there is an account, a directory, an endpoint — and the first
// publish fails with a 404 from a path that does not exist, which looks like a deleted environment
// and sends the operator to re-enrol against the same wrong service.
//
// Only zrok.io hosts are judged. A self-hosted v2 controller can be at any address, and guessing
// would refuse a working setup.
func TestTheV1ServiceIsRefusedAndSelfHostedIsNot(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		refused  bool
	}{
		{"https://api-v2.zrok.io", false},
		{"https://api-v2.zrok.io/", false},
		{"https://api.zrok.io", true},
		{"https://api.zrok.io/api/v1", true},
		{"https://zrok.io", true},
		// Self-hosted, so nothing is claimed either way.
		{"https://zrok.internal.example", false},
		{"https://previews.acme.com/api/v2", false},
		{"", false},
	} {
		got := zrokUnsupported(latestRootForTest{}, tc.endpoint)
		if (got != "") != tc.refused {
			t.Errorf("zrokUnsupported(%q) = %q, refused=%v want %v",
				tc.endpoint, got, got != "", tc.refused)
		}
		if tc.refused && !strings.Contains(got, "v2") {
			t.Errorf("the refusal for %q does not say v2 is needed: %q", tc.endpoint, got)
		}
	}
}

// An environment directory written by an older zrok is refused on its own, whatever endpoint it
// names — the on-disk format is the other way to be on the wrong version.
func TestAnOlderEnvironmentDirectoryIsRefused(t *testing.T) {
	why := zrokUnsupported(oldRootForTest{}, "https://api-v2.zrok.io")
	if why == "" {
		t.Fatal("an environment directory from an older zrok was accepted")
	}
	// The version is in the message, because "cannot use this" without saying which version
	// found is a message that cannot be acted on.
	if !strings.Contains(why, "v0.1") {
		t.Errorf("the refusal does not name the version found: %q", why)
	}
}

// A malformed API endpoint fails locally, with the value in the message.
func TestAMalformedZrokEndpointIsRefusedBeforeAnyRequest(t *testing.T) {
	for _, ep := range []string{"api-v2.zrok.io", "not a url", "/api/v2"} {
		if _, err := zrokAccountClient(ep); err == nil {
			t.Errorf("zrokAccountClient(%q) was accepted", ep)
		}
	}
	if _, err := zrokAccountClient(""); err != nil {
		t.Errorf("an empty endpoint should fall back to the hosted service, got %v", err)
	}
	if _, err := zrokAccountClient(DefaultZrokAPIEndpoint); err != nil {
		t.Errorf("the default endpoint was refused: %v", err)
	}
}
