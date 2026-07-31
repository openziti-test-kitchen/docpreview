package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/expose"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// `docpreview shares list` exists to separate three answers that look the same in a
// flat listing: a share doing its job, a share nothing claims, and a claim with no
// share. These tests pin the separation, because collapsing any two of them back
// together is the failure that makes the command worthless rather than broken —
// nothing crashes, the counts just stop meaning anything.

func sharesTestPR(number int) model.PullRequest {
	return model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "netfoundry", Name: "docpreview"},
		Number: number,
		Branch: "round-5",
	}
}

// rowFor finds one row by key, so a test asserts about the row it means rather than
// about a position in a sorted slice.
func rowFor(t *testing.T, rows []shareRow, key string) shareRow {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no row for %q in %+v", key, rows)
	return shareRow{}
}

// TestAuditSeparatesOrphansFromMissingShares. The two are opposite problems with
// opposite fixes — delete the one, republish the other — so a report that called
// both "mismatch" would send the operator the wrong way half the time.
func TestAuditSeparatesOrphansFromMissingShares(t *testing.T) {
	held := map[string]expose.Adoptable{
		"aaaaaaaaaaaa": {Handle: "tok-live", Origin: "https://live.share.zrok.io"},
		"orphanorphan": {Handle: "tok-orphan", Origin: "https://orphan.share.zrok.io"},
	}
	recorded := []recordedShare{
		{key: "aaaaaaaaaaaa", pr: "github:netfoundry/docpreview#1", url: "https://live.share.zrok.io/", published: true},
		{key: "bbbbbbbbbbbb", pr: "github:netfoundry/docpreview#2", url: "https://gone.share.zrok.io/", published: true},
	}

	rows, tally := auditShares(held, recorded)

	if got, want := (shareTally{Held: 2, Matched: 1, Orphaned: 1, Missing: 1}), tally; got != want {
		t.Fatalf("tally = %+v, want %+v", got, want)
	}
	if got := rowFor(t, rows, "orphanorphan"); got.State != stateOrphan || got.Share != "tok-orphan" {
		t.Fatalf("orphan row = %+v", got)
	}
	// The share token is what the operator would take to zrok to look at the thing,
	// and the database has no name for it — so an orphan row that lost the handle
	// names a problem nobody can then go and inspect.
	if got := rowFor(t, rows, "bbbbbbbbbbbb"); got.State != stateMissing || got.Share != "" {
		t.Fatalf("missing row = %+v", got)
	}
	if got := rowFor(t, rows, "aaaaaaaaaaaa"); got.State != stateOK {
		t.Fatalf("matched row = %+v", got)
	}
}

// TestAuditMustNotReportAnUnpublishedPreviewAsMissing. A preview whose first build
// has not finished has no share and never had one. Counting it as missing would put
// a permanent false alarm on every fresh project, and an audit that cries wolf is an
// audit nobody reads.
func TestAuditMustNotReportAnUnpublishedPreviewAsMissing(t *testing.T) {
	recorded := []recordedShare{
		{key: "cccccccccccc", pr: "github:netfoundry/docpreview#3", published: false},
	}

	rows, tally := auditShares(nil, recorded)

	if tally.Missing != 0 || tally.Unpublished != 1 {
		t.Fatalf("tally = %+v, want 0 missing and 1 unpublished", tally)
	}
	if got := rowFor(t, rows, "cccccccccccc"); got.State != stateNever {
		t.Fatalf("row = %+v, want state %q", got, stateNever)
	}
}

// TestAuditKeysBuildSharesTheWayTheDaemonDoes. A build share's key is
// "<preview>/<build>", and if this side of the comparison spelled it any other way
// every per-build share on the account would read as an orphan — pointing the
// operator at a leak that is not there while hiding any real one in the noise.
func TestAuditKeysBuildSharesTheWayTheDaemonDoes(t *testing.T) {
	key := expose.Spec{PreviewID: "19344c5ee369", BuildID: "20260729-190307-85912e2"}.Key()
	held := map[string]expose.Adoptable{
		key: {Handle: "tok-build", Origin: "https://c85912e2.share.zrok.io"},
	}
	recorded := []recordedShare{{key: key, pr: "github:netfoundry/docpreview#4",
		url: "https://c85912e2.share.zrok.io/", published: true}}

	rows, tally := auditShares(held, recorded)

	if tally.Orphaned != 0 || tally.Matched != 1 {
		t.Fatalf("tally = %+v, want the build share matched", tally)
	}
	if got := rowFor(t, rows, key); got.State != stateOK {
		t.Fatalf("row = %+v", got)
	}
}

// TestAuditPutsProblemsFirst. The table can run to dozens of rows and the reason
// somebody ran the command is in the problem rows, so they go at the top.
func TestAuditPutsProblemsFirst(t *testing.T) {
	held := map[string]expose.Adoptable{
		"aaaaaaaaaaaa": {Handle: "tok-ok", Origin: "https://a.share.zrok.io"},
		"zzzzzzzzzzzz": {Handle: "tok-orphan", Origin: "https://z.share.zrok.io"},
	}
	recorded := []recordedShare{
		{key: "aaaaaaaaaaaa", pr: "pr#1", url: "https://a.share.zrok.io/", published: true},
		{key: "mmmmmmmmmmmm", pr: "pr#2", published: false},
		{key: "nnnnnnnnnnnn", pr: "pr#3", url: "https://n.share.zrok.io/", published: true},
	}

	rows, _ := auditShares(held, recorded)

	var states []string
	for _, r := range rows {
		states = append(states, r.State)
	}
	want := []string{stateOrphan, stateMissing, stateOK, stateNever}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Fatalf("states = %v, want %v", states, want)
	}
}

// TestRecordedSharesCountsBuildSharesAsWell. Every publication is a share plus a
// reserved name on the account, so a flattening that only walked previews would
// report each build share as an orphan.
func TestRecordedSharesCountsBuildSharesAsWell(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "docpreview.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	pr := sharesTestPR(41)
	id := pr.PreviewID()

	if err := st.SavePreview(ctx, store.Preview{
		PreviewID: id, PR: pr, Name: "docpreview-round-5",
		URL: "https://docpreview-round-5.share.zrok.io/", State: scm.StateReady,
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// One build with a share of its own, one without. Only the first is a
	// publication; the second is history.
	if err := st.SaveBuild(ctx, store.Build{
		PreviewID: id, BuildID: "20260729-190307-85912e2", PR: pr, State: "ready",
		Name: "c85912e2", URL: "https://c85912e2.share.zrok.io/", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBuild(ctx, store.Build{
		PreviewID: id, BuildID: "20260729-180000-0000000", PR: pr, State: "failed",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	recorded, err := recordedShares(ctx, st)
	if err != nil {
		t.Fatal(err)
	}

	if len(recorded) != 2 {
		t.Fatalf("recorded %d publications, want the preview and its one build share: %+v",
			len(recorded), recorded)
	}
	byKey := map[string]recordedShare{}
	for _, r := range recorded {
		byKey[r.key] = r
	}
	if _, ok := byKey[id]; !ok {
		t.Fatalf("no entry for the preview itself: %+v", recorded)
	}
	buildKey := expose.Spec{PreviewID: id, BuildID: "20260729-190307-85912e2"}.Key()
	if got, ok := byKey[buildKey]; !ok || !got.published {
		t.Fatalf("no published entry for the build share %q: %+v", buildKey, recorded)
	}
}

// TestSharesTableNamesTheCounts. The summary line is the answer to "did something
// leak"; the table is the evidence. A table with no summary makes the reader count
// rows, which is the job being delegated.
func TestSharesTableNamesTheCounts(t *testing.T) {
	rows := []shareRow{
		{State: stateOrphan, Key: "zzzzzzzzzzzz", Share: "tok-orphan", URL: "https://z.share.zrok.io/"},
		{State: stateOK, Key: "aaaaaaaaaaaa", PR: "github:netfoundry/docpreview#1",
			Share: "tok-ok", URL: "https://a.share.zrok.io/"},
	}
	var sb strings.Builder
	writeSharesTable(&sb, rows, shareTally{Held: 2, Matched: 1, Orphaned: 1})
	out := sb.String()

	for _, want := range []string{"PUBLICATION", "orphan", "tok-orphan",
		"2 shares held", "1 orphaned"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not mention %q:\n%s", want, out)
		}
	}
	// Guidance for a problem that was found, and none for one that was not.
	if !strings.Contains(out, "Reap") {
		t.Fatalf("an orphan was reported without saying what deletes it:\n%s", out)
	}
	if strings.Contains(out, "404s") {
		t.Fatalf("advice about missing shares on a report with none:\n%s", out)
	}
}

// TestSharesJSONIsAnArrayOnAnEmptyAccount. `null` where a consumer expects a list is
// how a monitoring check written against the happy path fails on the quiet day.
func TestSharesJSONIsAnArrayOnAnEmptyAccount(t *testing.T) {
	var sb strings.Builder
	if err := writeSharesJSON(&sb, "zrok2", nil, shareTally{}); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Exposer string     `json:"exposer"`
		Counts  shareTally `json:"counts"`
		Shares  []shareRow `json:"shares"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, sb.String())
	}
	if got.Exposer != "zrok2" {
		t.Fatalf("exposer = %q", got.Exposer)
	}
	if !strings.Contains(sb.String(), `"shares": []`) {
		t.Fatalf("shares is not an empty array:\n%s", sb.String())
	}
}
