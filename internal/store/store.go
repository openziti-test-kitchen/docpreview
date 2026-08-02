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

-- Settings an operator changed from the dashboard.
--
-- The config file stays the source of the *default*; this holds the overrides. The split
-- exists because config.yml is written by hand and carries comments explaining why each
-- value is what it is — the most valuable thing in the file — and a daemon that rewrote it
-- to store one string would delete them.
--
-- So: config is what a fresh installation starts from, this is what an operator has since
-- decided, and the read path prefers this. One row per setting rather than a column per
-- setting, because the alternative is a migration for every new one.
--
-- Deliberately not a home for credentials. Those are in the vault, encrypted; this table is
-- plaintext and anything in it should be readable over somebody's shoulder.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Pull requests this installation has been told not to build.
--
-- A preview is created by discovery — a webhook delivery, or the scan that runs when a
-- project is added — and discovery cannot know which pull requests an operator cares
-- about. Removing the preview alone is not an answer: the next delivery or the next
-- scan finds the pull request again and rebuilds it, so "remove" without a record of
-- the decision reads as the removal having silently failed.
--
-- Keyed by repository and number rather than by preview id, because the preview is
-- deleted at the same moment this row is written and a reopened pull request gets a
-- new preview for the same number.
--
-- Deliberately not a column on previews. The row has to outlive the preview, which is
-- the entire point.
CREATE TABLE IF NOT EXISTS ignored_prs (
    platform   TEXT NOT NULL,
    owner      TEXT NOT NULL,
    repo       TEXT NOT NULL,
    number     INTEGER NOT NULL,
    branch     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (platform, owner, repo, number)
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
		// A label and a badge, so a list of ten projects can be scanned. Neither
		// affects a build: the identity a webhook is matched against is still
		// platform/owner/repo.
		`ALTER TABLE projects ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN avatar TEXT NOT NULL DEFAULT ''`,
		// A per-project build timeout. Stored as the text a human typed ("45m"), not
		// as a number of seconds: it is displayed in the form it was entered, and a
		// column of 2700s is a unit somebody has to work out.
		`ALTER TABLE projects ADD COLUMN timeout TEXT NOT NULL DEFAULT ''`,
		// Whether the repository needs a credential to clone. Advisory, and the reason the
		// form can warn about a missing token without nagging every public repository.
		`ALTER TABLE projects ADD COLUMN private INTEGER NOT NULL DEFAULT 0`,
		// A framework preset id, supplying the build command and output for the fields
		// left blank. Empty means the repository's own configuration decides, which is
		// what every existing row gets.
		`ALTER TABLE projects ADD COLUMN framework TEXT NOT NULL DEFAULT ''`,
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
// somebody watching a queue is actually asking. Without it, status assembly has
// nothing better to show for a re-queued preview than whatever its last finished
// build left behind, which reads as hours old the moment it is enqueued.
type PendingJob struct {
	PR         model.PullRequest
	EnqueuedAt time.Time
}

// Dequeue removes a preview's pending build, reporting whether there was one.
//
// For cancelling a build that has not started yet. Cancelling a *running* build is a
// context cancellation and touches nothing here; a queued one has no context to cancel, so
// the only way to stop it is to take it out of the queue before a worker claims it.
//
// Teardown is the second caller, and the reason this is not only about a button. Without it,
// tearing a preview down would leave its queued build behind for a worker to pick up minutes
// later — rebuilding, recording and republishing a preview an operator had deliberately
// deleted. See Daemon.teardown.
//
// It reports what it did rather than returning nothing, because of the race with Claim: a
// worker can take the job between the dashboard drawing the button and the click arriving,
// and the caller has to tell "removed it" from "too late, it is running" so it can cancel
// the running one instead.
func (s *Store) Dequeue(ctx context.Context, previewID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE preview_id = ?`, previewID)
	if err != nil {
		return false, fmt.Errorf("dequeueing %s: %w", previewID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
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

	// Framework is a preset id — "docusaurus", "mkdocs" — supplying the build command and
	// output directory for the fields left blank below. Empty means no preset, and the
	// repository's own .docpreview.yml decides.
	//
	// Stored rather than resolved at save time, so the preset's values are not frozen into
	// the row: a corrected default in a later version applies to every project that named
	// the framework instead of only to new ones. See config.Frameworks.
	Framework string `json:"framework,omitempty"`

	BuildDir     string `json:"build_dir,omitempty"`
	BuildCommand string `json:"build_command,omitempty"`
	BuildOutput  string `json:"build_output,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	DetectScript string `json:"detect_script,omitempty"`

	// Driver and Image override the server-wide build.driver and build.image for
	// this project. Empty means the server default.
	Driver string `json:"driver,omitempty"`
	Image  string `json:"image,omitempty"`

	// Private records that this repository cannot be cloned without a credential.
	//
	// Not derived from the platform, and not looked up: a public repository needs no token
	// at all, so without this the form has to either nag every project about a credential
	// it may not need, or say nothing and let a private repository be added with no way to
	// clone it. Asking once is the difference between a warning that means something and
	// one that is always on.
	//
	// Advisory. Nothing refuses to build because of it — the clone's own failure is the
	// authority — but the form warns when a private repository has no credential it can
	// reach, which is the moment the answer is cheap to fix.
	Private bool `json:"private,omitempty"`

	// Timeout caps one build of this project, as a Go duration string — "45m", "2h".
	// Empty means the server-wide build.timeout.
	//
	// Per project because the right value is a property of what is being built, not of
	// the host: a single Docusaurus site is done in two minutes and a repository that
	// clones seven others and builds each one is not. Raising the server default to
	// suit the slowest project instead would let every other project hang for the same
	// 45 minutes before anybody was told.
	//
	// A string rather than a duration, because it is stored and redisplayed exactly as
	// it was typed. Parsed at the point of use; an unparseable value is refused when
	// the form is saved, so a build never has to decide what to do with one.
	Timeout string `json:"timeout,omitempty"`

	// Notes is free text for whoever added the project. It appears in the UI and
	// nowhere else; the build never reads it.
	Notes string `json:"notes,omitempty"`

	// DisplayName is what to call this project on screen. Empty means the
	// repository name, which is what it was called before this existed.
	//
	// Separate from the identity rather than replacing it: platform/owner/repo is
	// the primary key and is what a webhook delivery is matched against, so it
	// cannot double as a label somebody wants to rename.
	DisplayName string `json:"display_name,omitempty"`

	// Avatar is a short label rendered as the project's badge — one or two emoji,
	// or a couple of characters. Empty means a monogram derived from the name.
	//
	// Deliberately not a URL. The dashboard is served by a loopback daemon with no
	// outbound dependency, and fetching a remote image would announce every project
	// to whoever hosts it every time somebody opens the page. Two characters
	// identify a row across a list of ten, which is what this is for.
	Avatar string `json:"avatar,omitempty"`

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
            build_output, base_url, detect_script, driver, image, notes,
            display_name, avatar, timeout, private, framework, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
            display_name  = excluded.display_name,
            avatar        = excluded.avatar,
            timeout       = excluded.timeout,
            private       = excluded.private,
            framework     = excluded.framework,
            updated_at    = excluded.updated_at`,
		p.Platform, p.Owner, p.Repo, p.Enabled, p.BuildDir, p.BuildCommand,
		p.BuildOutput, p.BaseURL, p.DetectScript, p.Driver, p.Image, p.Notes,
		p.DisplayName, p.Avatar, p.Timeout, p.Private, p.Framework, created, now)
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
                base_url, detect_script, driver, image, notes, display_name, avatar,
                timeout, private, framework, created_at, updated_at
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
                base_url, detect_script, driver, image, notes, display_name, avatar,
                timeout, private, framework, created_at, updated_at
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
			&p.Image, &p.Notes, &p.DisplayName, &p.Avatar, &p.Timeout, &p.Private,
			&p.Framework, &created, &updated); err != nil {
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

// ClearBuildShare forgets a build's share without touching the rest of its row.
//
// For a build whose artifacts have been pruned: the share is gone, so anything still
// offering its URL is offering a dead link, but the row is history and history did
// happen. Two columns, not a delete.
func (s *Store) ClearBuildShare(ctx context.Context, previewID, buildID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE builds SET name = '', url = '' WHERE preview_id = ? AND build_id = ?`,
		previewID, buildID)
	if err != nil {
		return fmt.Errorf("clearing the share on build %s/%s: %w", previewID, buildID, err)
	}
	return nil
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

// FailPreview empties a preview's URL and records why nothing is serving it.
//
// For the republish that fails at startup. The row is written by a build that succeeded,
// so it says `ready` and carries the URL that build published — and after a failed
// restore nothing is bound to that address, so `/status` offers a link that answers 502
// and the pull request comment advertises the same one. Both read this row, which is why
// emptying it is what fixes both.
//
// An UPDATE rather than a delete, because the artifacts are still on disk and still
// good: the failure is a controller round trip that may work on the next attempt, and a
// deleted row takes the preview off the dashboard — off the page holding the Rebuild
// button — while its comment still points at the dead URL.
//
// `name` is deliberately left alone. Teardown reads it to give the exposer's reserved
// name back (`Daemon.releaseNames`), and on zrok that name is a quota-bearing object, so
// clearing it here would leak one per failed restore with nothing left to name it.
//
// The sibling for a build is ClearBuildShare, which empties both columns and leaves the
// state alone — a build row is history and its state is a fact about what happened,
// where a preview row is a claim about what is being served right now.
func (s *Store) FailPreview(ctx context.Context, previewID, reason string) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE previews SET url = '', state = ?, reason = ?, updated_at = ?
        WHERE preview_id = ?`,
		string(scm.StateFailed), reason, time.Now().UnixMilli(), previewID)
	if err != nil {
		return fmt.Errorf("recording the failed republish of preview %s: %w", previewID, err)
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

// SettingNamePrefix is the key holding the per-installation name prefix.
//
// Named as a constant rather than spelled at each call site, because a typo in a settings
// key is not a compile error — it is a value that silently reads back as the default, which
// looks exactly like a setting that did not save.
const SettingNamePrefix = "exposer.prefix"

// SettingZrokScope is the key holding which zrok environment directory this installation uses:
// "system" for the machine's `~/.zrok2`, or "project" for its own beside the vault.
//
// Stored rather than derived because both can exist and both can be enabled, and the two are
// different zrok accounts. Startup reaps every share it recognises as its own, so a daemon that
// guessed would delete the shares belonging to whatever else uses the other one — see
// internal/expose/zrokenv.go.
const SettingZrokScope = "exposer.zrok.scope"

// SettingExposerKind overrides `exposer.kind` from the config file.
//
// Here rather than only in the config for the same reason the name prefix is: `config.yml` is
// hand-written and its comments are the part that survives being copied to another machine six
// months later, so the daemon does not rewrite it. Which left "switch to Frontdoor" as an
// instruction to edit a file and restart, on a page whose whole purpose is not needing to.
//
// Read once at startup, because the exposer is constructed during wiring and swapping one under a
// running daemon would leave published previews pointing at an exposer that no longer owns them.
// Empty means the config file decides, which is every installation that has never touched this.
const SettingExposerKind = "exposer.kind"

// The ziti exposer's three settings, for the same reason: enrolling an identity from the dashboard
// has to be a complete act, and it is not if it ends in "now edit config.yml and restart".
//
// A stored value wins over the config file, and empty means the file decides.
const (
	SettingZitiIdentityFile = "exposer.ziti.identity_file"
	SettingZitiService      = "exposer.ziti.service"
	SettingZitiDomain       = "exposer.ziti.domain"
)

// Frontdoor's four, on the same terms. None of them is a credential — the API token is in the
// vault — and all four are values an operator copies out of the Frontdoor console, which is a
// browser tab. Asking them to put those in a YAML file and restart, while a browser tab is open on
// this page, is the gap these close.
const (
	SettingFrontdoorAPIBase   = "exposer.frontdoor.api_base"
	SettingFrontdoorFrontend  = "exposer.frontdoor.frontend"
	SettingFrontdoorEnvZID    = "exposer.frontdoor.env_z_id"
	SettingFrontdoorAgentHost = "exposer.frontdoor.agent_reachable_host"
)

// Setting reads one setting, reporting whether it was set at all.
//
// The boolean matters: an operator who deliberately cleared the prefix has set it to the
// empty string, and that is a different answer from never having touched it — the first
// means "no prefix", the second means "use the config file's".
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading setting %s: %w", key, err)
	}
	return v, true, nil
}

// SetSetting writes one setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
        ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("writing setting %s: %w", key, err)
	}
	return nil
}

// ClearSetting removes an override, so the config file's value applies again.
func (s *Store) ClearSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("clearing setting %s: %w", key, err)
	}
	return nil
}

// IgnorePR records that this pull request must not be built again.
//
// Written as part of unlinking, whose other half is tearing the preview down. Idempotent:
// unlinking something already unlinked is not an error, it is the state the operator
// asked for.
func (s *Store) IgnorePR(ctx context.Context, pr model.PullRequest) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO ignored_prs (platform, owner, repo, number, branch, created_at)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT (platform, owner, repo, number) DO UPDATE SET branch = excluded.branch`,
		string(pr.Repo.Platform), pr.Repo.Owner, pr.Repo.Name, pr.Number, pr.Branch,
		time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("ignoring %s: %w", pr.String(), err)
	}
	return nil
}

// UnignorePR undoes IgnorePR, and reports whether there was anything to undo.
//
// The boolean is what tells the caller whether linking a pull request that was ignored
// is a re-link or a first build, which is the difference between two sentences the
// dashboard shows.
func (s *Store) UnignorePR(ctx context.Context, repo model.Repo, number int) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
        DELETE FROM ignored_prs WHERE platform = ? AND owner = ? AND repo = ? AND number = ?`,
		string(repo.Platform), repo.Owner, repo.Name, number)
	if err != nil {
		return false, fmt.Errorf("un-ignoring %s#%d: %w", repo.String(), number, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PRIgnored answers the question every build path asks before doing any work.
func (s *Store) PRIgnored(ctx context.Context, repo model.Repo, number int) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
        SELECT 1 FROM ignored_prs
        WHERE platform = ? AND owner = ? AND repo = ? AND number = ?`,
		string(repo.Platform), repo.Owner, repo.Name, number).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking whether %s#%d is ignored: %w", repo.String(), number, err)
	}
	return true, nil
}

// IgnoredPR is one unlinked pull request, for the list the projects page shows.
type IgnoredPR struct {
	Number    int       `json:"number"`
	Branch    string    `json:"branch,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ListIgnored returns the pull requests unlinked on one repository, lowest number first.
//
// Listed rather than merely enforced, because an ignore nothing displays is
// indistinguishable from a build system that has stopped working. The list is also
// where re-linking is offered.
func (s *Store) ListIgnored(ctx context.Context, repo model.Repo) ([]IgnoredPR, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT number, branch, created_at FROM ignored_prs
        WHERE platform = ? AND owner = ? AND repo = ? ORDER BY number`,
		string(repo.Platform), repo.Owner, repo.Name)
	if err != nil {
		return nil, fmt.Errorf("listing ignored pull requests for %s: %w", repo.String(), err)
	}
	defer rows.Close()

	var out []IgnoredPR
	for rows.Next() {
		var ig IgnoredPR
		var created int64
		if err := rows.Scan(&ig.Number, &ig.Branch, &created); err != nil {
			return nil, fmt.Errorf("scanning an ignored pull request: %w", err)
		}
		ig.CreatedAt = time.UnixMilli(created)
		out = append(out, ig)
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
