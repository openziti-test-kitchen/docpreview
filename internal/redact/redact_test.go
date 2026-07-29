package redact

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// The secret used throughout. Long enough to be realistic, distinctive enough
// that a partial leak is obvious in a failure message.
// The prefix is synthetic so secret scanners do not read this as a real
// provider's key. Redaction only needs a secret-shaped string.
const secret = "dpfake_9f2a1c4b7e01d3f5a8c6b2e4"

func TestScrubReplacesTheRawValue(t *testing.T) {
	r, _ := New([]string{secret})

	got := r.Scrub("npm ERR! auth token " + secret + " rejected")
	if strings.Contains(got, secret) {
		t.Fatalf("the secret survived: %q", got)
	}
	if !strings.Contains(got, Mask) {
		t.Errorf("no mask in the output: %q", got)
	}
}

func TestMaskIsFixedWidthRegardlessOfSecretLength(t *testing.T) {
	// The length of a credential is itself a clue — it distinguishes a 40-char
	// token from a 4-char PIN and tells anyone brute-forcing how much work they
	// face. A mask that mirrored the length would leak it.
	for _, s := range []string{
		"abcd",
		"abcdefgh",
		strings.Repeat("x", 64),
		strings.Repeat("y", 512),
	} {
		r, _ := New([]string{s})
		got := r.Scrub("value=" + s + " end")

		if got != "value="+Mask+" end" {
			t.Errorf("secret of length %d produced %q", len(s), got)
		}
		if len(Mask) != 5 {
			t.Fatalf("the mask is %d characters, not five", len(Mask))
		}
	}
}

func TestScrubCatchesEncodedForms(t *testing.T) {
	// A build tool rarely prints a credential exactly as given. These are the
	// transformations that actually show up in build output.
	r, _ := New([]string{secret})

	tests := map[string]string{
		"query escaped": url.QueryEscape(secret),
		"path escaped":  url.PathEscape(secret),
		"base64":        base64.StdEncoding.EncodeToString([]byte(secret)),
		"raw base64":    base64.RawStdEncoding.EncodeToString([]byte(secret)),
		"url base64":    base64.URLEncoding.EncodeToString([]byte(secret)),
	}

	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			got := r.Scrub("Authorization: Bearer " + encoded)
			if strings.Contains(got, encoded) {
				t.Errorf("the %s form survived: %q", name, got)
			}
		})
	}
}

func TestScrubCatchesJSONEscapedForm(t *testing.T) {
	// A secret containing a quote or a backslash appears differently once a
	// tool logs the JSON body it sent.
	awkward := `p@ss"word\with/slashes`
	r, _ := New([]string{awkward})

	logged := fmt.Sprintf(`{"token":%q}`, awkward)
	got := r.Scrub(logged)

	if strings.Contains(got, `p@ss`) {
		t.Errorf("the JSON-escaped form survived: %q", got)
	}
}

func TestLongerSecretsWinOverShorterPrefixes(t *testing.T) {
	// Registering both "abcd1234" and "abcd" must not turn the longer one into
	// "*****1234", leaving its tail exposed.
	short := "abcd"
	long := "abcd1234efgh"

	r, _ := New([]string{short, long})
	got := r.Scrub("token=" + long)

	if strings.Contains(got, "1234efgh") {
		t.Fatalf("a shorter secret matched inside a longer one, leaking its tail: %q", got)
	}
	if got != "token="+Mask {
		t.Errorf("got %q, want %q", got, "token="+Mask)
	}
}

func TestVeryShortValuesAreRefusedAndReported(t *testing.T) {
	// Redacting "a" would replace every "a" in the log, destroying the output
	// while telling an attacker the secret is one character. Declining is
	// right; declining silently is not.
	r, tooShort := New([]string{"a", "ab", "abc", secret})

	if len(tooShort) != 3 {
		t.Errorf("tooShort = %v, want the three values below the minimum", tooShort)
	}

	got := r.Scrub("a banana cabbage " + secret)
	if !strings.Contains(got, "banana") {
		t.Errorf("a short value was redacted anyway, destroying the log: %q", got)
	}
	if strings.Contains(got, secret) {
		t.Errorf("the real secret was not redacted: %q", got)
	}
}

func TestScrubHandlesRepeatsAndMultipleSecrets(t *testing.T) {
	a := "first_secret_value_aaaa"
	b := "second_secret_value_bbbb"
	r, _ := New([]string{a, b})

	got := r.Scrub(a + " and " + b + " and " + a + " again")
	if strings.Contains(got, a) || strings.Contains(got, b) {
		t.Fatalf("a secret survived: %q", got)
	}
	if n := strings.Count(got, Mask); n != 3 {
		t.Errorf("got %d masks, want 3: %q", n, got)
	}
}

func TestScrubErrorRedactsTheMessageAndKeepsTheChain(t *testing.T) {
	r, _ := New([]string{secret})

	sentinel := errors.New("underlying cause")
	wrapped := fmt.Errorf("running build with token %s: %w", secret, sentinel)

	scrubbed := r.ScrubError(wrapped)

	if strings.Contains(scrubbed.Error(), secret) {
		t.Fatalf("the secret survived in the error message: %v", scrubbed)
	}
	// The chain must survive, or errors.Is stops working everywhere upstream.
	if !errors.Is(scrubbed, sentinel) {
		t.Error("ScrubError broke the error chain")
	}
}

func TestScrubErrorOnNil(t *testing.T) {
	r, _ := New([]string{secret})
	if err := r.ScrubError(nil); err != nil {
		t.Errorf("ScrubError(nil) = %v", err)
	}
}

func TestInactiveRedactorIsATransparentPassthrough(t *testing.T) {
	r, _ := New(nil)

	if r.Active() {
		t.Error("a redactor with no secrets reports itself active")
	}
	const text = "nothing to hide here"
	if got := r.Scrub(text); got != text {
		t.Errorf("Scrub changed the text: %q", got)
	}
	err := errors.New("plain")
	if r.ScrubError(err) != err {
		t.Error("ScrubError wrapped an error for no reason")
	}
}

func TestNilRedactorIsSafe(t *testing.T) {
	var r *Redactor
	if got := r.Scrub("text"); got != "text" {
		t.Errorf("Scrub on a nil redactor = %q", got)
	}
	if r.Active() {
		t.Error("a nil redactor reports itself active")
	}
	if r.Count() != 0 {
		t.Error("a nil redactor reports a nonzero count")
	}
}

func TestScrubBytes(t *testing.T) {
	r, _ := New([]string{secret})

	got := r.ScrubBytes([]byte("line one\ntoken " + secret + "\nline three"))
	if strings.Contains(string(got), secret) {
		t.Errorf("the secret survived: %s", got)
	}
}

func TestScrubIsIdempotent(t *testing.T) {
	// The daemon scrubs a second time on the way to a comment, because a
	// failure can arrive from paths the builder never touched. Scrubbing
	// already-scrubbed text must not corrupt it.
	r, _ := New([]string{secret})

	once := r.Scrub("token " + secret)
	twice := r.Scrub(once)

	if once != twice {
		t.Errorf("scrubbing twice changed the result: %q then %q", once, twice)
	}
}

// TestRealisticBuildOutput is the case this package exists for: a build that
// prints its own environment, which npm does on failure and any script under
// `set -x` does always.
func TestRealisticBuildOutput(t *testing.T) {
	const (
		algolia = "9f2a1c4b7e01d3f5a8c6b2e4d1f3a5c7"
		npmTok  = "npm_AbCdEf1234567890GhIjKlMnOpQrSt"
	)

	r, _ := New([]string{algolia, npmTok})

	log := strings.Join([]string{
		"$ npm ci",
		"npm ERR! code E401",
		"npm ERR! Incorrect or missing password.",
		"npm ERR!   //registry.npmjs.org/:_authToken=" + npmTok,
		"$ npm run build",
		"+ ALGOLIA_WRITE_KEY=" + algolia,
		"[INFO] indexing to https://user:" + url.QueryEscape(algolia) + "@algolia.net/1/indexes",
		`{"apiKey":"` + algolia + `","index":"docs"}`,
		"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(npmTok)),
		"Error: request failed",
	}, "\n")

	got := r.Scrub(log)

	for name, s := range map[string]string{
		"algolia key":      algolia,
		"npm token":        npmTok,
		"url-encoded key":  url.QueryEscape(algolia),
		"base64 npm token": base64.StdEncoding.EncodeToString([]byte(npmTok)),
	} {
		if strings.Contains(got, s) {
			t.Errorf("%s survived redaction:\n%s", name, got)
		}
	}

	// The log has to remain readable, or nobody will use it to debug.
	for _, keep := range []string{"npm ERR! code E401", "Error: request failed", "indexing to"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction destroyed useful output, %q is gone:\n%s", keep, got)
		}
	}
}
