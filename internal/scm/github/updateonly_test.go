package github

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/netfoundry/docpreview/internal/scm"
)

// commentAPI answers the comment endpoints and counts the writes.
type commentAPI struct {
	mu      sync.Mutex
	posts   int
	patches int
}

func (a *commentAPI) serve(f *apiFixture, bodies map[int64]string) {
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			type comment struct {
				ID   int64  `json:"id"`
				Body string `json:"body"`
			}
			out := []comment{}
			if r.URL.Query().Get("page") == "1" {
				for id, body := range bodies {
					out = append(out, comment{ID: id, Body: body})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			a.posts++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 501}`))
		case http.MethodPatch:
			a.patches++
			if !strings.HasSuffix(r.URL.Path, "/issues/comments/77") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"id": 77}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func skipReport(updateOnly bool) scm.Report {
	return scm.Report{
		PR: testPR(), PreviewID: "abc123", State: scm.StateSkipped,
		Reason: "None of the 3 changed files matched a documentation path.", UpdateOnly: updateOnly,
	}
}

// A new comment notifies everybody on the pull request. A report marked UpdateOnly has
// nothing worth that, so on a pull request with no comment it writes none.
func TestUpdateOnlyReportPostsNoNewComment(t *testing.T) {
	f := newAPIFixture(t)
	api := &commentAPI{}
	api.serve(f, nil)

	if err := f.client.upsertComment(context.Background(), skipReport(true)); err != nil {
		t.Fatal(err)
	}
	if api.posts != 0 || api.patches != 0 {
		t.Errorf("posts = %d, patches = %d; want no write at all", api.posts, api.patches)
	}
}

// A comment an earlier commit earned is still edited, so it never shows a state that has passed.
func TestUpdateOnlyReportEditsTheCommentThatExists(t *testing.T) {
	f := newAPIFixture(t)
	api := &commentAPI{}
	api.serve(f, map[int64]string{77: scm.Marker("abc123") + "\n\n**Documentation preview**\n"})

	if err := f.client.upsertComment(context.Background(), skipReport(true)); err != nil {
		t.Fatal(err)
	}
	if api.posts != 0 || api.patches != 1 {
		t.Errorf("posts = %d, patches = %d; want one edit and no new comment", api.posts, api.patches)
	}
}

// A comment somebody deleted is not brought back by a report that may only edit.
func TestUpdateOnlyReportDoesNotRepostADeletedComment(t *testing.T) {
	f := newAPIFixture(t)
	api := &commentAPI{}
	api.serve(f, nil)
	f.client.rememberComment("abc123", 12345)

	if err := f.client.upsertComment(context.Background(), skipReport(true)); err != nil {
		t.Fatal(err)
	}
	if api.posts != 0 {
		t.Errorf("posts = %d; a deleted comment was posted again", api.posts)
	}
}

// And the ordinary report still creates the comment.
func TestOrdinaryReportStillPostsTheComment(t *testing.T) {
	f := newAPIFixture(t)
	api := &commentAPI{}
	api.serve(f, nil)

	if err := f.client.upsertComment(context.Background(), skipReport(false)); err != nil {
		t.Fatal(err)
	}
	if api.posts != 1 {
		t.Errorf("posts = %d, want 1", api.posts)
	}
}
