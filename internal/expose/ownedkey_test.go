package expose

import (
	"strings"
	"testing"
)

// TestOwnedKeyRecognisesBothNames pins which zrok share Targets count as this program's.
//
// The docpreview: spelling is what v0.4.9 and earlier wrote. A daemon that did not
// recognise it would neither adopt nor reap those shares, and each still holds the reserved
// name its preview republishes under. Anything else, including a Target that only contains
// the prefix, belongs to somebody else and must never be reaped.
func TestOwnedKeyRecognisesBothNames(t *testing.T) {
	cases := []struct {
		target string
		key    string
		ours   bool
	}{
		{"nfpreview:abc123", "abc123", true},
		{"docpreview:abc123", "abc123", true},
		{"nfpreview:abc123/20261105-145219-bbd9551", "abc123/20261105-145219-bbd9551", true},
		{"docpreview:abc123/20261105-145219-bbd9551", "abc123/20261105-145219-bbd9551", true},
		{"", "", false},
		{"-", "", false},
		{"atrium-hub", "", false},
		{"mypreview:abc123", "", false},
		{"x-nfpreview:abc123", "", false},
		{"http://127.0.0.1:8080/docpreview:abc123", "", false},
	}
	for _, c := range cases {
		key, ours := ownedKey(c.target)
		if ours != c.ours || key != c.key {
			t.Errorf("ownedKey(%q) = %q, %v; want %q, %v", c.target, key, ours, c.key, c.ours)
		}
	}
}

// TestListFilterMatchesBothNames: ListShares filters Target by substring on the controller,
// so the one filter has to be a substring of both spellings or one of them is never listed.
func TestListFilterMatchesBothNames(t *testing.T) {
	for _, p := range []string{targetPrefix, legacyTargetPrefix} {
		if !strings.Contains(p, listTargetFilter) {
			t.Errorf("listTargetFilter %q is not a substring of %q", listTargetFilter, p)
		}
	}
}
