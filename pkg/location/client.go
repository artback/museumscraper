package location

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
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
// level because the limit applies per client, not per caller: the enrichment
// pipeline runs steps concurrently and would otherwise burst.
var gate limiter

// limiter spaces calls at least minInterval apart.
type limiter struct {
	mu   sync.Mutex
	next time.Time
}

// wait blocks until the caller may issue a request, or until ctx is done.
func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	delay := l.next.Sub(now)
	if delay < 0 {
		delay = 0
	}
	// Reserve this caller's slot before unlocking so concurrent callers queue
	// behind it rather than all racing for the same instant.
	l.next = now.Add(delay + minInterval)
	l.mu.Unlock()

	if delay == 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// get performs a rate-limited, properly identified GET against Nominatim and
// decodes the JSON response into out.
func get(ctx context.Context, path string, params interface{ Encode() string }, out any) error {
	if err := gate.wait(ctx); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path+"?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call nominatim: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nominatim returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode nominatim response: %w", err)
	}
	return nil
}
