package api

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// requestsPerSecond is the sustained rate one client may keep up.
	//
	// Generous for a map application, which loads a view and then pans: a
	// handful of requests in a burst, then quiet. Deliberately not tuned to the
	// cheap requests — a radius query answers in 1 ms, so a legitimate client
	// never approaches this.
	requestsPerSecond = 10

	// burstSize is how many requests may arrive at once before the rate applies.
	// A map view loading museums and exhibitions together, twice over, fits.
	burstSize = 30

	// maxInFlightPerClient is how many requests one client may have running at
	// once.
	//
	// This, not the request rate, is what stops a single caller monopolising
	// the connection pool. The two limit different things, and only this one
	// bounds a *slow* attack: 60 concurrent expensive searches from one client
	// never tripped the rate limiter, because each occupied a connection for
	// ten seconds and tokens refilled faster than they were spent. The rate
	// stayed low; the cost did not.
	//
	// Four is well above what a map client needs — it loads museums and
	// exhibitions for a view, then waits for the user — and well below the
	// pool, so cheap queries always have connections left.
	maxInFlightPerClient = 4

	// clientTTL is how long an idle client's bucket is kept. Long enough that a
	// browsing session keeps one bucket, short enough that the table cannot
	// grow without bound.
	clientTTL = 10 * time.Minute
)

// errRateLimited is returned to a client that is asking too often.
var errRateLimited = errors.New("too many requests; slow down")

// errTooManyInFlight is returned to a client with too much work already
// running. Distinct from the rate error because the remedy is different: wait
// for your own requests to finish, rather than simply ask less often.
var errTooManyInFlight = errors.New("too many concurrent requests; wait for your requests to finish")

// bucket is one client's token allowance.
//
// A token bucket rather than a fixed window: a window lets a client spend its
// whole allowance in the last instant of one window and again in the first
// instant of the next, which is exactly the burst the limit exists to stop.
type bucket struct {
	tokens float64
	last   time.Time
	// inFlight is how many of this client's requests are running right now.
	inFlight int
}

// rateLimiter caps how often one client may ask.
//
// This is the missing floor under dishonest load. A per-request deadline
// bounds how long any one query runs, but nothing stopped a single client
// opening the pool's every connection and holding it: during a load test the
// first innocent request waited 9.2 seconds behind 30 deliberately expensive
// ones. Bounding the rate is what keeps one caller from spending everyone
// else's capacity.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
	ttl     time.Duration
	// now is injectable so the tests do not have to sleep.
	now func() time.Time
}

func newRateLimiter(rate, burst float64, ttl time.Duration) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		ttl:     ttl,
		now:     time.Now,
	}
}

// acquire reserves a slot for one in-flight request, reporting whether the
// client had one to spare. A caller that succeeds must call release.
func (l *rateLimiter) acquire(client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	held, ok := l.buckets[client]
	if !ok {
		held = &bucket{tokens: l.burst, last: l.now()}
		l.buckets[client] = held
	}
	if held.inFlight >= maxInFlightPerClient {
		return false
	}
	held.inFlight++
	return true
}

// release gives back a slot taken by acquire.
func (l *rateLimiter) release(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if held, ok := l.buckets[client]; ok && held.inFlight > 0 {
		held.inFlight--
	}
}

// allow reports whether this client may make one more request now.
func (l *rateLimiter) allow(client string) bool {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	held, ok := l.buckets[client]
	if !ok {
		// Sweeping on a miss keeps the table bounded without a background
		// goroutine whose lifetime would then have to be managed.
		l.sweep(now)
		l.buckets[client] = &bucket{tokens: l.burst - 1, last: now}
		return true
	}

	held.tokens = min(l.burst, held.tokens+now.Sub(held.last).Seconds()*l.rate)
	held.last = now

	if held.tokens < 1 {
		return false
	}
	held.tokens--
	return true
}

// sweep drops buckets no one has used recently. The caller holds the lock.
func (l *rateLimiter) sweep(now time.Time) {
	for client, held := range l.buckets {
		if held.inFlight == 0 && now.Sub(held.last) > l.ttl {
			delete(l.buckets, client)
		}
	}
}

// withRateLimit rejects a client that is asking faster than the limit allows.
//
// Clients are identified by IP. Behind a proxy every request would appear to
// come from the proxy, so a deployment that terminates elsewhere must pass the
// real address through — deliberately not read from X-Forwarded-For here,
// because that header is caller-supplied and trusting it unconditionally lets
// anyone forge a fresh identity per request and bypass the limit entirely.
func withRateLimit(limiter *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probes are exempt: rate-limiting an orchestrator's liveness check
		// turns a busy minute into a restart.
		if r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		client := clientAddr(r)

		if !limiter.allow(client) {
			w.Header().Set("Retry-After", strconv.Itoa(1))
			writeError(w, http.StatusTooManyRequests, errRateLimited)
			return
		}

		// Refused immediately rather than queued: a client that is already at
		// its limit is better told so in a millisecond than made to wait, and
		// queueing here would reintroduce the unbounded wait this prevents.
		if !limiter.acquire(client) {
			w.Header().Set("Retry-After", strconv.Itoa(1))
			writeError(w, http.StatusTooManyRequests, errTooManyInFlight)
			return
		}
		defer limiter.release(client)

		next.ServeHTTP(w, r)
	})
}

// clientAddr is the address a request came from, without its port — otherwise
// every connection from one client would count as a different client.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
