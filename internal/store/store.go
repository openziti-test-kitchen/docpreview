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
	"strings"
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

-- One row per build attempt, which the previews table cannot express: it holds
-- the *current* state of a preview and is overwritten on every rebuild, so the
-- history of what a branch has done is nowhere.
--
-- Two things needed it. The build-log picker lists stored logs and could say
-- nothing about how each one ended, so choosing between them meant opening each
-- in turn. And the activity feed was an in-memory ring, so it was empty after
-- every restart — a feed of "recent activity" that forgot the moment the process
-- did.
--
-- Keyed by (preview, build) because the build id already carries the commit and a
-- timestamp, so the same commit rebuilt twice is two rows, which is what somebody
-- reading the history wants to see.
CREATE TABLE IF NOT EXISTS builds (
    preview_id  TEXT NOT NULL,
    build_id    TEXT NOT NULL,
    platform    TEXT NOT NULL DEFAULT '',
    owner       TEXT NOT NULL DEFAULT '',
    repo        TEXT NOT NULL DEFAULT '',
    number      INTEGER NOT NULL DEFAULT 0,
    branch      TEXT NOT NULL DEFAULT '',
    commit_sha  TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    started_at  INTEGER NOT NULL DEFAULT 0,
    finished_at INTEGER NOT NULL DEFAULT 0,
    -- This build's own share, when per-build publishing is on. Also declared in
    -- migrate(), because a database created before these existed gets them there
    -- and one created after gets them here.
    name        TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (preview_id, build_id)
);

CREATE INDEX IF NOT EXISTS builds_started_at ON builds(started_at);

-- A project is a repository docpreview is willing to build, and how to build it.
--
-- It exists to move the build instructions off the pull request. .docpreview.yml
-- lives in the branch, so on any repository where opening a pull request is not a
-- privilege, whoever opens one chooses the command that runs — and the local
-- driver runs it. An operator-authored row cannot be edited by a contributor.
--
-- Every build field is nullable-by-emptiness: empty means "no opinion, fall back
-- to the repository's own .docpreview.yml". That keeps a project row useful as
-- just an allowlist entry, which is the common case, without forcing an operator
-- to restate settings the repository already gets right.
--
-- Keyed by (platform, owner, repo) rather than by a surrogate id, because that is
-- what a webhook arrives carrying and a lookup per delivery should not need a
-- second query to resolve.
CREATE TABLE IF NOT EXISTS projects (
    platform      TEXT NOT NULL,
    owner         TEXT NOT NULL,
    repo          TEXT NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    build_dir     TEXT NOT NULL DEFAULT '',
    build_command TEXT NOT NULL DEFAULT '',
    build_output  TEXT NOT NULL DEFAULT '',
    base_url      TEXT NOT NULL DEFAULT '',
    detect_script TEXT NOT NULL DEFAULT '',
    driver        TEXT NOT NULL DEFAULT '',
    image         TEXT NOT NULL DEFAULT '',
    notes         TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (platform, owner, repo)
);

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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// migrate adds columns that were introduced after their table shipped.
//
// `CREATE TABLE IF NOT EXISTS` does nothing at all to a table that already exists,
// so a column added to the schema above appears only in databases created after the
// change — every existing one silently lacks it and every query naming it fails.
// This is the whole of the migration story: one list, applied every start.
//
// Each statement must be idempotent, because there is no version table deciding
// what has run. `ADD COLUMN` is not, so "duplicate column name" is treated as the
// success it is. That is the trade for having no schema versioning: the check is on
// the error text, and a future sqlite that rewords it would make these look failed.
// The alternative — a schema_version table — is worth adding at the second
// migration that cannot be expressed this way, not the first that can.
//
// Additive only. A column that has to change type or go away needs a real
// migration, and doing that here would run it again on every restart.
func migrate(db *sql.DB) error {
	stmts := []string{
		// A build's own share. Per-build publishing gives each build a URL of its
		// own, and without these a restart cannot tell the exposer which build
		// shares to keep — so Reap deletes every one of them on the first sweep.
		`ALTER TABLE builds ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE builds ADD COLUMN url TEXT NOT NULL DEFAULT ''`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrating: %s: %w", s, err)
		}
	}
	return nil
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

// Project is a repository docpreview will build, and how.
//
// Every build field is optional. Empty means "defer to the repository's own
// .docpreview.yml", so a row that only names a repository is a valid project and
// is the common case — an operator should not have to restate settings the
// repository already gets right in order to allow it.
type Project struct {
	Platform string `json:"platform"`
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	Enabled  bool   `json:"enabled"`

	BuildDir     string `json:"build_dir,omitempty"`
	BuildCommand string `json:"build_command,omitempty"`
	BuildOutput  string `json:"build_output,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	DetectScript string `json:"detect_script,omitempty"`

	// Driver and Image override the server-wide build.driver and build.image for
	// this project. Empty means the server default.
	Driver string `json:"driver,omitempty"`
	Image  string `json:"image,omitempty"`

	// Notes is free text for whoever added the project. It appears in the UI and
	// nowhere else; the build never reads it.
	Notes string `json:"notes,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Key is the project's identity, as it appears in a webhook.
func (p Project) Key() string { return p.Platform + ":" + p.Owner + "/" + p.Repo }

// SaveProject creates or replaces a project.
func (s *Store) SaveProject(ctx context.Context, p Project) error {
	now := time.Now().UnixMilli()
	created := now
	if !p.CreatedAt.IsZero() {
		created = p.CreatedAt.UnixMilli()
	}

	_, err := s.db.ExecContext(ctx, `
        INSERT INTO projects (platform, owner, repo, enabled, build_dir, build_command,
            build_output, base_url, detect_script, driver, image, notes, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(platform, owner, repo) DO UPDATE SET
            enabled       = excluded.enabled,
            build_dir     = excluded.build_dir,
            build_command = excluded.build_command,
            build_output  = excluded.build_output,
            base_url      = excluded.base_url,
            detect_script = excluded.detect_script,
            driver        = excluded.driver,
            image         = excluded.image,
            notes         = excluded.notes,
            updated_at    = excluded.updated_at`,
		p.Platform, p.Owner, p.Repo, p.Enabled, p.BuildDir, p.BuildCommand,
		p.BuildOutput, p.BaseURL, p.DetectScript, p.Driver, p.Image, p.Notes,
		created, now)
	if err != nil {
		return fmt.Errorf("saving project %s: %w", p.Key(), err)
	}
	return nil
}

// ErrNoProject is returned when a repository has no project row.
var ErrNoProject = errors.New("no such project")

// ProjectFor returns the project for a repository.
//
// ErrNoProject is an ordinary answer, not a failure: it is what every repository
// returns before anybody adds it, and the caller decides what that means.
func (s *Store) ProjectFor(ctx context.Context, platform, owner, repo string) (Project, error) {
	rows, err := s.queryProjects(ctx,
		`SELECT platform, owner, repo, enabled, build_dir, build_command, build_output,
                base_url, detect_script, driver, image, notes, created_at, updated_at
         FROM projects WHERE platform = ? AND owner = ? AND repo = ?`,
		platform, owner, repo)
	if err != nil {
		return Project{}, err
	}
	if len(rows) == 0 {
		return Project{}, fmt.Errorf("%w: %s:%s/%s", ErrNoProject, platform, owner, repo)
	}
	return rows[0], nil
}

// ListProjects returns every project, ordered for display.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	return s.queryProjects(ctx,
		`SELECT platform, owner, repo, enabled, build_dir, build_command, build_output,
                base_url, detect_script, driver, image, notes, created_at, updated_at
         FROM projects ORDER BY platform, owner, repo`)
}

// DeleteProject removes a project. Removing one does not touch its previews:
// those are live shares and database rows with their own lifecycle, and silently
// tearing them down because somebody edited a list would be a surprise.
func (s *Store) DeleteProject(ctx context.Context, platform, owner, repo string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM projects WHERE platform = ? AND owner = ? AND repo = ?`,
		platform, owner, repo)
	if err != nil {
		return fmt.Errorf("deleting project %s:%s/%s: %w", platform, owner, repo, err)
	}
	return nil
}

func (s *Store) queryProjects(ctx context.Context, query string, args ...any) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		var created, updated int64
		if err := rows.Scan(&p.Platform, &p.Owner, &p.Repo, &p.Enabled, &p.BuildDir,
			&p.BuildCommand, &p.BuildOutput, &p.BaseURL, &p.DetectScript, &p.Driver,
			&p.Image, &p.Notes, &created, &updated); err != nil {
			return nil, err
		}
		p.CreatedAt = time.UnixMilli(created)
		p.UpdatedAt = time.UnixMilli(updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Build is one attempt at building a preview.
type Build struct {
	PreviewID  string            `json:"preview_id"`
	BuildID    string            `json:"build_id"`
	PR         model.PullRequest `json:"pr"`
	Commit     string            `json:"commit"`
	State      string            `json:"state"`
	Reason     string            `json:"reason,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at,omitempty"`

	// Name and URL are this build's own share, empty when it has none — a failed
	// or skipped build, or a daemon with per-build publishing off.
	//
	// Recorded rather than derived, for the same reason a preview's URL is: after a
	// restart the exposer has to be told which shares to keep, and a share whose
	// key nothing remembers is indistinguishable from an orphan and gets reaped.
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

// SaveBuild records or updates one build attempt.
//
// Upsert on (preview, build) so the same row can be written twice: once when the
// build starts, so a build in flight is visible in the history rather than
// appearing only after it ends, and once when it finishes with its outcome.
func (s *Store) SaveBuild(ctx context.Context, b Build) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO builds (preview_id, build_id, platform, owner, repo, number, branch,
            commit_sha, state, reason, started_at, finished_at, name, url)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(preview_id, build_id) DO UPDATE SET
            state       = excluded.state,
            reason      = excluded.reason,
            finished_at = excluded.finished_at,
            -- Only when the new row carries one. The row is written twice, and the
            -- first write — at build start — has no share yet; taking its empty
            -- value on the second would erase the URL the publish just produced if
            -- the writes ever arrive out of order.
            name        = CASE WHEN excluded.name <> '' THEN excluded.name ELSE builds.name END,
            url         = CASE WHEN excluded.url  <> '' THEN excluded.url  ELSE builds.url  END`,
		b.PreviewID, b.BuildID, string(b.PR.Repo.Platform), b.PR.Repo.Owner, b.PR.Repo.Name,
		b.PR.Number, b.PR.Branch, b.Commit, b.State, b.Reason,
		b.StartedAt.UnixMilli(), b.FinishedAt.UnixMilli(), b.Name, b.URL)
	if err != nil {
		return fmt.Errorf("saving build %s/%s: %w", b.PreviewID, b.BuildID, err)
	}
	return nil
}

// BuildsFor returns a preview's build attempts, newest first.
func (s *Store) BuildsFor(ctx context.Context, previewID string) ([]Build, error) {
	return s.queryBuilds(ctx,
		`SELECT preview_id, build_id, platform, owner, repo, number, branch,
                commit_sha, state, reason, started_at, finished_at, name, url
         FROM builds WHERE preview_id = ? ORDER BY started_at DESC`, previewID)
}

// RecentBuilds returns the newest build attempts across every preview.
//
// This is what backfills the activity feed after a restart. Bounded rather than
// paged: the feed is a window onto what just happened, and a caller wanting the
// whole history wants the build logs on disk instead.
func (s *Store) RecentBuilds(ctx context.Context, limit int) ([]Build, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryBuilds(ctx,
		`SELECT preview_id, build_id, platform, owner, repo, number, branch,
                commit_sha, state, reason, started_at, finished_at, name, url
         FROM builds ORDER BY started_at DESC LIMIT ?`, limit)
}

func (s *Store) queryBuilds(ctx context.Context, query string, args ...any) ([]Build, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}
	defer rows.Close()

	var out []Build
	for rows.Next() {
		var b Build
		var platform string
		var started, finished int64
		if err := rows.Scan(&b.PreviewID, &b.BuildID, &platform, &b.PR.Repo.Owner,
			&b.PR.Repo.Name, &b.PR.Number, &b.PR.Branch, &b.Commit, &b.State, &b.Reason,
			&started, &finished, &b.Name, &b.URL); err != nil {
			return nil, err
		}
		b.PR.Repo.Platform = model.Platform(platform)
		b.StartedAt = time.UnixMilli(started)
		if finished > 0 {
			b.FinishedAt = time.UnixMilli(finished)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PruneBuilds drops build rows older than cutoff.
//
// The history is bounded the same way build logs are, and for the same reason: a
// row per push accumulates forever on a busy repository. Kept independent of the
// log files rather than derived from them, because a log can fail to open while
// the build still happened and belongs in the history.
func (s *Store) PruneBuilds(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM builds WHERE started_at < ?`, cutoff.UnixMilli())
	if err != nil {
		return fmt.Errorf("pruning builds: %w", err)
	}
	return nil
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
