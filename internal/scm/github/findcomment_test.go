package github

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

func testPR() model.PullRequest {
	return model.PullRequest{
		Repo:           model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:         42,
		InstallationID: 999,
	}
}

// serveComments answers the list-comments endpoint with one page of bodies.
func (f *apiFixture) serveComments(bodies map[int64]string) {
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			// Page two is empty, which is how findComment learns to stop.
			_, _ = w.Write([]byte(`[]`))
			return
		}
		type comment struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		out := make([]comment, 0, len(bodies))
		for id, body := range bodies {
			out = append(out, comment{ID: id, Body: body})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// TestFindCommentMatchesEitherMarkerStyle is the upgrade path, and the reason
// HasMarker exists rather than a comparison against one rendered marker.
//
// A daemon that recognised only the style it currently writes would decide there was
// no comment, post a second one, and do it on every open pull request at once — the
// exact failure the marker exists to prevent. The comments are on GitHub's servers and
// outlive every release of this program, so both styles have to be found forever.
func TestFindCommentMatchesEitherMarkerStyle(t *testing.T) {
	for _, tc := range []struct {
		style scm.MarkerStyle
		name  string
	}{
		{scm.MarkerHTMLComment, "html comment"},
		{scm.MarkerLinkRef, "link reference definition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			f.serveComments(map[int64]string{
				77: scm.MarkerFor("abc123", tc.style) + "\n\n**Documentation preview**\n",
			})

			got, err := f.client.findComment(context.Background(), testPR(), "abc123")
			if err != nil {
				t.Fatal(err)
			}
			if got != 77 {
				t.Errorf("findComment = %d, want 77; a missed comment means a duplicate", got)
			}
		})
	}
}

// TestFindCommentIgnoresOtherComments — the marker carries a preview id so that two
// docpreview instances watching one repository, staging and production, do not fight
// over a single comment. A reviewer quoting the marker in a discussion must not be
// mistaken for it either.
func TestFindCommentIgnoresOtherComments(t *testing.T) {
	f := newAPIFixture(t)
	f.serveComments(map[int64]string{
		1: "LGTM",
		2: scm.Marker("def456") + "\n\nanother instance's preview\n",
		3: "the marker is `" + scm.MarkerFor("def456", scm.MarkerLinkRef) + "`, in case that helps",
	})

	got, err := f.client.findComment(context.Background(), testPR(), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("findComment = %d, want 0; it would edit a comment that is not ours", got)
	}
}
