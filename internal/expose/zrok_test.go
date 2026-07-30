package expose

import (
	"errors"
	"fmt"
	"testing"

	"github.com/openziti/zrok/v2/rest_client_zrok/share"
)

// TestQuotaConflictIsNotAnExistingName — POST /share/name answers 409 for four
// different situations and only one of them means "the name is already there".
//
// Matching the generated type alone, which this used to do, reported success to an
// account that had hit its reserved-name limit. The CreateShare a few lines later then
// failed with an error that never mentioned quotas, so the operator was told a name
// could not be bound and had nothing pointing at the reason. One name per commit
// reaches that limit far sooner than one per branch, which is what made this worth a
// test.
//
// The payload strings are the controller's own, from createShareName.go.
func TestQuotaConflictIsNotAnExistingName(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		exists  bool
		because string
	}{
		{
			name:    "the name already exists",
			err:     &share.CreateShareNameConflict{},
			exists:  true,
			because: "an empty payload is the only 409 that means the name is registered",
		},
		{
			name:   "the account is at its name limit",
			err:    &share.CreateShareNameConflict{Payload: "names limit reached; cannot reserve additional names"},
			exists: false,
			because: "reporting a registered name here makes the next call fail for a " +
				"reason that does not mention the quota",
		},
		{
			name:    "the name failed the profanity or DNS check",
			err:     &share.CreateShareNameConflict{Payload: "'xx' is not a valid share name; failed profanity or DNS check"},
			exists:  false,
			because: "the name does not exist and never will under this template",
		},
		{
			name:    "a stale frontend mapping blocks the name",
			err:     &share.CreateShareNameConflict{Payload: "name 'x' has a stale frontend mapping"},
			exists:  false,
			because: "the controller could not heal the mapping, so nothing was registered",
		},
		{
			name:    "some other error entirely",
			err:     errors.New("connection refused"),
			exists:  false,
			because: "only a 409 can mean the name is present",
		},
		{
			name:    "wrapped, because Publish wraps what it returns",
			err:     fmt.Errorf("registering zrok name: %w", &share.CreateShareNameConflict{}),
			exists:  true,
			because: "errors.As unwraps, and the caller adds context to every error it returns",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isNameAlreadyExists(c.err); got != c.exists {
				t.Errorf("isNameAlreadyExists = %v, want %v: %s", got, c.exists, c.because)
			}
		})
	}
}

// TestZrokReleasesNamesAndNothingElseDoes documents the shape rather than the
// behaviour, because releasing a name is one round trip to a live zrok controller and
// there is no seam to fake it behind.
//
// What it does catch is a fourth exposer growing a ReleaseName by accident — the
// daemon type-asserts on NameReleaser at teardown, so any exposer that satisfies it
// starts being asked to release names it does not have.
func TestZrokReleasesNamesAndNothingElseDoes(t *testing.T) {
	if _, ok := any((*Zrok)(nil)).(NameReleaser); !ok {
		t.Error("*Zrok does not implement NameReleaser, so every teardown leaks a name")
	}
	for _, ex := range []any{(*Local)(nil), (*Frontdoor)(nil), (*Ziti)(nil)} {
		if _, ok := ex.(NameReleaser); ok {
			t.Errorf("%T implements NameReleaser but has no name object to release", ex)
		}
	}
}
