package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/preview"
)

func TestPreviewUnderTheLocalExposerServesTheURLItPrints(t *testing.T) {
	// `docpreview preview` runs no daemon, so under the local exposer it used to
	// print a URL on the daemon's port that nothing in this process answered.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>hello</p>"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := expose.NewLocal(slog.New(slog.DiscardHandler), "http://127.0.0.1:8471")
	stop, err := serveMounts(l)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	base := foldMountPath(l.MountPath("my-docs"), "/")
	site, err := preview.New(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := l.Publish(context.Background(), expose.Spec{PreviewID: "manual-my-docs", Name: "my-docs", BaseURL: base}, site)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	if strings.Contains(pub.URL, ":8471") {
		t.Fatalf("URL %s still points at the daemon's port", pub.URL)
	}
	r, err := http.Get(pub.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || !strings.Contains(string(b), "hello") {
		t.Errorf("GET %s = %d %q, want 200 and the page", pub.URL, r.StatusCode, b)
	}
}

func TestFoldMountPath(t *testing.T) {
	for _, c := range []struct{ base, want string }{
		{"", "/preview/x/"},
		{"/", "/preview/x/"},
		{"/docs/", "/preview/x/docs/"},
	} {
		if got := foldMountPath("/preview/x/", c.base); got != c.want {
			t.Errorf("foldMountPath(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}
