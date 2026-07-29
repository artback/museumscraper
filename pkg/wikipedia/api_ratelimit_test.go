package wikipedia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Two clients must not run at twice the rate of one. A crawl builds a client
// per source, and independent limiters put the lists and category sources
// together at double the tolerated rate: both were throttled and whole
// countries were skipped.
func TestClients_ShareOneRateLimiter(t *testing.T) {
	first, second := NewClient(), NewClient()
	if first.limiter() != second.limiter() {
		t.Fatal("each client has its own limiter, so N sources run at N times the rate")
	}
	if first.limiter() != &apiGate {
		t.Error("clients should share the process-wide gate")
	}
	// A bare literal must share it too, rather than dereference nil.
	if (&Client{}).limiter() != &apiGate {
		t.Error("a zero-value client should fall back to the shared gate")
	}
}

// The limiter widens after a refusal and recovers gradually, rather than
// retrying at the rate that was just refused.
func TestLimiter_AdaptsToThrottling(t *testing.T) {
	var l limiter

	l.slowDown()
	first := l.interval
	if first <= minRequestInterval {
		t.Fatalf("interval = %v after a refusal, want more than %v", first, minRequestInterval)
	}

	l.slowDown()
	if l.interval <= first {
		t.Errorf("interval = %v after a second refusal, want more than %v", l.interval, first)
	}

	// Sustained refusal must not widen without bound.
	for range 20 {
		l.slowDown()
	}
	if l.interval != maxRequestInterval {
		t.Errorf("interval = %v, want it capped at %v", l.interval, maxRequestInterval)
	}

	// Recovery is gradual, and settles back at the floor rather than below it.
	before := l.interval
	l.speedUp()
	if l.interval >= before {
		t.Errorf("interval = %v after success, want less than %v", l.interval, before)
	}
	for range 500 {
		l.speedUp()
	}
	if l.interval != minRequestInterval {
		t.Errorf("interval = %v after sustained success, want the %v floor", l.interval, minRequestInterval)
	}
}

func TestParseRetryAfter(t *testing.T) {
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
		if got := parseRetryAfter(tt.header); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}

	// The HTTP-date form must also be understood.
	if got := parseRetryAfter(time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)); got < 25*time.Second {
		t.Errorf("parseRetryAfter(http date) = %v, want about 30s", got)
	}
}

// A throttled request must be retried, not abandoned: every abandoned category
// page drops the museums listed on it.
func TestClient_RetriesAfterThrottling(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()

		if n < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"batchcomplete":""}`))
	}))
	defer server.Close()

	client := NewClient()
	client.gate = &limiter{} // a private gate, so the test does not slow the shared one

	var out APIResponse
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.getFrom(ctx, server.URL, &out); err != nil {
		t.Fatalf("request gave up rather than retrying: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}
