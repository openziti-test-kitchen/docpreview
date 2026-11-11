package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/store"
)

// TestADroppedRequestDoesNotCancelABranchBuild.
//
// Production, 2026-11-03: Build it logged "reading branch main ... context canceled" and
// queued nothing, because the browser dropped the request and the Bitbucket call was made on
// r.Context(). The stub here stands in for that call: it waits until the request is already
// cancelled and then checks whether its own context went with it.
func TestADroppedRequestDoesNotCancelABranchBuild(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}

	reqCtx, dropRequest := context.WithCancel(context.Background())
	var workErr error
	var deadline bool
	h := NewProjectsAdmin(st, cfg, slog.New(slog.DiscardHandler)).
		WithBrancher(func(ctx context.Context, repo model.Repo, branch string) (string, error) {
			dropRequest() // the browser goes away mid-call
			<-reqCtx.Done()
			workErr = ctx.Err()
			_, deadline = ctx.Deadline()
			return "main", nil
		}).Handler()

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/projects/bitbucket/netfoundry/k8s-on-prem-installations/branch", nil).
		WithContext(reqCtx)
	h.ServeHTTP(rec, fromCaller(r, localCaller))

	if workErr != nil {
		t.Fatalf("the branch build's context was cancelled with the request: %v", workErr)
	}
	if !deadline {
		t.Error("the detached context has no deadline; a hung platform call would never end")
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["queued"] != true || body["branch"] != "main" {
		t.Errorf("answer = %d %s, want 200 queued:true branch:main", rec.Code, rec.Body)
	}
}

// TestQueueContextKeepsTheRequestsValues. The role rides on the request context, and the work
// queued from a button must still see who pressed it.
func TestQueueContextKeepsTheRequestsValues(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "admin"))
	r := httptest.NewRequest("POST", "/", nil).WithContext(parent)
	ctx, done := queueContext(r)
	defer done()
	cancel()
	if ctx.Err() != nil {
		t.Error("cancelling the request cancelled the queued work")
	}
	if ctx.Value(key{}) != "admin" {
		t.Error("the request's values did not survive the detach")
	}
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > queueBudget {
		t.Errorf("deadline = %v %v", d, ok)
	}
}

// TestAPartialScanReportsWhatItQueued. ScanRepo carries on past a pull request whose enqueue
// failed, so the answer is "queued N, and this one failed" — reporting zero beside the error
// would send somebody to rebuild previews that are already building.
func TestAPartialScanReportsWhatItQueued(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.Listeners = []config.Listener{{TCP: "127.0.0.1:8471"}}

	h := NewProjectsAdmin(st, cfg, slog.New(slog.DiscardHandler)).
		WithScanner(func(context.Context, model.Repo) (int, error) {
			return 2, errors.New("pull request 14: the queue is closed")
		}).Handler()
	rec := secretCall(t, h, "POST", "/api/projects/bitbucket/acme/docs/scan", "", localCaller)

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["queued"] != float64(2) ||
		body["error"] != "pull request 14: the queue is closed" {
		t.Errorf("answer = %d %s, want 200 queued:2 with the error", rec.Code, rec.Body)
	}
}
