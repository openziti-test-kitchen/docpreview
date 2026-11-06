package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
	"github.com/netfoundry/docpreview/internal/store"
)

// A skip replaces a failed row with nothing published, and leaves a published one alone.
//
// The failed case is docusaurus-shared #150: its build failed on a zrok 429, a Rebuild found its
// head touched no documentation and skipped, and the card went on showing the day-old 429.
func TestRecordSkipReplacesOnlyAnUnpublishedRow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{store: st, log: slog.New(slog.DiscardHandler)}
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		prior store.Preview
		want  scm.State
	}{
		{"failed, nothing published", store.Preview{State: scm.StateFailed, Reason: "status 429"}, scm.StateSkipped},
		{"ready", store.Preview{State: scm.StateReady, URL: "https://x.example/"}, scm.StateReady},
		{"failed rebuild of a live preview", store.Preview{State: scm.StateFailed, URL: "https://x.example/"},
			scm.StateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := model.PullRequest{
				Repo:   model.Repo{Platform: "github", Owner: "o", Name: tc.name},
				Number: 150, HeadSHA: "6a6e06c",
			}
			tc.prior.PreviewID, tc.prior.PR, tc.prior.UpdatedAt = pr.PreviewID(), pr, time.Now()
			if err := st.SavePreview(ctx, tc.prior); err != nil {
				t.Fatal(err)
			}

			d.recordSkip(ctx, pr, "None of the 1 changed files matched a documentation path.")

			all, err := st.ListPreviews(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range all {
				if p.PreviewID == pr.PreviewID() && p.State != tc.want {
					t.Errorf("state %q after the skip, want %q", p.State, tc.want)
				}
			}
		})
	}
}
