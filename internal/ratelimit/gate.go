// Package ratelimit spaces outbound requests to a rate a remote API tolerates,
// and adapts when it turns out not to.
//
// Every public source this catalogue draws on — Wikipedia, Wikidata, Nominatim,
// Overpass — publishes an etiquette rate and enforces it with 429s. Getting
// that wrong is not a performance problem but a data-loss one: a throttled
// crawl skips whole pages, and what it skips is invisible afterwards. The same
// mistake has now been made twice in this codebase, once per client, so the
// logic lives here rather than being written a third time.
package ratelimit

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Gate spaces requests, widening the gap when the API pushes back.
//
// A fixed interval is a guess about someone else's capacity. When the guess is
// wrong the only signal is a refusal, and retrying at the same rate earns
// another. Widening on refusal and narrowing on success lets a long run settle
// at whatever rate is actually being served.
//
// The zero value is not usable; call NewGate. Safe for concurrent use, and
// meant to be shared: the limit belongs to the remote endpoint, so one Gate per
// endpoint per process, not one per client object.
type Gate struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration

	min, max time.Duration
}

// NewGate returns a Gate that starts at min and will widen no further than max.
func NewGate(min, max time.Duration) *Gate {
	if max < min {
		max = min
	}
	return &Gate{interval: min, min: min, max: max}
}

// Wait blocks until the caller may issue a request, or until ctx is done.
func (g *Gate) Wait(ctx context.Context) error {
	g.mu.Lock()
	now := time.Now()
	delay := g.next.Sub(now)
	if delay < 0 {
		delay = 0
	}
	g.next = now.Add(delay + g.interval)
	g.mu.Unlock()

	if delay == 0 {
		return ctx.Err()
	}
	return Sleep(ctx, delay)
}

// SlowDown widens the spacing after the API refuses a request.
func (g *Gate) SlowDown() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.interval *= 2; g.interval > g.max {
		g.interval = g.max
	}
}

// SpeedUp narrows the spacing again after a success.
//
// Recovery is deliberately slower than the backoff: halving on every success
// would undo the widening as fast as it was applied and oscillate straight back
// into refusals.
func (g *Gate) SpeedUp() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.interval > g.min {
		if g.interval -= g.interval / 8; g.interval < g.min {
			g.interval = g.min
		}
	}
}

// Interval reports the current spacing, for tests and for logging.
func (g *Gate) Interval() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.interval
}

// RetryAfter reads a Retry-After header in either form the spec allows: a
// number of seconds, or an HTTP date. An unreadable or past value yields zero,
// leaving the caller's own backoff in charge.
//
// The server's own figure beats a guess. Backing off one second when it asked
// for sixty simply spends the remaining attempts too early, which is how a
// retry budget gets exhausted without ever waiting long enough to succeed.
func RetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

// Backoff returns the exponential delay before the given retry attempt,
// counting from one.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// Sleep waits for d, or until ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
