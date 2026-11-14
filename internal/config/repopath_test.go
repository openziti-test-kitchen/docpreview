package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The three repository-relative paths are an allowlist, not a denylist:
// a comma in build.dir became a bind mount of the host.
func TestCheckRepoPath(t *testing.T) {
	for _, ok := range []string{
		".", "build", "docs/site", ".vitepress/dist", "_build/html", "pkg/@scope/site+v2",
		"docusaurus/nfpreview-detect", "a-b/c_d.e", "./docs",
	} {
		if err := CheckRepoPath(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "a,type=bind,source=/,target=/h", "a=b", `a"b`, "a b", "a;b", "a$(id)", "a`id`",
		"a'b", "a\nb", "build\n", `a\b`, "-v", "--mount", "/etc", "../x", "a/../../x", "..", "C:/x",
		"a*", "a?", "a[b]", "a|b", "a&b", "a<b", "a>b", "a{b}", "a:b", "a%b", "a#b", "a!b",
		"é", strings.Repeat("a", 513),
	} {
		if err := CheckRepoPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// .nfpreview.yml comes from the pull request, so each of the three keys is refused on load.
func TestLoadRepoConfigRefusesPathsOutsideTheAllowlist(t *testing.T) {
	for _, yml := range []string{
		"build:\n  dir: \"a,type=bind,source=/,target=/h\"\n",
		"build:\n  output: \"out,readonly\"\n",
		"detect:\n  script: \"x;reboot\"\n",
		"build:\n  dir: \"-v\"\n",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, RepoConfigName), []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRepoConfig(dir); err == nil {
			t.Errorf("accepted:\n%s", yml)
		}
	}
}

// Every preset has to pass the check its values are held to at build time, or choosing one
// would fail every build.
func TestEveryFrameworkPresetPassesThePathCheck(t *testing.T) {
	for _, f := range frameworks {
		for name, p := range map[string]string{"dir": f.Dir, "output": f.Output} {
			if p == "" {
				continue
			}
			if err := CheckRepoPath(p); err != nil {
				t.Errorf("preset %s %s: %v", f.ID, name, err)
			}
		}
	}
}
