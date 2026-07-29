package location

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"museum/internal/ratelimit"
)

// ErrNoResults means the query was valid but Nominatim matched nothing.
var ErrNoResults = errors.New("no results")

const (
	// baseURL is the public Nominatim instance.
	baseURL = "https://nominatim.openstreetmap.org"

	// defaultUserAgent identifies this application. Nominatim rejects requests
	// carrying a generic agent such as Go's default "Go-http-client/1.1", which
	// is why this must never be left empty. Override it with NOMINATIM_USER_AGENT.
	defaultUserAgent = "museum-pipeline/1.0 (https://github.com/example/museum)"

	// minInterval is the shortest gap allowed between requests, per Nominatim's
	// usage policy of at most one request per second.
	minInterval = 1100 * time.Millisecond

	// maxInterval is how far apart requests are spaced once Nominatim has
	// refused some. Slow, but a slow run places every museum and a throttled
	// one places none.
	maxInterval = 30 * time.Second

	// maxAttempts is how many times a refused request is retried.
	maxAttempts = 5

	requestTimeout = 30 * time.Second
)

// httpClient is shared so connections are reused across lookups.
var httpClient = &http.Client{Timeout: requestTimeout}

// userAgent is resolved once, allowing deployments to supply their own contact
// details as Nominatim's policy asks.
var userAgent = func() string {
	if ua := os.Getenv("NOMINATIM_USER_AGENT"); ua != "" {
		return ua
	}
	return defaultUserAgent
}()

// gate serialises outbound requests to respect the rate limit. It is package
// level because the limit applies per endpoint, not per caller: the enrichment
// pipeline runs steps concurrently and would otherwise burst.
var gate = ratelimit.NewGate(minInterval, maxInterval)

// get performs a rate-limited, properly identified GET against Nominatim and
// decodes the JSON response into out.
//
// A refusal is retried rather than returned. There was no retry here at all,
// so a single 429 failed the lookup outright: locating a few hundred museums
// gave up on every one of them the moment Nominatim began throttling, and
// reported them as unlocatable when they were merely asked for too quickly.
func get(ctx context.Context, path string, params interface{ Encode() string }, out any) error {
	requestURL := baseURL + path + "?" + params.Encode()

	var (
		lastErr error
		wait    time.Duration
	)
	for attempt := range maxAttempts {
		if attempt > 0 {
			if wait < ratelimit.Backoff(attempt) {
				wait = ratelimit.Backoff(attempt)
			}
			if err := ratelimit.Sleep(ctx, wait); err != nil {
				return err
			}
		}
		if err := gate.Wait(ctx); err != nil {
			return err
		}

		retryable, retryAfter, err := doRequest(ctx, requestURL, out)
		if err == nil {
			gate.SpeedUp()
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
		gate.SlowDown()
		if retryAfter > wait {
			wait = retryAfter
		}
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// doRequest issues one request, reporting whether a failure is worth retrying.
func doRequest(ctx context.Context, requestURL string, out any) (retryable bool, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return false, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		// A cancelled context is deliberate; anything else may be transient.
		return ctx.Err() == nil, 0, fmt.Errorf("call nominatim: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return true, ratelimit.RetryAfter(resp.Header.Get("Retry-After")),
			fmt.Errorf("nominatim returned %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return false, 0, fmt.Errorf("nominatim returned %s", resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, 0, fmt.Errorf("decode nominatim response: %w", err)
	}
	return false, 0, nil
}
