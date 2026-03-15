package location

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// GeoResult is the unified result type returned by all Geocoder implementations.
type GeoResult struct {
	PlaceID     int64   `json:"place_id"`
	OsmType     string  `json:"osm_type"`
	OsmID       int64   `json:"osm_id"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	Class       string  `json:"class"`
	Type        string  `json:"type"`
	Importance  float64 `json:"importance"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	City        string  `json:"city"`
	Postcode    string  `json:"postcode"`
}

// Geocoder looks up a location query and returns structured geo results.
type Geocoder interface {
	Geocode(ctx context.Context, query string) (*GeoResult, error)
}

// PlaceDetailer fetches extended details for a place identified by its OSM type and ID.
type PlaceDetailer interface {
	PlaceDetails(ctx context.Context, osmType string, osmID int64) (*NominatimDetailsResponse, error)
}

// newHTTPClient returns an HTTP client configured with sensible timeouts
// and connection pooling for external geocoding APIs.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 10,
			MaxConnsPerHost:     10,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

// doWithRetry executes an HTTP request with exponential backoff retries.
// It retries on transient errors (5xx, 429) up to maxRetries times.
func doWithRetry(ctx context.Context, client *http.Client, req *http.Request, maxRetries int) (*http.Response, error) {
	var lastErr error
	for attempt := range maxRetries + 1 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			backoff(ctx, attempt)
			// Re-create request with fresh context for retry
			req = req.Clone(ctx)
			continue
		}

		// Success
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		// Rate limited or server error — retry
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
			backoff(ctx, attempt)
			req = req.Clone(ctx)
			continue
		}

		// Client error (4xx except 429) — don't retry
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}
	return nil, fmt.Errorf("all %d retries exhausted: %w", maxRetries+1, lastErr)
}

// backoff waits for an exponentially increasing duration, respecting context cancellation.
func backoff(ctx context.Context, attempt int) {
	if attempt == 0 {
		return
	}
	wait := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s, ...
	if wait > 16*time.Second {
		wait = 16 * time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
