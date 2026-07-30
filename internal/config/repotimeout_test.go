package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestARepositoryCannotSetItsOwnBuildTimeout.
//
// RepoConfig is read from the pull request's own working tree, so every field in it
// is written by whoever opened the pull request. How long a branch may occupy a build
// worker is not the branch's decision — a repository that could set this could hold a
// worker for as long as the parser accepts, on a daemon with two of them.
//
// The mechanism is `yaml:"-"` on RepoBuild.Timeout, which is invisible until somebody
// "tidies up" the tag. Hence a test: the field must stay in the struct, because the
// operator's project row writes it in applyProject, and must stay undecodable here.
func TestARepositoryCannotSetItsOwnBuildTimeout(t *testing.T) {
	var rc RepoConfig
	body := "build:\n  dir: www\n  timeout: 6h\n"
	if err := yaml.Unmarshal([]byte(body), &rc); err != nil {
		t.Fatalf("decoding a repo config: %v", err)
	}

	// The neighbouring field proves the document was really parsed, so a zero Timeout
	// cannot be explained away by the yaml never having been read.
	if rc.Build.Dir != "www" {
		t.Fatalf("build.dir = %q, want www — the document did not decode", rc.Build.Dir)
	}
	if rc.Build.Timeout != 0 {
		t.Errorf("a repository set its own build timeout to %s; only the operator's "+
			"project row may set this", rc.Build.Timeout)
	}
}
