// Package store persists the two pieces of state docpreview cannot afford to
// keep only in memory: the queue of pending builds, and the set of previews
// that are supposed to be live.
//
// Both need to survive a restart. A queue that does not means a push landing
// during a deploy is silently dropped and the reviewer waits forever for a
// comment that will never come. A preview table that does not means the process
// comes back with no idea which zrok shares belong to it, and either leaks them
// forever or deletes shares it does not own.
//
// sqlite via modernc.org/sqlite, which is a pure-Go translation with no cgo, so
// the binary cross-compiles and runs on a Windows box without a toolchain.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// Store is the persistent state.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    preview_id  TEXT PRIMARY KEY,
    payload     TEXT NOT NULL,
    enqueued_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS previews (
    preview_id      TEXT PRIMARY KEY,
    platform        TEXT NOT NULL,
    owner           TEXT NOT NULL,
    repo            TEXT NOT NULL,
    number          INTEGER NOT NULL,
    branch          TEXT NOT NULL,
    installation_id INTEGER NOT NULL DEFAULT 0,
    name            TEXT NOT NULL DEFAULT '',
    url             TEXT NOT NULL DEFAULT '',
    base_url        TEXT NOT NULL DEFAULT '/',
    artifact_dir    TEXT NOT NULL DEFAULT '',
    commit_sha      TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    updated_at      INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS previews_updated_at ON previews(updated_at);

-- Comments exist only for the local platform, which has nowhere else to put
-- them. GitHub and Bitbucket keep the comment on the pull request, which is the
-- point: the comment is self-identifying by its marker and needs no record
-- here. See scm.Marker.
CREATE TABLE IF NOT EXISTS comments (
    preview_id TEXT PRIMARY KEY,
    platform   TEXT NOT NULL,
    owner      TEXT NOT NULL,
    repo       TEXT NOT NULL,
    number     INTEGER NOT NULL,
    branch     TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL,
    revision   INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
`

// Open creates or opens the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating data directory: %w", err)
	}

	// WAL keeps the reaper's reads from blocking a worker's writes. busy_timeout
	// converts the "database is locked" error — which sqlite returns
	// immediately by default, and which would surface as a random build failure
	// under concurrency — into a short wait.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening database: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Enqueue records a pending build for a pull request, replacing any build
// already pending for the same one.
//
// Replacement rather than accumulation is the whole point. A reviewer fixing
// typos pushes five commits in two minutes; building all five wastes four
// builds and, worse, publishes four previews the reviewer will never look at
// before the fifth replaces them. Only the newest commit matters.
func (s *Store) Enqueue(ctx context.Context, pr model.PullRequest) error {
	payload, err := json.Marshal(pr)
	if err != nil {
		return fmt.Errorf("encoding job: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO jobs (preview_id, payload, enqueued_at)
        VALUES (?, ?, ?)
        ON CONFLICT(preview_id) DO UPDATE SET
            payload     = excluded.payload,
            enqueued_at = excluded.enqueued_at`,
		pr.PreviewID(), string(payload), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("enqueuing %s: %w", pr, err)
	}
	return nil
}

// ErrNoJobs is returned by Claim when the queue is empty.
var ErrNoJobs = errors.New("no pending jobs")

// Claim removes and returns the oldest pending job.
//
// DELETE ... RETURNING is atomic in sqlite, so two workers racing for the same
// job cannot both get it. Deleting rather than marking it "running" means a
// crash mid-build loses that build — which is the right trade, because the
// alternative is a row stuck in "running" forever after a hard kill, and
// distinguishing "running" from "abandoned" needs heartbeats nobody wants to
// operate. A lost build is recovered by pushing again, or by the next webhook.
func (s *Store) Claim(ctx context.Context) (model.PullRequest, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `
        DELETE FROM jobs
        WHERE preview_id = (SELECT preview_id FROM jobs ORDER BY enqueued_at LIMIT 1)
        RETURNING payload`).Scan(&payload)

	if errors.Is(err, sql.ErrNoRows) {
		return model.PullRequest{}, ErrNoJobs
	}
	if err != nil {
		return model.PullRequest{}, fmt.Errorf("claiming job: %w", err)
	}

	var pr model.PullRequest
	if err := json.Unmarshal([]byte(payload), &pr); err != nil {
		return model.PullRequest{}, fmt.Errorf("decoding job: %w", err)
	}
	return pr, nil
}

// PendingCount reports how many builds are queued.
func (s *Store) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&n)
	return n, err
}

// PendingJob is a queued build and when it was queued.
//
// The timestamp is carried out of the store because the dashboard shows a
// relative age against it, and "how long has this been waiting" is the question
// somebody watching a queue is actually asking. Without it the status assembly
// had nothing better to show for a re-queued preview than whatever its last
// finished build left behind, which read as hours old the moment it was enqueued.
type PendingJob struct {
	PR         model.PullRequest
	EnqueuedAt time.Time
}

// PendingJobs returns the queued builds, oldest first.
//
// PendingCount answers "how many", which is all a header needs. A dashboard
// that lets you click the number and see which ones needs the rows, and a
// queued build has no preview record yet — the first build of a branch exists
// nowhere but here until it finishes.
func (s *Store) PendingJobs(ctx context.Context) ([]PendingJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload, enqueued_at FROM jobs ORDER BY enqueued_at`)
	if err != nil {
		return nil, fmt.Errorf("listing pending jobs: %w", err)
	}
	defer rows.Close()

	var out []PendingJob
	for rows.Next() {
		var payload string
		var enqueued int64
		if err := rows.Scan(&payload, &enqueued); err != nil {
			return nil, err
		}
		var pr model.PullRequest
		if err := json.Unmarshal([]byte(payload), &pr); err != nil {
			return nil, fmt.Errorf("decoding job: %w", err)
		}
		out = append(out, PendingJob{PR: pr, EnqueuedAt: time.UnixMilli(enqueued)})
	}
	return out, rows.Err()
}

// Preview is a recorded preview.
type Preview struct {
	PreviewID   string
	PR          model.PullRequest
	Name        string
	URL         string
	BaseURL     string
	ArtifactDir string
	Commit      string
	State       scm.State
	Reason      string
	UpdatedAt   time.Time
}

// SavePreview records or updates a preview.
func (s *Store) SavePreview(ctx context.Context, p Preview) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO previews (
            preview_id, platform, owner, repo, number, branch, installation_id,
            name, url, base_url, artifact_dir, commit_sha, state, reason, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(preview_id) DO UPDATE SET
            branch          = excluded.branch,
            installation_id = excluded.installation_id,
            name            = excluded.name,
            url             = excluded.url,
            base_url        = excluded.base_url,
            artifact_dir    = excluded.artifact_dir,
            commit_sha      = excluded.commit_sha,
            state           = excluded.state,
            reason          = excluded.reason,
            updated_at      = excluded.updated_at`,
		p.PreviewID, p.PR.Repo.Platform, p.PR.Repo.Owner, p.PR.Repo.Name, p.PR.Number,
		p.PR.Branch, p.PR.InstallationID, p.Name, p.URL, p.BaseURL, p.ArtifactDir,
		p.Commit, string(p.State), p.Reason, p.UpdatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("saving preview %s: %w", p.PreviewID, err)
	}
	return nil
}

// DeletePreview forgets a preview.
func (s *Store) DeletePreview(ctx context.Context, previewID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM previews WHERE preview_id = ?`, previewID)
	return err
}

// ListPreviews returns every recorded preview, newest first.
func (s *Store) ListPreviews(ctx context.Context) ([]Preview, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT preview_id, platform, owner, repo, number, branch, installation_id,
               name, url, base_url, artifact_dir, commit_sha, state, reason, updated_at
        FROM previews ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing previews: %w", err)
	}
	defer rows.Close()

	var out []Preview
	for rows.Next() {
		var p Preview
		var platform, state string
		var updated int64
		if err := rows.Scan(&p.PreviewID, &platform, &p.PR.Repo.Owner, &p.PR.Repo.Name,
			&p.PR.Number, &p.PR.Branch, &p.PR.InstallationID, &p.Name, &p.URL, &p.BaseURL,
			&p.ArtifactDir, &p.Commit, &state, &p.Reason, &updated); err != nil {
			return nil, fmt.Errorf("scanning preview: %w", err)
		}
		p.PR.Repo.Platform = model.Platform(platform)
		p.PR.HeadSHA = p.Commit
		p.State = scm.State(state)
		p.UpdatedAt = time.UnixMilli(updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Comment is a pull request comment docpreview owns.
type Comment struct {
	PreviewID string
	PR        model.PullRequest
	Body      string

	// Revision counts how many times this comment has been written. It exists
	// to make the edit-in-place behaviour visible: on a hosted platform you can
	// see it in the comment's edit history, and locally there is nowhere else
	// to look.
	Revision  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertComment writes docpreview's comment for a pull request, creating it on
// the first call and editing it thereafter.
//
// The revision increments and created_at is preserved, so "one comment, updated
// five times" stays distinguishable from "five comments".
func (s *Store) UpsertComment(ctx context.Context, c Comment) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO comments (
            preview_id, platform, owner, repo, number, branch, body,
            revision, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
        ON CONFLICT(preview_id) DO UPDATE SET
            branch     = excluded.branch,
            body       = excluded.body,
            revision   = comments.revision + 1,
            updated_at = excluded.updated_at`,
		c.PreviewID, c.PR.Repo.Platform, c.PR.Repo.Owner, c.PR.Repo.Name,
		c.PR.Number, c.PR.Branch, c.Body, now, now)
	if err != nil {
		return fmt.Errorf("saving comment for %s: %w", c.PR, err)
	}
	return nil
}

// GetComment returns the comment for a preview, if there is one.
func (s *Store) GetComment(ctx context.Context, previewID string) (Comment, bool, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT preview_id, platform, owner, repo, number, branch, body,
               revision, created_at, updated_at
        FROM comments WHERE preview_id = ?`, previewID)

	c, err := scanComment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Comment{}, false, nil
	}
	if err != nil {
		return Comment{}, false, err
	}
	return c, true, nil
}

// ListComments returns every comment, newest first.
func (s *Store) ListComments(ctx context.Context) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT preview_id, platform, owner, repo, number, branch, body,
               revision, created_at, updated_at
        FROM comments ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing comments: %w", err)
	}
	defer rows.Close()

	var out []Comment
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteComment removes a comment.
func (s *Store) DeleteComment(ctx context.Context, previewID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM comments WHERE preview_id = ?`, previewID)
	return err
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanComment(s scanner) (Comment, error) {
	var c Comment
	var platform string
	var created, updated int64

	if err := s.Scan(&c.PreviewID, &platform, &c.PR.Repo.Owner, &c.PR.Repo.Name,
		&c.PR.Number, &c.PR.Branch, &c.Body, &c.Revision, &created, &updated); err != nil {
		return Comment{}, err
	}
	c.PR.Repo.Platform = model.Platform(platform)
	c.CreatedAt = time.UnixMilli(created)
	c.UpdatedAt = time.UnixMilli(updated)
	return c, nil
}

// ExpiredPreviews returns previews not updated within ttl.
func (s *Store) ExpiredPreviews(ctx context.Context, ttl time.Duration) ([]Preview, error) {
	all, err := s.ListPreviews(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-ttl)
	var out []Preview
	for _, p := range all {
		if p.UpdatedAt.Before(cutoff) {
			out = append(out, p)
		}
	}
	return out, nil
}
