package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/netfoundry/docpreview/internal/model"
	"github.com/netfoundry/docpreview/internal/scm"
)

// These tests cover the invariant Mercurius flagged as C1 and C2: when two
// pushes land in quick succession, the second one wins, and the first cannot
// reach into the published state after being superseded.

func TestSupersededBuildIsNotCurrent(t *testing.T) {
	// The check that guards the commit phase. A build that has been superseded
	// must fail it even though its own pointer is perfectly valid.
	_, d, _ := testIngress(t, &fakeClient{})

	id := "preview1"
	first := &build{cancel: func() {}}
	second := &build{cancel: func() {}}

	d.mu.Lock()
	d.running[id] = first
	d.mu.Unlock()

	if !d.isCurrent(id, first) {
		t.Fatal("the only registered build is not current")
	}

	d.mu.Lock()
	d.running[id] = second
	d.mu.Unlock()

	if d.isCurrent(id, first) {
		t.Error("a superseded build still reports itself as current")
	}
	if !d.isCurrent(id, second) {
		t.Error("the replacing build is not current")
	}
}

func TestCancellingClearsTheEntrySoNothingIsCurrent(t *testing.T) {
	// handleBuild cancels and *clears*. Without the clear, a build cancelled
	// while no replacement has been claimed by a worker yet would still see
	// itself as current and publish stale output.
	client := &fakeClient{}
	_, d, _ := testIngress(t, client)

	pr := model.PullRequest{
		Repo:    model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number:  42,
		Branch:  "feature/x",
		HeadSHA: "aaa",
	}
	id := pr.PreviewID()

	cancelled := false
	inFlight := &build{cancel: func() { cancelled = true }}
	d.mu.Lock()
	d.running[id] = inFlight
	d.mu.Unlock()

	newer := pr
	newer.HeadSHA = "bbb"
	if err := d.Handle(context.Background(), scm.Event{Kind: scm.EventBuild, PR: newer}); err != nil {
		t.Fatal(err)
	}

	if !cancelled {
		t.Error("the in-flight build was not cancelled")
	}
	if d.isCurrent(id, inFlight) {
		t.Error("the cancelled build still reports itself as current")
	}
}

func TestLateBuildDoesNotStealTheCurrentCancelHandle(t *testing.T) {
	// C2. An older build finishing late must not delete the newer build's
	// entry, or the next push would find nothing to cancel and the stale-write
	// race reopens.
	_, d, _ := testIngress(t, &fakeClient{})

	id := "preview1"
	older := &build{cancel: func() {}}
	newer := &build{cancel: func() {}}

	d.mu.Lock()
	d.running[id] = older
	d.mu.Unlock()

	// The newer build registers, replacing the older one.
	d.mu.Lock()
	d.running[id] = newer
	d.mu.Unlock()

	// Now the older build's deferred cleanup runs.
	d.mu.Lock()
	if d.running[id] == older {
		delete(d.running, id)
	}
	d.mu.Unlock()

	if !d.isCurrent(id, newer) {
		t.Fatal("a late-finishing older build removed the newer build's cancel handle")
	}
}

func TestCommitLockSerializesPerPreview(t *testing.T) {
	// The commit phase must be mutually exclusive per preview, because
	// publishing a name withdraws whoever currently holds it. Two overlapping
	// commit phases would let the loser's teardown-and-republish interleave
	// with the winner's.
	_, d, _ := testIngress(t, &fakeClient{})

	lock := d.commitLock("preview1")
	if lock != d.commitLock("preview1") {
		t.Fatal("commitLock returned different mutexes for the same preview")
	}
	if lock == d.commitLock("preview2") {
		t.Fatal("commitLock shared one mutex across previews")
	}

	var order []string
	var mu sync.Mutex
	note := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}

	lock.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		l := d.commitLock("preview1")
		l.Lock()
		note("second")
		l.Unlock()
	}()

	// Give the goroutine time to block on the lock.
	time.Sleep(50 * time.Millisecond)
	note("first")
	lock.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the second commit never acquired the lock")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("commit phases overlapped: %v", order)
	}
}

func TestCommitLockSurvivesConcurrentCreation(t *testing.T) {
	// commitLock is called from several goroutines; all must agree on one
	// mutex per preview or the exclusion it provides is imaginary.
	_, d, _ := testIngress(t, &fakeClient{})

	const goroutines = 32
	locks := make([]*sync.Mutex, goroutines)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			locks[i] = d.commitLock("shared")
		}()
	}
	close(start)
	wg.Wait()

	for i, l := range locks {
		if l != locks[0] {
			t.Fatalf("goroutine %d got a different mutex", i)
		}
	}
}

func TestTeardownClearsRunningAndCommitState(t *testing.T) {
	client := &fakeClient{}
	_, d, _ := testIngress(t, client)

	pr := model.PullRequest{
		Repo:   model.Repo{Platform: model.PlatformGitHub, Owner: "acme", Name: "docs"},
		Number: 42, Branch: "feature/x",
	}
	id := pr.PreviewID()

	cancelled := false
	d.mu.Lock()
	d.running[id] = &build{cancel: func() { cancelled = true }}
	d.mu.Unlock()
	_ = d.commitLock(id)

	if err := d.teardown(context.Background(), pr, id); err != nil {
		t.Fatal(err)
	}

	if !cancelled {
		t.Error("teardown did not cancel the in-flight build")
	}

	d.mu.Lock()
	_, running := d.running[id]
	_, commit := d.commit[id]
	d.mu.Unlock()

	if running {
		t.Error("teardown left a running entry behind")
	}
	if commit {
		t.Error("teardown leaked the commit lock; the map would grow without bound")
	}
}
