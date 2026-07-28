package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"runtime/debug"
	"time"
)

const (
	// requestTimeout bounds how long any one request may occupy a connection,
	// a goroutine and a database backend.
	//
	// Without it nothing bounded a request at all: no WriteTimeout on the
	// server, no deadline on the query. A search for a common word plans as a
	// sequential scan over the whole table, and a hundred concurrent ones took
	// 58 seconds each while pushing an ordinary radius query from 4 ms to 51 s.
	// Every one of them returned 200 — the work was done, just far too late for
	// anyone to want it.
	//
	// Well above the slowest honest query and far below a client's patience.
	requestTimeout = 10 * time.Second

	// readinessTimeout bounds the probe's database check. Short, because a
	// probe that has not answered quickly has already answered.
	readinessTimeout = 2 * time.Second

	// maxLoggedQueryChars bounds the query string echoed into the log line.
	// It was unbounded, so a request with a 100 KB parameter wrote a 100 KB log
	// line; about a thousand of those rotated away the entire log ring, which
	// is a cheap way to erase the evidence of whatever else was going on.
	maxLoggedQueryChars = 256
)

// withMiddleware wraps the routes in the behaviour every response needs.
//
// Order matters. Recovery is outermost so it also catches a panic raised by
// another wrapper; CORS is next so preflight is answered without paying for a
// timeout context; the timeout is innermost so it covers only the handler.
func withMiddleware(next http.Handler) http.Handler {
	return recoverPanics(withCORS(withTimeout(logRequests(next))))
}

// withTimeout gives every request a deadline, and the handler a context that
// carries it. Cancelling the context is what actually stops the work: pgx
// propagates it, so Postgres cancels the running query rather than finishing a
// scan nobody is waiting for.
func withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withCORS makes the catalogue reachable from a browser.
//
// The intended consumer is a map showing museums and exhibitions in an area,
// which is a browser application; without these headers it cannot read a single
// response. The data is public and read-only, so any origin may have it — there
// are no cookies, no credentials and no state to protect. Preflight is answered
// here because the routes register GET only, so the mux answers OPTIONS with
// 405 and a browser reads that as a refusal.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Access-Control-Allow-Origin", "*")
		header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		header.Set("Access-Control-Allow-Headers", "Content-Type")
		header.Set("Access-Control-Max-Age", "86400")
		header.Set("Vary", "Origin")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverPanics keeps one failed request from killing the connection silently.
//
// net/http already recovers per connection, so a panic never took down the
// server. What it did was worse to diagnose: the client saw an empty reply with
// no status at all, and because the panic unwound past the logging wrapper the
// request never appeared in the log. An operator watching for errors saw
// nothing — no 500, no latency spike, just a hole.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			// A panic after the header is written cannot be turned into a 500;
			// the status is already on the wire. Logging is all that is left.
			log.Printf("api: panic serving %s %s: %v\n%s",
				r.Method, r.URL.Path, recovered, debug.Stack())
			writeError(w, http.StatusInternalServerError, errors.New("internal error"))
		}()
		next.ServeHTTP(w, r)
	})
}
