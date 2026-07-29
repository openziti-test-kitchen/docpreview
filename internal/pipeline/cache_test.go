package pipeline

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/config"
)

// TestCacheMountsPointEachManagerAtItsOwnDirectory is the assertion that keeps
// builds fast. Without these mounts every build re-downloads its whole dependency
// tree, because the workspace they would otherwise cache into is created per commit
// and pruned with its siblings.
func TestCacheMountsPointEachManagerAtItsOwnDirectory(t *testing.T) {
	root := t.TempDir()
	b := &Builder{
		defaults: config.BuildDefaults{CacheDir: root},
		log:      slog.New(slog.DiscardHandler),
	}

	args, err := b.cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"npm_config_cache=/cache/npm",
		"YARN_CACHE_FOLDER=/cache/yarn",
		"npm_config_store_dir=/cache/pnpm",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no environment pointing a manager at its cache: want %s in\n%s", want, joined)
		}
	}

	// Created on the host first. A bind mount of a missing path creates it
	// root-owned, which on a Linux host leaves a cache the operator cannot clear.
	for _, m := range []string{"npm", "yarn", "pnpm"} {
		if _, err := os.Stat(filepath.Join(root, m)); err != nil {
			t.Errorf("the %s cache directory was not created on the host: %v", m, err)
		}
	}

	// Windows spellings must not survive into a mount argument — see hostMountPath.
	for _, a := range args {
		if strings.HasPrefix(a, "type=bind") && strings.ContainsAny(strings.SplitN(a, ",target=", 2)[0], `\`) {
			t.Errorf("a mount source is still in Windows form: %s", a)
		}
	}
}

// TestCacheMountsAreAbsentWithoutACacheDir — the docker driver has to work with no
// cache configured, since that is what every existing config says.
func TestCacheMountsAreAbsentWithoutACacheDir(t *testing.T) {
	b := &Builder{log: slog.New(slog.DiscardHandler)}
	args, err := b.cacheMounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none when no cache dir is set", args)
	}
}
