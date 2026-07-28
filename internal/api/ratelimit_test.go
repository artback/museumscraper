package api

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsBurstThenLimits(t *testing.T) {
	limiter := newRateLimiter(10, 5, time.Minute)
	clock := time.Now()
	limiter.now = func() time.Time { return clock }

	for i := range 5 {
		if !limiter.allow("10.0.0.1") {
			t.Fatalf("request %d refused inside the burst allowance", i+1)
		}
	}
	if limiter.allow("10.0.0.1") {
		t.Error("the request past the burst should have been refused")
	}

	// One tenth of a second buys exactly one token back at 10/s.
	clock = clock.Add(100 * time.Millisecond)
	if !limiter.allow("10.0.0.1") {
		t.Error("a token should have refilled")
	}
	if limiter.allow("10.0.0.1") {
		t.Error("only one token should have refilled")
	}
}

// One client exhausting its allowance must not affect anyone else — the whole
// point is to contain a bad caller, not to throttle everybody.
func TestRateLimiter_IsolatesClients(t *testing.T) {
	limiter := newRateLimiter(10, 3, time.Minute)
	clock := time.Now()
	limiter.now = func() time.Time { return clock }

	for range 3 {
		limiter.allow("10.0.0.1")
	}
	if limiter.allow("10.0.0.1") {
		t.Fatal("the noisy client should be limited")
	}
	if !limiter.allow("10.0.0.2") {
		t.Error("a different client was refused because of the first one")
	}
}

// Buckets must not accumulate for every address ever seen.
func TestRateLimiter_ForgetsIdleClients(t *testing.T) {
	limiter := newRateLimiter(10, 3, time.Minute)
	clock := time.Now()
	limiter.now = func() time.Time { return clock }

	limiter.allow("10.0.0.1")
	if len(limiter.buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(limiter.buckets))
	}

	// A later request from someone else sweeps the idle bucket away.
	clock = clock.Add(2 * time.Minute)
	limiter.allow("10.0.0.2")

	if _, still := limiter.buckets["10.0.0.1"]; still {
		t.Error("the idle client's bucket was kept")
	}
}

func TestRateLimiter_RefillIsCappedAtBurst(t *testing.T) {
	limiter := newRateLimiter(10, 3, time.Minute)
	clock := time.Now()
	limiter.now = func() time.Time { return clock }

	limiter.allow("10.0.0.1")

	// An hour idle must not bank an hour's worth of tokens.
	clock = clock.Add(time.Hour)
	for i := range 3 {
		if !limiter.allow("10.0.0.1") {
			t.Fatalf("request %d refused after a long idle period", i+1)
		}
	}
	if limiter.allow("10.0.0.1") {
		t.Error("tokens accumulated beyond the burst size")
	}
}

// The control that actually contains a slow attack. Rate limiting does not:
// 60 concurrent expensive searches from one client never tripped it, because
// each held a connection for ten seconds and tokens refilled faster than they
// were spent.
func TestRateLimiter_CapsConcurrentRequests(t *testing.T) {
	limiter := newRateLimiter(10, 100, time.Minute)

	for i := range maxInFlightPerClient {
		if !limiter.acquire("10.0.0.1") {
			t.Fatalf("request %d refused inside the in-flight allowance", i+1)
		}
	}
	if limiter.acquire("10.0.0.1") {
		t.Error("a request past the in-flight cap should have been refused")
	}

	// Another client is unaffected: the cap contains one caller, not everyone.
	if !limiter.acquire("10.0.0.2") {
		t.Error("a different client was refused because of the first one")
	}

	// Finishing a request frees the slot again.
	limiter.release("10.0.0.1")
	if !limiter.acquire("10.0.0.1") {
		t.Error("releasing a slot did not free capacity")
	}
}

// A bucket with work still running must survive the sweep, or release would
// decrement a bucket that no longer exists and the count would drift.
func TestRateLimiter_KeepsBucketsWithWorkRunning(t *testing.T) {
	limiter := newRateLimiter(10, 10, time.Minute)
	clock := time.Now()
	limiter.now = func() time.Time { return clock }

	limiter.allow("10.0.0.1")
	if !limiter.acquire("10.0.0.1") {
		t.Fatal("acquire failed")
	}

	clock = clock.Add(2 * time.Minute)
	limiter.allow("10.0.0.2") // triggers a sweep

	if _, still := limiter.buckets["10.0.0.1"]; !still {
		t.Fatal("a bucket with a request in flight was swept away")
	}

	limiter.release("10.0.0.1")
	if got := limiter.buckets["10.0.0.1"].inFlight; got != 0 {
		t.Errorf("in-flight count = %d after release, want 0", got)
	}
}
