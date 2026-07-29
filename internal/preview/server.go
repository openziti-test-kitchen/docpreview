// Package preview serves a built documentation site over HTTP.
//
// This is deliberately not `docusaurus serve` in a subprocess. A built
// Docusaurus site is a directory of static files; serving it needs a file
// server, not a Node process per preview holding a hundred megabytes of
// resident memory and its own port. Twenty open pull requests should cost
// twenty directories on disk and one Go process, not twenty Node processes.
package preview

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Site is an http.Handler serving one built site at one base URL.
type Site struct {
	handler http.Handler

	// Dir is the directory being served.
	Dir string

	// BaseURL is the normalized path prefix the site is mounted at.
	BaseURL string
}

// New builds a handler for the site in dir, mounted at baseURL.
//
// baseURL must be the same value the site was built with. The builder verifies
// that they agree before anything reaches this package, so a mismatch here is a
// programming error rather than a configuration one.
func New(dir, baseURL string) (*Site, error) {
	stat, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("serving %s: %w", dir, err)
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("serving %s: not a directory", dir)
	}
	if baseURL == "" {
		baseURL = "/"
	}

	s := &Site{Dir: dir, BaseURL: baseURL}

	files := &fileServer{root: dir, notFound: filepath.Join(dir, "404.html")}

	mux := http.NewServeMux()
	if baseURL == "/" {
		mux.Handle("/", files)
	} else {
		mux.Handle(baseURL, http.StripPrefix(strings.TrimSuffix(baseURL, "/"), files))
		// A reviewer who clicks a bare origin link should land on the site
		// rather than on a 404, so redirect the root to the mount point.
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, baseURL, http.StatusFound)
		})
	}

	s.handler = withPreviewHeaders(mux)
	return s, nil
}

func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// fileServer serves static files with a Docusaurus-shaped 404 fallback.
type fileServer struct {
	root     string
	notFound string
}

func (f *fileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Rejecting anything but GET and HEAD keeps the surface as small as the
	// content: this is a directory of HTML, and there is nothing to POST to.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// http.Dir refuses paths that escape the root and rejects the Windows
	// device names and alternate separators that a hand-crafted request might
	// try, so path traversal is handled for us.
	dir := http.Dir(f.root)

	upstream := &statusRecorder{ResponseWriter: w}
	http.FileServer(dir).ServeHTTP(upstream, r)

	if upstream.status != http.StatusNotFound || upstream.wrote {
		return
	}
	f.serveNotFound(w, r)
}

func (f *fileServer) serveNotFound(w http.ResponseWriter, r *http.Request) {
	page, err := os.ReadFile(f.notFound)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		w.Write(page)
	}
}

// statusRecorder lets the file server's 404 be intercepted before its body is
// written, so the site's own 404 page can be substituted.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	if code == http.StatusNotFound {
		// Swallow the header so serveNotFound can write its own.
		return
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == http.StatusNotFound {
		// Swallow http.FileServer's plain-text "404 page not found" body.
		return len(b), nil
	}
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// withPreviewHeaders adds the headers every preview should carry.
func withPreviewHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// A preview is a snapshot of one commit and is replaced wholesale on
		// the next push. Caching it means a reviewer refreshes and sees the
		// version they already rejected, which is worse than a slow page.
		h.Set("Cache-Control", "no-store, must-revalidate")

		// Unreleased documentation has no business in a search index.
		h.Set("X-Robots-Tag", "noindex, nofollow")

		// The content is untrusted in the sense that anyone who can open a pull
		// request can put arbitrary HTML in it. These do not make that safe,
		// but they stop the obvious escalations.
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")

		next.ServeHTTP(w, r)
	})
}
