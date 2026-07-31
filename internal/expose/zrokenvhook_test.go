package expose

import (
	"testing"

	"github.com/openziti/zrok/v2/environment"
	"github.com/openziti/zrok/v2/environment/env_core"
	"github.com/openziti/zrok/v2/environment/env_v0_4"

	"github.com/netfoundry/docpreview/internal/vault"
)

// restoreZrokRootForTest lets one test choose a zrok root without deciding it for the rest of the
// binary.
//
// UseZrokRoot is deliberately one-way in production: rebinding zrok's process-wide root global
// under a running exposer would leave the daemon holding one environment while writing to another.
// That is exactly what makes it untestable more than once per process, so the reset lives here, in
// a test file, where nothing shipped can reach it.
func restoreZrokRootForTest(t *testing.T) {
	t.Helper()

	zrokRootMu.Lock()
	saved := zrokRootChosen
	zrokRootChosen = struct {
		set   bool
		scope ZrokScope
		path  string
	}{}
	zrokRootMu.Unlock()

	// The library global too, since UseZrokRoot may have moved it.
	environment.SetRootDirName(ZrokRootDirName)

	t.Cleanup(func() {
		zrokRootMu.Lock()
		zrokRootChosen = saved
		zrokRootMu.Unlock()
		if saved.set {
			environment.SetRootDirName(saved.path)
		} else {
			environment.SetRootDirName(ZrokRootDirName)
		}
	})
}

// zrokNoSecret is an empty credential, spelled out so the test reads as "no token" rather than as
// a zero value somebody might mistake for an oversight.
func zrokNoSecret() vault.Secret { return vault.Secret{} }

// latestRootForTest and oldRootForTest are the two answers `environment.IsLatest` can give.
//
// A stub rather than a real directory, because the version check is the thing under test and
// producing an environment in an older on-disk format means writing that format by hand — which
// would be asserting against my own guess at it rather than against the library's opinion.
//
// Only Metadata is implemented; IsLatest reads nothing else, and a nil-panic in some future caller
// is a better outcome than a stub that quietly answers for a method it knows nothing about.
type latestRootForTest struct{ env_core.Root }

func (latestRootForTest) Metadata() *env_core.Metadata {
	return &env_core.Metadata{V: env_v0_4.V, RootPath: "test"}
}

type oldRootForTest struct{ env_core.Root }

func (oldRootForTest) Metadata() *env_core.Metadata {
	return &env_core.Metadata{V: "v0.1", RootPath: "test"}
}
