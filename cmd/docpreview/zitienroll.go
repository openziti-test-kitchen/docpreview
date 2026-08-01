package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/enroll"

	"github.com/netfoundry/docpreview/internal/config"
)

// enrollZitiIdentity turns a one-time enrolment JWT into an identity file on disk.
//
// # Why this exists rather than "run ziti edge enroll"
//
// It was the last thing on the exposer panel that could not be done from a browser, and the reason
// was a second binary rather than anything intrinsic: `ziti edge enroll` reads a JWT, generates a
// key pair, exchanges the token with the controller and writes a JSON identity. The Go SDK docpreview
// already depends on exposes exactly that — `enroll.Enroll` — so the whole step is this function.
//
// # The one-time token, and why the write happens before anything is reported
//
// The token is one-time in the strict sense. The controller marks it spent on the exchange, and the
// private key generated during enrolment exists nowhere but the file written here. So a failure
// *after* the exchange is unrecoverable: the identity exists on the controller, nothing can
// authenticate as it, and the operator's only route back is deleting it and issuing a new token.
// That is why the file is written immediately, with no work between the exchange and the write, and
// why the error when the write fails says what was lost.
//
// # Where it writes
//
// `<data_dir>/ziti/<name>.json`, beside the vault, for the reason the zrok environment lives there:
// it is a private key, so it belongs with the other credential material and in the directory the
// migration runbook already says to copy and keep private. 0600, and the directory 0700.
func enrollZitiIdentity(_ context.Context, cfg config.Server, jwtToken, name string) (string, error) {
	jwtToken = strings.TrimSpace(jwtToken)
	if jwtToken == "" {
		return "", errors.New("no enrolment token")
	}

	// Parsed before anything else, because a mistyped or expired token is the common failure and
	// it is knowable locally. The alternative is a request to the controller whose refusal says
	// only "unauthorized".
	claims, token, err := enroll.ParseToken(jwtToken)
	if err != nil {
		return "", fmt.Errorf("that is not a usable enrolment token: %w "+
			"(it is the JWT from `ziti edge create identity`, and it expires)", err)
	}

	// The token carries no name — its claims are the enrolment method, the controllers and the
	// registered JWT set, whose Subject is the identity *id* rather than anything an operator
	// would recognise. So the default is the product's own name, which is also what
	// `docpreview configure ziti` creates.
	if name == "" {
		name = "docpreview"
	}
	// A filename, not a display name: this becomes a path, and a ziti identity name may contain
	// anything. Restricted rather than escaped, because the file is referenced from a config
	// setting and a quoted path there is a different class of problem.
	safe := zitiFileName(name)
	if safe == "" {
		return "", fmt.Errorf("the identity name %q has no characters usable in a filename; "+
			"pass a name", name)
	}

	dir := filepath.Join(cfg.DataDir, "ziti")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, safe+".json")

	// Refused rather than overwritten. The file is the only proof of an enrolled identity, and
	// clobbering it destroys that with no error and nothing to restore from — the same rule
	// `vault keygen -out` follows, for the same reason.
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists; docpreview will not overwrite an identity file, "+
			"since it is the only proof of that identity — move it aside first", path)
	}

	// KeyAlg is not optional, and the SDK does not say so politely: left unset, `enroll.Enroll`
	// reaches a switch with no default and **panics** with "invalid KeyAlg specified: ". Found by
	// running it — the first enrolment against a real controller took the handler down with a
	// panic that `net/http` caught, so the request died with no response and the log had a stack
	// trace where an error message should have been.
	//
	// EC rather than RSA: smaller keys, faster handshakes, and it is what `ziti edge enroll`
	// defaults to, so an identity enrolled here matches one enrolled with the CLI.
	var alg ziti.KeyAlgVar
	if err := alg.Set("EC"); err != nil {
		return "", fmt.Errorf("selecting the key algorithm: %w", err)
	}

	idCfg, err := enroll.Enroll(enroll.EnrollmentFlags{
		Token:     claims,
		JwtToken:  token,
		JwtString: jwtToken,
		IDName:    name,
		KeyAlg:    alg,
	})
	if err != nil {
		return "", fmt.Errorf("the controller refused the enrolment: %w", err)
	}

	body, err := json.MarshalIndent(idCfg, "", "  ")
	if err != nil {
		// The exchange has happened, so this is the unrecoverable case. Said plainly, because
		// the next thing the operator needs to know is that the token is spent.
		return "", fmt.Errorf("the identity enrolled but could not be encoded (%w) — the "+
			"enrolment token is now spent, so delete the identity on the controller and issue "+
			"a new one", err)
	}
	// 0600: this file is the private key that lets anything host docpreview's services.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", fmt.Errorf("the identity enrolled but %s could not be written (%w) — the "+
			"enrolment token is now spent, so delete the identity on the controller and issue "+
			"a new one", path, err)
	}
	return path, nil
}

// zitiFileName reduces an identity name to something usable as a filename.
//
// Letters, digits, dash, dot and underscore survive; anything else becomes a dash, and runs of
// dashes collapse. Not a general sanitizer — it exists so that a ziti identity called
// "docpreview (aws) / east" cannot become a path with a directory separator in it.
func zitiFileName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		ok := r == '-' || r == '.' || r == '_' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-.")
}
