package ratelimit

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// The gate widens after a refusal and recovers gradually, rather than retrying
// at the rate that was just refused.
func TestGate_AdaptsToRefusal(t *testing.T) {
	g := NewGate(100*time.Millisecond, 2*time.Second)

	g.SlowDown()
	first := g.Interval()
	if first <= 100*time.Millisecond {
		t.Fatalf("interval = %v after a refusal, want wider", first)
	}

	// Sustained refusal must not widen without bound.
	for range 20 {
		g.SlowDown()
	}
	if got := g.Interval(); got != 2*time.Second {
		t.Errorf("interval = %v, want it capped at 2s", got)
	}

	// Recovery is gradual and settles at the floor, not below it.
	before := g.Interval()
	g.SpeedUp()
	if g.Interval() >= before {
		t.Errorf("interval = %v after success, want narrower than %v", g.Interval(), before)
	}
	for range 500 {
		g.SpeedUp()
	}
	if got := g.Interval(); got != 100*time.Millisecond {
		t.Errorf("interval = %v after sustained success, want the 100ms floor", got)
	}
}

// Concurrent callers must queue behind each other rather than all firing at
// once: the limit is per endpoint, and a burst is what earns a 429.
func TestGate_SerialisesConcurrentCallers(t *testing.T) {
	g := NewGate(20*time.Millisecond, time.Second)
	ctx := context.Background()

	start := time.Now()
	done := make(chan struct{})
	for range 5 {
		go func() {
			_ = g.Wait(ctx)
			done <- struct{}{}
		}()
	}
	for range 5 {
		<-done
	}

	// Five callers at 20ms apart cannot finish in under about 80ms.
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Errorf("five calls took %v, want them spaced out", elapsed)
	}
}

func TestGate_WaitRespectsCancellation(t *testing.T) {
	g := NewGate(time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	_ = g.Wait(ctx) // takes the first slot, so the next must wait an hour
	cancel()

	if err := g.Wait(ctx); err == nil {
		t.Error("Wait should return once the context is cancelled")
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"60", time.Minute},
		{"0", 0},
		{"", 0},
		{"-5", 0},
		{"not a number", 0},
	}
	for _, tt := range tests {
		if got := RetryAfter(tt.header); got != tt.want {
			t.Errorf("RetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}

	if got := RetryAfter(time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)); got < 25*time.Second {
		t.Errorf("RetryAfter(http date) = %v, want about 30s", got)
	}
	// A date already past must not produce a negative wait.
	if got := RetryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)); got != 0 {
		t.Errorf("RetryAfter(past date) = %v, want 0", got)
	}
}

func TestBackoff(t *testing.T) {
	for attempt, want := range map[int]time.Duration{
		0: 0, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second,
	} {
		if got := Backoff(attempt); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}
