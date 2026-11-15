package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/pipeline"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// A failure replaces an older row with nothing published, and leaves a published one alone.
//
// The replaced case is frontdoor's branch preview: its card kept the first failure's commit and
// reason while every later retry failed at a newer commit for a different reason.
func TestRecordFailureReplacesOnlyAnUnpublishedRow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{store: st, log: slog.New(slog.DiscardHandler), builder: &pipeline.Builder{}}
	ctx := context.Background()

	const newReason = `no package.json in "docusaurus"`
	for _, tc := range []struct {
		name       string
		prior      store.Preview
		wantState  scm.State
		wantCommit string
		wantReason string
	}{
		{"older failure", store.Preview{State: scm.StateFailed, Commit: "942cfd8", Reason: "old reason"},
			scm.StateFailed, "bd20358", newReason},
		{"skipped", store.Preview{State: scm.StateSkipped, Commit: "942cfd8", Reason: "no docs changed"},
			scm.StateFailed, "bd20358", newReason},
		{"ready", store.Preview{State: scm.StateReady, Commit: "942cfd8", URL: "https://x.example/"},
			scm.StateReady, "942cfd8", ""},
		{"failed rebuild of a live preview",
			store.Preview{State: scm.StateFailed, Commit: "942cfd8", URL: "https://x.example/", Reason: "old reason"},
			scm.StateFailed, "942cfd8", "old reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := model.PullRequest{
				Repo:    model.Repo{Platform: "bitbucket", Owner: "o", Name: tc.name},
				Branch:  "develop",
				HeadSHA: "bd20358",
			}
			tc.prior.PreviewID, tc.prior.PR, tc.prior.UpdatedAt = pr.PreviewID(), pr, time.Now()
			if err := st.SavePreview(ctx, tc.prior); err != nil {
				t.Fatal(err)
			}

			d.recordFailure(ctx, pr, errors.New(newReason+"\nmore detail"))

			all, err := st.ListPreviews(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range all {
				if p.PreviewID != pr.PreviewID() {
					continue
				}
				found = true
				if p.State != tc.wantState || p.Commit != tc.wantCommit || p.Reason != tc.wantReason {
					t.Errorf("got state %q commit %q reason %q, want %q %q %q",
						p.State, p.Commit, p.Reason, tc.wantState, tc.wantCommit, tc.wantReason)
				}
			}
			if !found {
				t.Fatal("no row for the preview")
			}
		})
	}
}
