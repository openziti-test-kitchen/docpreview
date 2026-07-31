package expose

import (
	"testing"

	"github.com/openziti/zrok/v2/environment"

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
