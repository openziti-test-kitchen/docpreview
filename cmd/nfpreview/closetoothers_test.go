package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCloseToOthersTakesAwayOnlyTheWorldBits: a data directory or database that already exists
// keeps whatever mode it was made with, so startup has to close it. The group's access is an
// operator's decision and stays.
func TestCloseToOthersTakesAwayOnlyTheWorldBits(t *testing.T) {
	if err := closeToOthers(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("a path that does not exist is an error: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("permission bits do not describe access on Windows")
	}

	for _, tc := range []struct{ before, after os.FileMode }{
		{0o644, 0o640},
		{0o666, 0o660},
		{0o640, 0o640},
		{0o600, 0o600},
	} {
		path := filepath.Join(t.TempDir(), "nfpreview.db")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, tc.before); err != nil {
			t.Fatal(err)
		}
		if err := closeToOthers(path); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != tc.after {
			t.Errorf("mode %o became %o, want %o", tc.before, got, tc.after)
		}
	}

	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := closeToOthers(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o750 {
		t.Errorf("a 0755 directory became %o, want 750", fi.Mode().Perm())
	}
}
