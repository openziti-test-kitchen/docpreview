package expose

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The retry helper is exported so `webhook-only` and `dashboard-only` share this exact
// judgement rather than carrying a second copy of it. These tests are on the exported name for
// that reason: they are the contract those two commands depend on.

// A single controller timeout must not be fatal.
//
// For the two tunnel commands the consequence of treating it as fatal is worse than a failed
// start: the zrok share record outlives the process, so the frontend answers 502 for a backend
// that is gone, with nothing on the machine indicating which of the three processes died.
func TestOneTimeoutIsRetriedRatherThanReturned(t *testing.T) {
	restore := fastBackoff(t)
	defer restore()

	calls := 0
	err := RetryZrok(context.Background(), nil, "create share", func() error {
		calls++
		if calls == 1 {
			return errors.New(`Post "https://api-v2.zrok.io/api/v2/share": context deadline exceeded`)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RetryZrok returned %v after a retryable failure", err)
	}
	if calls != 2 {
		t.Errorf("the call was made %d times, want 2", calls)
	}
}

// A refusal must not be retried. A permission failure or a quota refusal asked three times is
// three times the same answer, and the delay only makes the real error slower to see.
func TestAnUnrecognisedErrorIsNotRetried(t *testing.T) {
	restore := fastBackoff(t)
	defer restore()

	calls := 0
	want := errors.New("[401] unauthorized")
	err := RetryZrok(context.Background(), nil, "create share", func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Errorf("RetryZrok returned %v, want the original error", err)
	}
	if calls != 1 {
		t.Errorf("the call was made %d times, want 1", calls)
	}
}

// Retries are bounded. A controller that is down stays down, and a process that waits forever
// for it is indistinguishable from one that is hung.
func TestRetriesAreBounded(t *testing.T) {
	restore := fastBackoff(t)
	defer restore()

	calls := 0
	err := RetryZrok(context.Background(), nil, "create share", func() error {
		calls++
		return errors.New("i/o timeout")
	})
	if err == nil {
		t.Fatal("RetryZrok succeeded while every attempt failed")
	}
	if calls != len(zrokBackoff)+1 {
		t.Errorf("the call was made %d times, want %d", calls, len(zrokBackoff)+1)
	}
}

// A cancelled context ends the wait, so a shutdown does not sit through the backoff.
func TestCancellationEndsTheWaitEarly(t *testing.T) {
	restore := fastBackoff(t)
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	err := RetryZrok(ctx, nil, "create share", func() error {
		calls++
		return errors.New("TLS handshake timeout")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RetryZrok returned %v, want the context error joined in", err)
	}
	if calls != 1 {
		t.Errorf("the call was made %d times, want 1 — the backoff was not skipped", calls)
	}
}

// The exported test and the unexported one the Zrok methods use must stay the same test. Two
// definitions of "worth retrying" is the thing exporting it was meant to prevent.
func TestTheExportedAndInternalTestsAgree(t *testing.T) {
	for _, err := range []error{
		nil,
		io.ErrUnexpectedEOF,
		errors.New("context deadline exceeded"),
		errors.New("[404] shareNotFound"),
	} {
		if got, want := transient(err), TransientZrok(err); got != want {
			t.Errorf("transient(%v) = %v but TransientZrok = %v", err, got, want)
		}
	}
}

// fastBackoff shrinks the waits for the duration of a test. The real gaps are seconds, which is
// right in production and would make this file take twenty of them.
func fastBackoff(t *testing.T) func() {
	t.Helper()
	saved := zrokBackoff
	savedLimit := zrokRateLimitBackoff
	zrokBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	zrokRateLimitBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	return func() {
		zrokBackoff = saved
		zrokRateLimitBackoff = savedLimit
	}
}

// A rate limit is retried, on its own schedule, and given more attempts than a timeout.
//
// The controller refuses state changes in a window, so retrying inside that window is refused
// again — and the timeout schedule is short enough to spend every attempt inside one. A publish
// that gives up here is a build that succeeded and a preview with no URL.
func TestARateLimitIsRetriedOnTheLongerSchedule(t *testing.T) {
	defer fastBackoff(t)()

	// The shape the controller actually returns when opening the listener: the rate limit arrives
	// wrapped in an authentication failure, because the SDK could not get a session to do the work.
	limited := errors.New("opening zrok listener for share a2w9t3o1f8ed: error creating listener: " +
		"failed to listen: no apiSession, authentication attempt failed: error for request dS1X1evcI: " +
		"SERVER_TOO_MANY_REQUESTS: Too many requests to alter state have been issued. " +
		"Please slow your request rate or try again later.")

	if !RateLimitedZrok(limited) {
		t.Fatal("the controller's rate-limit refusal was not recognised as one")
	}
	if !TransientZrok(limited) {
		t.Fatal("a rate limit must be retried, so it has to count as transient")
	}

	calls := 0
	err := RetryZrok(context.Background(), nil, "publish", func() error {
		calls++
		// Refused for longer than the timeout schedule allows, which is the whole point: three
		// attempts on the short schedule would all land inside one window.
		if calls <= 3 {
			return limited
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RetryZrok returned %v after a rate limit that cleared", err)
	}
	if calls != 4 {
		t.Errorf("the call was made %d times, want 4 — one attempt plus three waits", calls)
	}
}

// Concurrent state-altering calls are spaced out rather than issued together.
//
// Retrying after a refusal is the cure; this is the prevention. The controller limits how fast
// state changes are asked for, so publishing three previews at once — three workers finishing
// together, or a restart republishing everything — is what provokes the refusal in the first
// place.
//
// Asserted on the gaps between calls rather than on total elapsed time, because a total says
// nothing about whether the calls queued or all slept and then fired at once. The second is what
// a naive pacer does, and it is indistinguishable from no pacer at the controller.
func TestStateChangesAreSpacedOutRatherThanBursted(t *testing.T) {
	saved := zrokLastCall
	defer func() { zrokLastCall = saved }()
	zrokLastCall = time.Time{}

	const calls = 4
	var mu sync.Mutex
	var at []time.Time

	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = RetryZrok(context.Background(), nil, "publish", func() error {
				mu.Lock()
				at = append(at, time.Now())
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	if len(at) != calls {
		t.Fatalf("%d calls ran, want %d", len(at), calls)
	}
	sort.Slice(at, func(i, j int) bool { return at[i].Before(at[j]) })

	// A little slack for scheduling: the assertion is that they did not arrive together, not that
	// the interval is exact.
	min := zrokMinInterval - zrokMinInterval/5
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < min {
			t.Errorf("calls %d and %d were %s apart, want at least %s — they arrived as a burst",
				i-1, i, gap, min)
		}
	}
}

// A wait is narrated where the person waiting is looking.
//
// A rate-limited publish sits for up to two and a half minutes after the build has finished, and
// the build log is what somebody is watching while it does. With nothing written there the pane
// ends on "$ build finished" and a wait is indistinguishable from a hang — the daemon's own log,
// on the host, is the only place that says otherwise.
func TestABackoffIsNarratedToTheProgressWriter(t *testing.T) {
	defer fastBackoff(t)()

	var progress strings.Builder
	calls := 0
	err := retryZrokTo(context.Background(), nil, &progress, "create share a-preview", func() error {
		calls++
		if calls == 1 {
			return errors.New("SERVER_TOO_MANY_REQUESTS: Too many requests to alter state")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryZrokTo returned %v", err)
	}

	got := progress.String()
	for _, want := range []string{"rate limiting", "create share a-preview", "retrying in"} {
		if !strings.Contains(got, want) {
			t.Errorf("the narration does not mention %q:\n%s", want, got)
		}
	}
}

// A publish with nowhere to narrate to must still publish. Every caller outside a build — startup
// recovery, the teardown sweep, the two tunnel commands — passes no writer.
func TestANilProgressWriterIsNotAFailure(t *testing.T) {
	defer fastBackoff(t)()

	Spec{}.say("this goes nowhere and must not panic")

	calls := 0
	err := retryZrokTo(context.Background(), nil, nil, "create share", func() error {
		calls++
		if calls == 1 {
			return errors.New("context deadline exceeded")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryZrokTo returned %v with no progress writer", err)
	}
}

// The schedules are not the same length, and the rate-limit one is the longer wait.
//
// Asserted rather than left to a reader comparing two var blocks: the reason a rate limit has its
// own schedule is that the controller's refusal outlasts a timeout's, and a change that made them
// equal would silently reinstate the failure this exists to fix.
func TestTheRateLimitBackoffIsLongerThanTheTimeoutBackoff(t *testing.T) {
	if len(zrokRateLimitBackoff) <= len(zrokBackoff) {
		t.Errorf("the rate-limit schedule has %d attempts and the timeout schedule has %d; "+
			"a rate limit needs more", len(zrokRateLimitBackoff), len(zrokBackoff))
	}
	var limit, timeout time.Duration
	for _, d := range zrokRateLimitBackoff {
		limit += d
	}
	for _, d := range zrokBackoff {
		timeout += d
	}
	if limit <= timeout {
		t.Errorf("the rate-limit schedule waits %s in total and the timeout schedule waits %s; "+
			"the controller's refusal outlasts a timeout", limit, timeout)
	}
}
