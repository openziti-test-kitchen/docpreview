package vault

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A KeySource is somewhere the master key can be read from without a human
// present.
//
// # Why this exists at all
//
// The vault is a secrets manager, so its master key is the secret-zero problem
// in miniature: the thing that protects everything else has nowhere inside the
// system to live. OWASP's answer is not to solve it but to move it — "you will
// often have to secure the primary secret of that secrets management solution
// in a secondary secrets management solution". That is what a KeySource is: the
// seam where the secondary mechanism plugs in.
//
// # Why not the environment
//
// $DOCPREVIEW_MASTER_KEY still works and is still the fallback, but it is no
// longer the recommended path. OWASP is direct about it: environment variables
// "are generally accessible to all processes and may be included in logs or
// system dumps", and using them is "not recommended unless the other methods
// are not possible". On this daemon specifically the variable is visible in the
// service definition, in a process listing, and in whatever shell history set
// it.
//
// # What each form is worth
//
// A file is protected by its permissions and its location, and nothing else. It
// survives a reboot, which is the reason to want it, and it means anyone who can
// read that path can read every secret in the vault. Keeping it out of DataDir
// is enforced (see config validation) because a key beside the ciphertext it
// decrypts is not encryption at rest, it is a filename change.
//
// A command is the form that reaches an actual secret manager — `op read`,
// `pass show`, `az keyvault secret show`. The key exists in this process for as
// long as it takes to derive an age identity, and nowhere else on the machine.
// That is the recommended shape, and the only one where the master key is not
// sitting somewhere permanently readable.
type KeySource struct {
	kind string // "", "file" or "exec"
	path string // kind == "file"
	argv []string
}

// SourceKindFile and SourceKindExec are the recognised schemes.
const (
	SourceKindFile = "file"
	SourceKindExec = "exec"
)

// execTimeout bounds a credential helper.
//
// A minute rather than a few seconds: `op read` can legitimately block on a
// biometric prompt or a device approval, and killing that at five seconds turns
// the recommended configuration into the one that does not work. A helper that
// has not answered in a minute is not waiting for a person.
const execTimeout = time.Minute

// ParseKeySource reads the config spelling of a key source.
//
// Accepted:
//
//	""                                  no source; the vault stays locked
//	file:/etc/docpreview/master.key     read the key from a file
//	D:\keys\docpreview.key              a bare path, same as file:
//	exec:op read op://ops/docpreview    run a command and take its stdout
//
// A bare path is accepted because `file:` in front of a Windows path reads like
// a mistake, and because the shorter spelling is the one people will write
// anyway.
func ParseKeySource(s string) (KeySource, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return KeySource{}, nil
	}

	scheme, rest, found := strings.Cut(s, ":")
	// A drive letter is not a scheme. "D:\keys\k" has to survive this, and no
	// scheme is one character long.
	if !found || len(scheme) < 2 {
		return fileSource(s)
	}

	switch strings.ToLower(scheme) {
	case SourceKindFile:
		return fileSource(rest)
	case SourceKindExec:
		argv, err := splitArgv(rest)
		if err != nil {
			return KeySource{}, fmt.Errorf("vault.key_source: %w", err)
		}
		if len(argv) == 0 {
			return KeySource{}, fmt.Errorf("vault.key_source: exec: needs a command")
		}
		return KeySource{kind: SourceKindExec, argv: argv}, nil
	default:
		// Not an unknown-scheme error: an unrecognised prefix is much more
		// likely to be a path than a scheme somebody invented.
		return fileSource(s)
	}
}

func fileSource(path string) (KeySource, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return KeySource{}, fmt.Errorf("vault.key_source: file: needs a path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return KeySource{}, fmt.Errorf("vault.key_source %q: %w", path, err)
	}
	return KeySource{kind: SourceKindFile, path: abs}, nil
}

// IsZero reports whether there is no source, meaning the vault can only be
// opened by a person.
func (k KeySource) IsZero() bool { return k.kind == "" }

// Kind is "file", "exec", or "" for no source.
func (k KeySource) Kind() string { return k.kind }

// Path is the resolved absolute file path, empty unless Kind is file.
func (k KeySource) Path() string { return k.path }

// Describe names the source for a log line. It never includes key material —
// for exec that means the command, which is a locator and not a secret, in the
// same way a file path is.
func (k KeySource) Describe() string {
	switch k.kind {
	case SourceKindFile:
		return "file " + k.path
	case SourceKindExec:
		return "command " + strings.Join(k.argv, " ")
	default:
		return "none"
	}
}

// Read fetches the key material.
//
// The returned string is the caller's to use and drop. It is not stored, logged,
// or cached: a source is re-read on the next Open, which is what makes rotating
// the master key a matter of changing the source rather than restarting into a
// migration.
func (k KeySource) Read() (string, error) {
	switch k.kind {
	case SourceKindFile:
		return k.readFile()
	case SourceKindExec:
		return k.readExec()
	default:
		return "", fmt.Errorf("%w: no key source is configured", ErrLocked)
	}
}

func (k KeySource) readFile() (string, error) {
	st, err := os.Stat(k.path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: the key file %s does not exist "+
				"(create one with: docpreview vault keygen -out %s)", ErrLocked, k.path, k.path)
		}
		return "", fmt.Errorf("reading the vault key file: %w", err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("vault.key_source %s is a directory", k.path)
	}

	// Refuse a key anyone else on the box can read.
	//
	// Only on Unix. Windows permissions are ACLs, which a mode bit does not
	// describe — os.Stat reports 0666 for an ordinary file regardless of who
	// can open it — so a mode check there would reject every key file for a
	// reason that is not true. See the same carve-out in Save.
	if !isWindows() && st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("the vault key file %s is mode %#o: it must not be readable "+
			"by group or other, since it decrypts every secret in the vault "+
			"(fix with: chmod 600 %s)", k.path, st.Mode().Perm(), k.path)
	}

	raw, err := os.ReadFile(k.path)
	if err != nil {
		return "", fmt.Errorf("reading the vault key file: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	zero(raw)
	if key == "" {
		return "", fmt.Errorf("%w: the key file %s is empty", ErrLocked, k.path)
	}
	return key, nil
}

func (k KeySource) readExec() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()

	// argv, never a shell. The command comes from a config file, and handing a
	// config value to `sh -c` would turn "where is my key" into arbitrary
	// command construction the moment any part of that path is templated.
	cmd := exec.CommandContext(ctx, k.argv[0], k.argv[1:]...)
	// Inherit stderr so a credential helper's own prompt or complaint reaches
	// the operator. Its stdout is the secret and is captured.
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%w: the key command %q did not finish within %s",
			ErrLocked, k.argv[0], execTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("the vault key command %q failed: %w", k.argv[0], err)
	}
	key := strings.TrimSpace(string(out))
	zero(out)
	if key == "" {
		return "", fmt.Errorf("%w: the key command %q printed nothing", ErrLocked, k.argv[0])
	}
	return key, nil
}

// splitArgv splits a command string on whitespace, honouring double quotes.
//
// Not a shell: no variable expansion, no globbing, no operators, no single
// quotes. Quotes exist only so that a Windows path with a space in it can be one
// argument, which is the only reason anybody needs quoting here. Anything more
// involved belongs in a script that the config points at.
func splitArgv(s string) ([]string, error) {
	var (
		args  []string
		cur   strings.Builder
		inArg bool
		quote bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			quote = !quote
			inArg = true
		case (r == ' ' || r == '\t') && !quote:
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote {
		return nil, fmt.Errorf("unterminated quote in %q", s)
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}
