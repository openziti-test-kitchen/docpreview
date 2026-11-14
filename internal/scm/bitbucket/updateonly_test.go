package bitbucket

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// commentAPI answers the comment endpoints and counts the writes.
type commentAPI struct {
	mu     sync.Mutex
	bodies map[int64]string
	posts  int
	puts   int
}

func (a *commentAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		type comment struct {
			ID      int64             `json:"id"`
			Content map[string]string `json:"content"`
		}
		values := []comment{}
		for id, body := range a.bodies {
			values = append(values, comment{ID: id, Content: map[string]string{"raw": body}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"values": values})
	case http.MethodPost:
		a.posts++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 501}`))
	case http.MethodPut:
		a.puts++
		_, _ = w.Write([]byte(`{"id": 77}`))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func skipReport(updateOnly bool) scm.Report {
	return scm.Report{
		PR: model.PullRequest{
			Repo:   model.Repo{Platform: model.PlatformBitbucket, Owner: "acme", Name: "docs"},
			Number: 42,
		},
		PreviewID: "abc123", State: scm.StateSkipped,
		Reason: "None of the 3 changed files matched a documentation path.", UpdateOnly: updateOnly,
	}
}

// A new comment notifies everybody on the pull request. A report marked UpdateOnly has
// nothing worth that, so on a pull request with no comment it writes none.
func TestUpdateOnlyReportPostsNoNewComment(t *testing.T) {
	api := &commentAPI{}
	c, _ := newTestClient(t, api)

	if err := c.upsertComment(context.Background(), skipReport(true)); err != nil {
		t.Fatal(err)
	}
	if api.posts != 0 || api.puts != 0 {
		t.Errorf("posts = %d, puts = %d; want no write at all", api.posts, api.puts)
	}
}

// A comment an earlier commit earned is still edited, so it never shows a state that has passed.
func TestUpdateOnlyReportEditsTheCommentThatExists(t *testing.T) {
	api := &commentAPI{bodies: map[int64]string{77: scm.Marker("abc123") + "\n\n**Documentation preview**\n"}}
	c, _ := newTestClient(t, api)

	if err := c.upsertComment(context.Background(), skipReport(true)); err != nil {
		t.Fatal(err)
	}
	if api.posts != 0 || api.puts != 1 {
		t.Errorf("posts = %d, puts = %d; want one edit and no new comment", api.posts, api.puts)
	}
}

// And the ordinary report still creates the comment.
func TestOrdinaryReportStillPostsTheComment(t *testing.T) {
	api := &commentAPI{}
	c, _ := newTestClient(t, api)

	if err := c.upsertComment(context.Background(), skipReport(false)); err != nil {
		t.Fatal(err)
	}
	if api.posts != 1 {
		t.Errorf("posts = %d, want 1", api.posts)
	}
}
