package vault

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The master key is the one secret with nowhere inside the system to live, so
// every way of reading it is a tradeoff rather than a solution. These tests pin
// the tradeoffs: that a key file anyone else can read is refused rather than
// used, that a command cannot become a shell, and that "no source at all" is a
// state and not an error.

func TestParseKeySourceForms(t *testing.T) {
	cases := []struct {
		in   string
		kind string
	}{
		{"", ""},
		{"   ", ""},
		{"file:/etc/docpreview/master.key", SourceKindFile},
		{"FILE:/etc/docpreview/master.key", SourceKindFile},
		{"/etc/docpreview/master.key", SourceKindFile},
		{"exec:op read op://ops/docpreview", SourceKindExec},
		// A Windows drive letter is not a scheme. Getting this wrong turns every
		// absolute Windows path into an unrecognised source.
		{`D:\keys\docpreview.key`, SourceKindFile},
		{`file:D:\keys\docpreview.key`, SourceKindFile},
		// An unrecognised prefix is far more likely a path than an invented
		// scheme, so it is treated as one.
		{"relative/path.key", SourceKindFile},
	}
	for _, c := range cases {
		src, err := ParseKeySource(c.in)
		if err != nil {
			t.Errorf("ParseKeySource(%q): %v", c.in, err)
			continue
		}
		if src.Kind() != c.kind {
			t.Errorf("ParseKeySource(%q).Kind() = %q, want %q", c.in, src.Kind(), c.kind)
		}
	}
}

func TestParseKeySourceRejectsNonsense(t *testing.T) {
	for _, in := range []string{"file:", "exec:", `exec:op read "unterminated`} {
		if _, err := ParseKeySource(in); err == nil {
			t.Errorf("ParseKeySource(%q) was accepted", in)
		}
	}
}

func TestParseKeySourceFilePathIsAbsolute(t *testing.T) {
	// The path is resolved once, at parse, so that the containment check in
	// config and the read here agree — and so that neither depends on the
	// working directory the daemon happened to be started from.
	src, err := ParseKeySource("relative/master.key")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(src.Path()) {
		t.Errorf("Path() = %q, want an absolute path", src.Path())
	}
}

func TestSplitArgvHonoursQuotesAndNotShellSyntax(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"op read op://ops/key", []string{"op", "read", "op://ops/key"}},
		{`"C:\Program Files\op\op.exe" read x`, []string{`C:\Program Files\op\op.exe`, "read", "x"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		// No shell means these are arguments, not operators. A source that
		// silently piped or redirected would be a config file with a shell in
		// it, which is the thing being avoided.
		{"printf x | tee /tmp/leak", []string{"printf", "x", "|", "tee", "/tmp/leak"}},
	}
	for _, c := range cases {
		got, err := splitArgv(c.in)
		if err != nil {
			t.Errorf("splitArgv(%q): %v", c.in, err)
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("splitArgv(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func writeKey(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies the umask, so ask for the mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFileSourceReadsAndTrims(t *testing.T) {
	// Trailing newline included on purpose: every editor and every `keygen -out`
	// writes one, and a key with a newline glued to it derives a different
	// scrypt identity and reports "wrong master key".
	path := writeKey(t, "a-passphrase\n", 0o600)

	src, err := ParseKeySource("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got != "a-passphrase" {
		t.Errorf("Read() = %q, want the trimmed contents", got)
	}
}

func TestFileSourceRefusesAKeyOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows permissions are ACLs, which a mode bit does not describe:
		// os.Stat reports 0666 for an ordinary file no matter who can open it,
		// so the check is skipped there rather than rejecting every key file.
		t.Skip("mode bits do not describe Windows permissions")
	}
	path := writeKey(t, "a-passphrase\n", 0o644)

	src, err := ParseKeySource("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Read()
	if err == nil {
		t.Fatal("a world-readable master key was accepted")
	}
	// The operator's next move has to be in the message; "permission denied"
	// with no path and no fix is the error that gets worked around by chmod 777.
	for _, want := range []string{path, "chmod 600"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
}

func TestFileSourceMissingAndEmpty(t *testing.T) {
	missing, err := ParseKeySource(filepath.Join(t.TempDir(), "nope.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Read(); !errors.Is(err, ErrLocked) {
		t.Errorf("a missing key file gave %v, want ErrLocked — the daemon serves "+
			"a locked vault rather than refusing to start", err)
	}

	// An empty file must not become an empty passphrase. age accepts one, and
	// the result is a vault anybody can open.
	empty, err := ParseKeySource(writeKey(t, "\n  \n", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Read(); !errors.Is(err, ErrLocked) {
		t.Errorf("an empty key file gave %v, want ErrLocked", err)
	}
}

func TestNoSourceIsLockedNotBroken(t *testing.T) {
	// The default. It has to be distinguishable from a misconfiguration, because
	// the daemon's response is to serve the setup page rather than exit.
	var none KeySource
	if !none.IsZero() {
		t.Error("the zero KeySource is not reporting itself as absent")
	}
	if _, err := none.Read(); !errors.Is(err, ErrLocked) {
		t.Errorf("Read() with no source gave %v, want ErrLocked", err)
	}
	if none.Describe() != "none" {
		t.Errorf("Describe() = %q, want %q", none.Describe(), "none")
	}
}

func TestExecSourceReadsStdout(t *testing.T) {
	// `go` is the one command guaranteed present wherever these tests run, and
	// `go env GOROOT` prints one line to stdout — enough to prove the plumbing
	// without shipping a fixture binary.
	src, err := ParseKeySource("exec:go env GOROOT")
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.Read()
	if err != nil {
		t.Skipf("go is not runnable here: %v", err)
	}
	if got == "" || strings.ContainsAny(got, "\r\n") {
		t.Errorf("Read() = %q, want a single trimmed line", got)
	}
}

func TestExecSourceFailureIsReported(t *testing.T) {
	src, err := ParseKeySource("exec:docpreview-no-such-credential-helper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Read(); err == nil {
		t.Fatal("a missing credential helper was accepted")
	}
}

func TestOpenFromUsesTheKeySource(t *testing.T) {
	// The whole point: a daemon with a key source opens its vault with nobody
	// present and no environment variable set.
	t.Setenv(MasterKeyEnv, "")

	keyPath := writeKey(t, "file-supplied-passphrase\n", 0o600)
	src, err := ParseKeySource("file:" + keyPath)
	if err != nil {
		t.Fatal(err)
	}

	vaultPath := filepath.Join(t.TempDir(), "vault.age")
	v, err := OpenFrom(vaultPath, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("k", NewSecretString("a-stored-value")); err != nil {
		t.Fatal(err)
	}
	if err := v.Save(); err != nil {
		t.Fatal(err)
	}

	// Reopening proves the key derived from the file is the one the vault was
	// written with, which a single Open cannot.
	again, err := OpenFrom(vaultPath, src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if got.RevealString() != "a-stored-value" {
		t.Errorf("secret = %q after reopening with the file key", got.RevealString())
	}
}

func TestKeySourceBeatsTheEnvironment(t *testing.T) {
	// Order matters and is not arbitrary: the environment is the fallback
	// precisely because it is the weakest of the three. A daemon configured with
	// a key source must not be silently opened by a stray variable instead.
	t.Setenv(MasterKeyEnv, "the-environment-passphrase")

	src, err := ParseKeySource(writeKey(t, "the-file-passphrase\n", 0o600))
	if err != nil {
		t.Fatal(err)
	}

	vaultPath := filepath.Join(t.TempDir(), "vault.age")
	v, err := OpenFrom(vaultPath, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("k", NewSecretString("value")); err != nil {
		t.Fatal(err)
	}
	if err := v.Save(); err != nil {
		t.Fatal(err)
	}

	// The environment's passphrase must not open it.
	if _, err := OpenWithKey(vaultPath, "the-environment-passphrase"); err == nil {
		t.Fatal("the vault was written with the environment key, not the key source")
	}
	if _, err := OpenWithKey(vaultPath, "the-file-passphrase"); err != nil {
		t.Fatalf("the vault was not written with the key source: %v", err)
	}
}
