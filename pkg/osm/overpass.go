// Package osm reads museums from OpenStreetMap via the Overpass API.
//
// OpenStreetMap is the complement to the wiki sources: it is mapped by people
// standing in front of the building, so it holds thousands of small local
// museums that never acquired a Wikipedia article or a Wikidata item. What it
// lacks is prose — an OSM museum is a name, a position and a handful of tags.
package osm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// SourceName identifies records that came from OpenStreetMap.
const SourceName = "openstreetmap"

// endpoints are Overpass instances, tried in order. The main instance is
// frequently overloaded and answers with an HTML error page, so a mirror is
// tried before a query is treated as failed.
var endpoints = []string{
	"https://overpass-api.de/api/interpreter",
	"https://overpass.kumi.systems/api/interpreter",
	"https://overpass.private.coffee/api/interpreter",
}

const (
	// defaultUserAgent identifies this client. Overpass asks that automated
	// clients be identifiable.
	defaultUserAgent = "museum-pipeline/1.0 (https://github.com/example/museum)"

	// minRequestInterval respects the Overpass fair-use policy, which asks for
	// a light touch from anonymous clients.
	minRequestInterval = 3 * time.Second

	// serverTimeout is the budget given to Overpass inside the query itself.
	serverTimeout = 180

	// requestTimeout must exceed serverTimeout so the server gets to answer.
	requestTimeout = 300 * time.Second

	maxAttempts = 3
)

// Client queries the Overpass API.
type Client struct {
	httpClient *http.Client
	userAgent  string
	endpoints  []string
	gate       limiter
}

// NewClient returns a Client with rate limiting and a descriptive user agent.
// Set OVERPASS_USER_AGENT to supply your own contact details.
func NewClient() *Client {
	agent := os.Getenv("OVERPASS_USER_AGENT")
	if agent == "" {
		agent = defaultUserAgent
	}
	return &Client{
		httpClient: &http.Client{Timeout: requestTimeout},
		userAgent:  agent,
		endpoints:  endpoints,
	}
}

// element is one OSM object in an Overpass JSON response.
type element struct {
	Type string  `json:"type"`
	ID   int64   `json:"id"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	// Center carries the computed centroid for ways and relations, which have
	// no position of their own.
	Center struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	} `json:"center"`
	Tags map[string]string `json:"tags"`
}

// response is the Overpass JSON envelope.
type response struct {
	Elements []element `json:"elements"`
}

// query runs an Overpass QL query, falling back across endpoints and retrying
// transient failures.
func (c *Client) query(ctx context.Context, overpassQL string) ([]element, error) {
	var lastErr error

	for attempt := range maxAttempts {
		for _, endpoint := range c.endpoints {
			if err := c.gate.wait(ctx); err != nil {
				return nil, err
			}

			elements, err := c.do(ctx, endpoint, overpassQL)
			if err == nil {
				return elements, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("%s: %w", endpoint, err)
		}

		if err := sleepCtx(ctx, time.Duration(1<<uint(attempt))*5*time.Second); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("all endpoints failed: %w", lastErr)
}

// do performs one request against one endpoint.
func (c *Client) do(ctx context.Context, endpoint, overpassQL string) ([]element, error) {
	form := url.Values{}
	form.Set("data", overpassQL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call overpass: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("overpass returned %s", resp.Status)
	}

	// An overloaded Overpass instance answers 200 with an HTML error document
	// rather than JSON, so the content type is the only reliable signal.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		return nil, fmt.Errorf("overpass returned %s instead of JSON", ct)
	}

	var parsed response
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode overpass response: %w", err)
	}
	return parsed.Elements, nil
}

// limiter spaces requests at least minRequestInterval apart.
type limiter struct {
	mu   sync.Mutex
	next time.Time
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	delay := l.next.Sub(now)
	if delay < 0 {
		delay = 0
	}
	l.next = now.Add(delay + minRequestInterval)
	l.mu.Unlock()

	if delay == 0 {
		return ctx.Err()
	}
	return sleepCtx(ctx, delay)
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
