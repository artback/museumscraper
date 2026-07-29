package wikipedia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"museum/internal/ratelimit"
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
	if first.limiter() != apiGate {
		t.Error("clients should share the process-wide gate")
	}
	// A bare literal must share it too, rather than dereference nil.
	if (&Client{}).limiter() != apiGate {
		t.Error("a zero-value client should fall back to the shared gate")
	}
}

// (adaptive limiter behaviour is tested in internal/ratelimit)

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
	client.gate = ratelimit.NewGate(time.Millisecond, time.Millisecond) // a private gate, so the test does not slow the shared one

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
