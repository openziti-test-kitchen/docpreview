package buildlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/netfoundry/docpreview/internal/redact"
)

// ErrNoLog means there is no log for that preview or build.
var ErrNoLog = errors.New("no such build log")

// keepPerPreview is how many completed logs to retain for one preview.
//
// Enough to compare a failure against the build before it, which is the
// question anyone actually asks. Retaining every build of a long-lived pull
// request would accumulate without bound for no benefit; the TTL sweep is the
// backstop for previews nobody touches again.
const keepPerPreview = 5

// Store owns build logs on disk and tracks the ones being written right now.
//
// Every method tolerates a nil receiver. A store that could not be created —
// a full disk, a read-only data directory — should degrade to "builds run, they
// just are not captured" rather than take previews down or force a nil check at
// every call site.
type Store struct {
	dir string

	mu   sync.RWMutex
	live map[string]*Writer // preview ID -> the writer for its in-flight build
}

// NewStore returns a store rooted at dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the log directory: %w", err)
	}
	return &Store{dir: dir, live: map[string]*Writer{}}, nil
}

// previewDir is where one preview's logs live.
//
// Preview IDs are hex from a hash, so they cannot contain a separator — but
// this is the one place a caller-supplied string becomes a filesystem path, and
// checking costs nothing next to the cost of being wrong.
func (s *Store) previewDir(previewID string) (string, error) {
	if previewID == "" || strings.ContainsAny(previewID, `/\:.`) {
		return "", fmt.Errorf("invalid preview id %q", previewID)
	}
	return filepath.Join(s.dir, previewID), nil
}

// Begin starts a log for a new build and registers it as the live one.
//
// A preview only ever has one build in flight — the daemon supersedes rather
// than running two — so an existing writer means a previous build was
// abandoned. It is closed, which ends any tail attached to it.
func (s *Store) Begin(previewID, buildID string, r *redact.Redactor) (*Writer, error) {
	if s == nil {
		return nil, nil
	}
	dir, err := s.previewDir(previewID)
	if err != nil {
		return nil, err
	}
	if buildID == "" || strings.ContainsAny(buildID, `/\:`) {
		return nil, fmt.Errorf("invalid build id %q", buildID)
	}

	w, err := Create(filepath.Join(dir, buildID+".log"), r)
	if err != nil {
		return nil, err
	}

	// Swap under the lock, close outside it. Writer.Close flushes a partial
	// line, scrubs it, writes to the file and fans out to subscribers before
	// its final syscall — all of which would happen with the store's lock held,
	// blocking every Live and Finish caller. Live is on the request path for
	// both log endpoints, so that is a dashboard stalling on a file close.
	s.mu.Lock()
	old := s.live[previewID]
	s.live[previewID] = w
	s.mu.Unlock()

	if old != nil {
		old.Close()
	}

	return w, nil
}

// Finish closes a build's log and prunes older ones.
func (s *Store) Finish(previewID string, w *Writer) error {
	if s == nil || w == nil {
		return nil
	}
	s.mu.Lock()
	// Only clear the entry if it is still this writer. A superseding build may
	// already have replaced it, and clearing that would leave the new build's
	// tail unreachable.
	if s.live[previewID] == w {
		delete(s.live, previewID)
	}
	s.mu.Unlock()

	err := w.Close()
	s.prune(previewID)
	return err
}

// Live returns the writer for a preview's in-flight build, if there is one.
func (s *Store) Live(previewID string) (*Writer, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.live[previewID]
	return w, ok
}

// List returns a preview's stored logs, newest first.
func (s *Store) List(previewID string) ([]Meta, error) {
	if s == nil {
		return nil, nil
	}
	dir, err := s.previewDir(previewID)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	var out []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Meta{
			PreviewID: previewID,
			BuildID:   strings.TrimSuffix(e.Name(), ".log"),
			Path:      filepath.Join(dir, e.Name()),
			Size:      info.Size(),
			ModTime:   info.ModTime(),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// Latest returns a preview's most recent log.
func (s *Store) Latest(previewID string) (Meta, error) {
	logs, err := s.List(previewID)
	if err != nil {
		return Meta{}, err
	}
	if len(logs) == 0 {
		return Meta{}, ErrNoLog
	}
	return logs[0], nil
}

// Open returns a reader over one stored log.
//
// buildID is resolved against the store's own listing rather than joined
// straight onto a path, so a crafted value cannot address a file elsewhere.
func (s *Store) Open(previewID, buildID string) (*os.File, Meta, error) {
	if s == nil {
		return nil, Meta{}, ErrNoLog
	}
	logs, err := s.List(previewID)
	if err != nil {
		return nil, Meta{}, err
	}
	for _, m := range logs {
		if m.BuildID == buildID {
			f, err := os.Open(m.Path)
			if err != nil {
				return nil, Meta{}, err
			}
			return f, m, nil
		}
	}
	return nil, Meta{}, ErrNoLog
}

// prune keeps only the most recent logs for a preview.
func (s *Store) prune(previewID string) {
	logs, err := s.List(previewID)
	if err != nil {
		return
	}
	for i := keepPerPreview; i < len(logs); i++ {
		os.Remove(logs[i].Path)
	}
}

// Remove deletes every log for a preview. Called when its preview is torn down.
func (s *Store) Remove(previewID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if w := s.live[previewID]; w != nil {
		w.Close()
		delete(s.live, previewID)
	}
	s.mu.Unlock()

	dir, err := s.previewDir(previewID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// Sweep deletes logs older than ttl.
//
// The per-preview cap handles active pull requests; this handles the ones
// nobody will touch again, whose directories would otherwise sit there holding
// whatever their builds happened to print.
func (s *Store) Sweep(ttl time.Duration) (removed int, err error) {
	if s == nil {
		return 0, nil
	}
	if ttl <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-ttl)

	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", s.dir, err)
	}

	s.mu.RLock()
	live := make(map[string]bool, len(s.live))
	for id := range s.live {
		live[id] = true
	}
	s.mu.RUnlock()

	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] {
			continue
		}

		logs, err := s.List(e.Name())
		if err != nil || len(logs) == 0 {
			continue
		}
		// Judge the directory by its newest log; a preview rebuilt yesterday
		// keeps its older logs even if they predate the cutoff.
		if logs[0].ModTime.After(cutoff) {
			continue
		}

		// Re-check liveness under the lock immediately before deleting. The
		// snapshot above is stale by the time we get here, and Begin creates
		// the file before it registers the writer — so a build starting during
		// the sweep can be absent from the snapshot, have its log created, and
		// then have the directory removed out from under it.
		s.mu.RLock()
		_, nowLive := s.live[e.Name()]
		s.mu.RUnlock()
		if nowLive {
			continue
		}

		if err := os.RemoveAll(filepath.Join(s.dir, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}
