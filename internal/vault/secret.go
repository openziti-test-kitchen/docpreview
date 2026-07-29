package vault

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
)

// redacted is what a Secret renders as in every string context.
const redacted = "***REDACTED***"

// Secret wraps a credential so that it cannot be printed by accident.
//
// The hazard this defends against is mundane and constant: a struct containing
// a GitHub App private key gets passed to log.Printf("%+v"), or marshalled into
// a debug endpoint, or embedded in an error message that ends up in a PR
// comment. Go's fmt and encoding/json both consult interfaces before falling
// back to reflection, so implementing Stringer, Formatter, GoStringer, and
// json.Marshaler closes every one of those paths at once.
//
// The value is only retrievable through Reveal, which is deliberately
// awkward to type and easy to grep for.
type Secret struct {
	value []byte
}

// NewSecret wraps b. The caller should not retain or mutate b afterwards.
func NewSecret(b []byte) Secret { return Secret{value: b} }

// NewSecretString wraps s.
func NewSecretString(s string) Secret { return Secret{value: []byte(s)} }

// Reveal returns the underlying bytes. Every call site is a place where a
// credential enters the clear, so keep them few and keep them shallow.
func (s Secret) Reveal() []byte { return s.value }

// RevealString returns the underlying value as a string.
func (s Secret) RevealString() string { return string(s.value) }

// IsZero reports whether the secret is unset.
func (s Secret) IsZero() bool { return len(s.value) == 0 }

// Equal compares two secrets in constant time.
func (s Secret) Equal(other Secret) bool {
	return subtle.ConstantTimeCompare(s.value, other.value) == 1
}

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// Format implements fmt.Formatter so that %v, %s, %q, %x and friends all yield
// the redaction rather than the value. Without this, %x on a []byte-backed
// type would happily hex-dump the credential.
func (s Secret) Format(f fmt.State, verb rune) {
	switch verb {
	case 'q':
		fmt.Fprintf(f, "%q", redacted)
	default:
		fmt.Fprint(f, redacted)
	}
}

// MarshalJSON keeps secrets out of any JSON the process emits.
//
// There is deliberately no UnmarshalJSON. The vault persists a plain
// map[string]string and converts on load, so no code path outside this package
// can round-trip a Secret through JSON and get the value back out.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }
