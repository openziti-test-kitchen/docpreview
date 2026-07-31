package store

import (
	"context"
	"path/filepath"
	"testing"
)

func settingsStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// "Never set" and "set to empty" are different answers, and the read path depends on it.
//
// The config file holds the default; this table holds an operator's override. An operator who
// deliberately cleared the name prefix has chosen "no prefix", and re-inheriting the file's
// value there would silently undo their decision on the next restart — which is exactly the
// kind of bug that only appears after a restart nobody connects to the change.
func TestSettingDistinguishesUnsetFromEmpty(t *testing.T) {
	st := settingsStore(t)
	ctx := context.Background()

	v, ok, err := st.Setting(ctx, SettingNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("a fresh database reports the setting as set, holding %q", v)
	}

	if err := st.SetSetting(ctx, SettingNamePrefix, ""); err != nil {
		t.Fatal(err)
	}
	v, ok, err = st.Setting(ctx, SettingNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("a setting written as empty reads back as never set, so the config file wins " +
			"and the operator's choice is lost")
	}
	if v != "" {
		t.Errorf("value = %q, want empty", v)
	}
}

func TestSetSettingOverwritesAndClearRemoves(t *testing.T) {
	st := settingsStore(t)
	ctx := context.Background()

	for _, want := range []string{"a", "aws", "b2"} {
		if err := st.SetSetting(ctx, SettingNamePrefix, want); err != nil {
			t.Fatal(err)
		}
		got, ok, err := st.Setting(ctx, SettingNamePrefix)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || got != want {
			t.Errorf("after writing %q, read back %q (set=%v)", want, got, ok)
		}
	}

	// Clearing is not the same as writing empty: it returns the setting to "never set", so
	// the config file's value applies again.
	if err := st.ClearSetting(ctx, SettingNamePrefix); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Setting(ctx, SettingNamePrefix); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("a cleared setting still reads as set")
	}
}

// Settings must not collide with each other, which is the whole reason the key is a constant
// rather than a string written at each call site: a typo is not a compile error, it is a
// value that reads back as the default and looks like a save that did not happen.
func TestSettingsAreIndependent(t *testing.T) {
	st := settingsStore(t)
	ctx := context.Background()

	if err := st.SetSetting(ctx, SettingNamePrefix, "a"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "something.else", "b"); err != nil {
		t.Fatal(err)
	}

	got, _, err := st.Setting(ctx, SettingNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if got != "a" {
		t.Errorf("the prefix reads %q after another setting was written", got)
	}
}
