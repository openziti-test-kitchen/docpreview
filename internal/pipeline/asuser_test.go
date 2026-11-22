package pipeline

import (
	"strings"
	"testing"
)

// Where the ids mean nothing, or the daemon is root, the build command is passed through as it
// was: there is no user to drop to.
func TestAsUserLeavesTheCommandAloneWithoutAUser(t *testing.T) {
	for _, ids := range [][2]int{{-1, -1}, {0, 0}} {
		if got := asUser("npm ci && npm run build", "/workspace", ids[0], ids[1]); got != "npm ci && npm run build" {
			t.Errorf("uid %d: got %q, want the command unchanged", ids[0], got)
		}
	}
}

// The build command reaches the inner shell as one quoted argument, so a command with quotes
// or shell syntax in it runs as written rather than being split by the outer shell.
func TestAsUserQuotesTheBuildCommand(t *testing.T) {
	cmd := `npm run build -- --title "it's here"; echo done`
	script := asUser(cmd, "/workspace/www", 1000, 1000)
	if !strings.Contains(script, shellQuote(cmd)) {
		t.Errorf("the build command is not passed as one quoted argument:\n%s", script)
	}
	if !strings.Contains(script, `'/workspace/www/node_modules'`) {
		t.Errorf("the node_modules volume is not handed to the uid:\n%s", script)
	}
}
