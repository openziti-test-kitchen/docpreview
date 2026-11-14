package model

import (
	"strings"
	"testing"
)

func TestIsCommitSHA(t *testing.T) {
	for _, ok := range []string{
		"cf9f37d25cf7515f8c7e531afbe97cc6ee4238f3",
		strings.Repeat("0", 40),
		strings.Repeat("a", 64),
	} {
		if !IsCommitSHA(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{
		"", "abc123", "../../../../", "--upload-pack=touch /tmp/x",
		"CF9F37D25CF7515F8C7E531AFBE97CC6EE4238F3",
		"cf9f37d25cf7515f8c7e531afbe97cc6ee4238f",
		"cf9f37d25cf7515f8c7e531afbe97cc6ee4238f3\n",
		"../" + strings.Repeat("a", 37),
		strings.Repeat("a", 41), strings.Repeat("a", 63), strings.Repeat("g", 40),
	} {
		if IsCommitSHA(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestIsSafeRef(t *testing.T) {
	for _, ok := range []string{"main", "feature/new-guide", "release-1.2", "a@b", "<svg/onload=x>"} {
		if !IsSafeRef(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{
		"", "-x", "--upload-pack=touch /tmp/x", "--output=/etc/passwd", "a\nb", "a\x00b",
		strings.Repeat("a", 256),
	} {
		if IsSafeRef(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
