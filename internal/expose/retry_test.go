package expose

import (
	"context"
	"errors"
	"io"
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
	zrokBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	return func() { zrokBackoff = saved }
}
