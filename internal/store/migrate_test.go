package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
)

// TestMigrateAddsColumnsToAnExistingDatabase is the case the schema cannot cover.
//
// `CREATE TABLE IF NOT EXISTS` does nothing to a table that already exists, so a
// column added to the schema reaches new databases only. Every database created
// before the change silently lacks it, and the first query naming it fails at
// runtime on the one machine that has real data.
func TestMigrateAddsColumnsToAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// A builds table as it was before name and url existed.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
        CREATE TABLE builds (
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
            PRIMARY KEY (preview_id, build_id)
        )`); err != nil {
		t.Fatal(err)
	}
	// A row written by the older version, which the migration must not disturb.
	if _, err := db.Exec(`INSERT INTO builds (preview_id, build_id, state, started_at)
        VALUES ('19344c5ee369', '20260729-190307-85912e2', 'ready', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening a database from before the columns existed: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Reading must work, and the pre-existing row must still be there with empty
	// values for the new columns rather than a scan error.
	builds, err := s.RecentBuilds(context.Background(), 10)
	if err != nil {
		t.Fatalf("reading builds after migration: %v", err)
	}
	if len(builds) != 1 {
		t.Fatalf("got %d builds, want the one that was already there", len(builds))
	}
	if builds[0].Name != "" || builds[0].URL != "" {
		t.Errorf("an old row came back with name=%q url=%q, want both empty",
			builds[0].Name, builds[0].URL)
	}

	// And opening again must not fail on "duplicate column name", which is what
	// makes the migration idempotent rather than a one-shot.
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("second open failed, so the migration is not idempotent: %v", err)
	}
	again.Close()
}

// TestSaveBuildKeepsAShareAcrossTheSecondWrite — a build row is written twice, at
// start and at finish. The first write has no share yet, so the second must not
// erase what the publish recorded if they ever arrive out of order.
func TestSaveBuildKeepsAShareAcrossTheSecondWrite(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	b := Build{
		PreviewID: "19344c5ee369",
		BuildID:   "20260729-190307-85912e2",
		PR: model.PullRequest{
			Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
			Number: 7, Branch: "add-guide",
		},
		State:     "ready",
		StartedAt: time.Now(),
		Name:      "add-guide-85912e2",
		URL:       "https://add-guide-85912e2.example/",
	}
	if err := s.SaveBuild(ctx, b); err != nil {
		t.Fatal(err)
	}

	// The shape of the start-of-build write: same key, no share.
	b.Name, b.URL = "", ""
	b.State = "building"
	if err := s.SaveBuild(ctx, b); err != nil {
		t.Fatal(err)
	}

	got, err := s.BuildsFor(ctx, "19344c5ee369")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].URL != "https://add-guide-85912e2.example/" {
		t.Errorf("url = %q, want the recorded share — a share nothing remembers gets reaped",
			got[0].URL)
	}
	if got[0].State != "building" {
		t.Errorf("state = %q, want the second write's value", got[0].State)
	}
}
