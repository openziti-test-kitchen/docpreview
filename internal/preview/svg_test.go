package preview

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSiteIconsAreWellFormedXML guards a failure with no error message.
//
// An SVG is XML, and XML forbids two consecutive hyphens inside a comment. The
// navbar logo shipped with a CSS custom property named in its comment — leading
// hyphen pair and all — which made the file malformed, and a browser answers that
// with a broken-image icon and nothing in any console. The favicon beside it, whose
// comment happened to avoid the sequence, rendered perfectly, so the two files
// disagreed for a reason invisible in either.
//
// Nothing else in this project parses these files, so nothing else would notice. A
// test that decodes them is the cheapest place to find out, and encoding/xml needs no
// dependency.
func TestSiteIconsAreWellFormedXML(t *testing.T) {
	dir := filepath.Join("..", "..", "www", "static", "img")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no documentation assets to check: %v", err)
	}

	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".svg") {
			continue
		}
		found++

		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}

		dec := xml.NewDecoder(strings.NewReader(string(body)))
		for {
			_, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("%s is not well-formed XML, so a browser will render a broken "+
					"image and say nothing: %v", e.Name(), err)
				break
			}
		}

		// The specific trap, named so the failure explains itself rather than
		// leaving somebody to work out what "invalid sequence" meant.
		for _, c := range comments(string(body)) {
			if strings.Contains(c, "--") {
				t.Errorf("%s has two consecutive hyphens inside a comment, which XML "+
					"forbids — usually a CSS custom property written with its leading "+
					"hyphen pair", e.Name())
			}
		}
	}

	if found == 0 {
		t.Error("no SVGs were checked, so this test is asserting nothing")
	}
}

// comments returns the body of each XML comment, found by scanning rather than by
// decoding: a malformed comment is exactly what this is looking for, and a decoder
// stops at the first one.
func comments(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			return out
		}
		s = s[i+4:]
		j := strings.Index(s, "-->")
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+3:]
	}
}
