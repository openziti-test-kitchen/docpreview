package pipeline

import (
	"encoding/csv"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
)

// The directory names a pull request used to turn the node_modules volume into a bind
// mount of the host's root.
var hostileDirs = []string{
	"a,type=bind,source=/,target=/h",
	"a,readonly",
	`a"b`,
	"a b",
	"a;reboot",
	"a$(id)",
	"a`id`",
	"a'b",
	"a=b",
	"-v",
	"a\nb",
	`a\b`,
	"../x",
	"/etc",
}

// mountValues returns the value of every --mount flag in a docker command line.
func mountValues(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--mount" {
			out = append(out, args[i+1])
		}
	}
	return out
}

// mountFields parses a --mount value the way docker does: one CSV record of key=value fields.
func mountFields(t *testing.T, spec string) []string {
	t.Helper()
	fields, err := csv.NewReader(strings.NewReader(spec)).Read()
	if err != nil {
		t.Fatalf("docker would not parse the mount %q: %v", spec, err)
	}
	return fields
}

// A hostile build directory never reaches docker: createArgs refuses it outright.
func TestCreateArgsRefusesABuildDirThatIsNotAPlainPath(t *testing.T) {
	b := &Builder{
		defaults: config.BuildDefaults{Driver: "docker", Image: "node:24-bookworm-slim"},
		log:      slog.New(slog.DiscardHandler),
	}
	for _, dir := range hostileDirs {
		cfg := config.RepoConfig{}
		cfg.Build.Dir = dir
		cfg.Build.Output = "build"
		cfg.Build.Command = "npm run build"

		args, err := b.createArgs(
			model.PullRequest{Repo: model.Repo{Platform: model.PlatformGitHub,
				Owner: "acme", Name: "docs"}, Number: 1},
			cfg, "/mnt/d/ws", "/workspace/"+dir, t.TempDir(), "node:24-bookworm-slim", "", nil)
		if err == nil {
			t.Errorf("build dir %q was accepted:\n%s", dir, strings.Join(args, " "))
		}
	}
}

// Build refuses the same values before anything runs, whichever driver is configured, and
// for the output directory as well as the build directory.
func TestBuildRefusesHostilePathsBeforeRunningAnything(t *testing.T) {
	ws := &Workspace{Dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(ws.Dir, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := NewBuilder(config.BuildDefaults{Driver: "docker"}, slog.New(slog.DiscardHandler))

	for _, p := range hostileDirs {
		for _, field := range []string{"dir", "output"} {
			cfg := config.DefaultRepoConfig()
			// An echo, so a build that did start would be harmless and would fail on the
			// missing output rather than on the path.
			cfg.Build.Command = "echo ran"
			if field == "dir" {
				cfg.Build.Dir = p
			} else {
				cfg.Build.Output = p
			}
			_, err := b.Build(t.Context(), ws, cfg, nil)
			if err == nil || !strings.Contains(err.Error(), "build."+field) {
				t.Errorf("build.%s = %q: got %v, want a refusal naming build.%s", field, p, err, field)
			}
		}
	}
}

// Every mount is a record docker reads back as exactly the fields that were meant, even when
// a value holds docker's own separators. This is the property that makes the allowlist a
// second line of defence rather than the only one.
func TestMountSpecCannotBeGivenExtraFields(t *testing.T) {
	for _, value := range []string{
		"/workspace/a,type=bind,source=/,target=/h/node_modules",
		`/workspace/a"b,readonly`,
		"/workspace/plain/node_modules",
		`C:\Users\x,y\ws`,
	} {
		spec, err := mountSpec("type=volume", "target="+value)
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		fields := mountFields(t, spec)
		if len(fields) != 2 || fields[0] != "type=volume" || fields[1] != "target="+value {
			t.Errorf("%q was read back as %q", value, fields)
		}
	}

	// A value with nothing to quote is written exactly as before, so nothing changes for
	// the mounts production already makes.
	if spec, _ := mountSpec("type=bind", "source=/var/lib/ws", "target=/workspace"); spec !=
		"type=bind,source=/var/lib/ws,target=/workspace" {
		t.Errorf("a plain mount was rewritten: %q", spec)
	}

	for _, value := range []string{"a\nb", "a\rb", "a\x00b"} {
		if spec, err := mountSpec("type=volume", "target="+value); err == nil {
			t.Errorf("a control character was accepted: %q", spec)
		}
	}
}

// The mounts a real build asks for carry only the fields they are meant to: no mount has a
// second type, a second target, or a source it was not given.
func TestCreateArgsMountsHaveOnlyTheIntendedFields(t *testing.T) {
	for _, dir := range []string{"", "www", "docs/site", "pkg/@scope/site+v2"} {
		for _, spec := range mountValues(createArgsFor(t, dir)) {
			seen := map[string]int{}
			for _, f := range mountFields(t, spec) {
				key, _, _ := strings.Cut(f, "=")
				seen[key]++
			}
			for key, n := range seen {
				if n > 1 {
					t.Errorf("build dir %q: mount %q has %d %q fields", dir, spec, n, key)
				}
				switch key {
				case "type", "source", "target":
				default:
					t.Errorf("build dir %q: mount %q has an unexpected field %q", dir, spec, key)
				}
			}
		}
	}
}

// The chown runs under `sh -lc`, so the directory is quoted rather than interpolated.
func TestTheChownQuotesTheBuildDirectory(t *testing.T) {
	cmd := reownCommand("/workspace/a b;touch /pwned", 1000, 1000)
	if !strings.Contains(cmd, `-path '/workspace/a b;touch /pwned/node_modules' -prune`) {
		t.Errorf("the directory is not quoted:\n%s", cmd)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}
