package vault

import (
	"path/filepath"
	"testing"
)

// TestProjectSCMCredentialsNeverReachABuild.
//
// A project's own environment variables and its own source-control credential share one
// vault prefix, and only one of them may be handed to a build. RevealPrefix is the single
// bulk read of values in this package and its only caller assembles a build's environment,
// so anything it returns goes into a process running the pull request's own build script.
//
// The separator is the key's shape: shell-shaped names are variables a build asks for by
// name, dotted names are the daemon's own. Without the filter, the token that can clone
// and comment on a repository would be injected into every build of that repository —
// where a contributor who can push a branch could print it.
func TestProjectSCMCredentialsNeverReachABuild(t *testing.T) {
	dir := t.TempDir()
	v, err := OpenWithKey(filepath.Join(dir, "vault.age"), "a-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}

	const platform, owner, repo = "bitbucket", "netfoundry", "customer-connect-docs"
	stored := map[string]string{
		// A build variable: injected, and the reason this namespace exists.
		ProjectSecretKey(platform, owner, repo, "BB_REPO_TOKEN_ONPREM"): "a-build-token",
		// The project's source-control credential: must not be injected.
		ProjectSCMKey(platform, owner, repo, SCMAccessToken): "a-repository-access-token",
		ProjectSCMKey(platform, owner, repo, SCMEmail):       "someone@example.com",
		ProjectSCMKey(platform, owner, repo, SCMAPIToken):    "an-api-token",
	}
	for k, val := range stored {
		if err := v.Set(k, NewSecretString(val)); err != nil {
			t.Fatal(err)
		}
	}

	env := v.RevealPrefix(ProjectSecretPrefix(platform, owner, repo))

	if env["BB_REPO_TOKEN_ONPREM"] != "a-build-token" {
		t.Errorf("the build variable did not survive the filter: %v", env)
	}
	for _, name := range []string{SCMAccessToken, SCMEmail, SCMAPIToken} {
		if got, ok := env[name]; ok {
			t.Errorf("%s reached a build's environment as %q", name, got)
		}
	}
	if len(env) != 1 {
		t.Errorf("a build's environment has %d entries, want 1: %v", len(env), env)
	}
}

// TestIsProjectSCMKeyIsAClosedList. "Anything dotted is a credential" would make a typo
// into a credential the daemon looks for and nothing ever sets — which fails as a build
// that cannot clone, with no indication that the name is wrong.
func TestIsProjectSCMKeyIsAClosedList(t *testing.T) {
	for _, name := range []string{SCMAccessToken, SCMEmail, SCMAPIToken} {
		if !IsProjectSCMKey(name) {
			t.Errorf("IsProjectSCMKey(%q) = false", name)
		}
	}
	for _, name := range []string{"scm.acess_token", "scm.token", "bitbucket.access_token",
		"BB_REPO_TOKEN_ONPREM", "", "scm."} {
		if IsProjectSCMKey(name) {
			t.Errorf("IsProjectSCMKey(%q) = true", name)
		}
	}
}

// TestProjectSCMKeysAreNotShellShaped ties the two rules together: the reason the SCM
// names are dotted is that IsBuildEnvKey is what the resolver filters on, so a name that
// passed both would be a credential in every build.
func TestProjectSCMKeysAreNotShellShaped(t *testing.T) {
	for _, name := range []string{SCMAccessToken, SCMEmail, SCMAPIToken} {
		if IsBuildEnvKey(name) {
			t.Errorf("%q is shell-shaped, so it would be injected into every build", name)
		}
	}
}
