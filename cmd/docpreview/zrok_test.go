package main

import (
	"testing"

	"github.com/netfoundry/docpreview/internal/expose"
)

// Which zrok environment a daemon adopts when nothing is stored, and — the part that matters —
// when it writes that answer down.
//
// The reason recording is not automatic: two enabled environments are two different zrok accounts,
// and startup deletes every share it recognises as its own. Recording a *guess* would make it the
// operator's decision, the panel would stop asking, and one of the two accounts would keep losing
// its shares to a daemon nobody knowingly pointed at it.
//
// The reason recording is not skipped either: with one environment enabled and nothing stored, a
// daemon that merely used it would move to the *other* account the moment that one was enrolled —
// a change of account with no change of configuration.
func TestChoosingAZrokEnvironmentWhenNothingIsStored(t *testing.T) {
	enabled := expose.ZrokRootInfo{Exists: true, Enabled: true}
	empty := expose.ZrokRootInfo{}

	for _, tc := range []struct {
		name      string
		state     expose.ZrokEnvState
		want      expose.ZrokScope
		record    bool
		ambiguous bool
	}{
		{
			name:  "nothing enrolled anywhere",
			state: expose.ZrokEnvState{System: empty, Project: empty},
			// The project root, so enrolling from the dashboard writes beside the vault.
			want: expose.ZrokProject, record: false,
		},
		{
			name:  "only the machine's",
			state: expose.ZrokEnvState{System: enabled, Project: empty},
			want:  expose.ZrokSystem, record: true,
		},
		{
			name:  "only this installation's",
			state: expose.ZrokEnvState{System: empty, Project: enabled},
			want:  expose.ZrokProject, record: true,
		},
		{
			name:  "both, which is a guess and must stay one",
			state: expose.ZrokEnvState{System: enabled, Project: enabled},
			want:  expose.ZrokProject, record: false, ambiguous: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope, record, ambiguous := chooseZrokScope(tc.state)
			if scope != tc.want {
				t.Errorf("scope = %q, want %q", scope, tc.want)
			}
			if record != tc.record {
				t.Errorf("record = %v, want %v", record, tc.record)
			}
			if ambiguous != tc.ambiguous {
				t.Errorf("ambiguous = %v, want %v", ambiguous, tc.ambiguous)
			}
		})
	}
}
