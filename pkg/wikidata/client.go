// Package wikidata queries the Wikidata Query Service for museums.
//
// Wikidata is the most complete machine-readable catalogue of museums there is:
// it knows about roughly 81,000, against the ~7,000 reachable by scraping
// English Wikipedia's "List of museums in X" articles. It also carries
// coordinates, official websites and links to the corresponding Wikipedia
// article, which is exactly the data this pipeline wants.
package wikidata

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

const (
	// endpoint is the public SPARQL endpoint.
	endpoint = "https://query.wikidata.org/sparql"

	// defaultUserAgent identifies this client. The query service blocks
	// requests with a generic agent, and asks for contact details.
	defaultUserAgent = "museum-pipeline/1.0 (https://github.com/example/museum)"

	// minRequestInterval keeps the crawl within the query service's fair-use
	// expectations for anonymous clients.
	minRequestInterval = 1200 * time.Millisecond

	// queryTimeout is generous: the service itself caps queries at 60s, and a
	// large page can take most of that.
	queryTimeout = 120 * time.Second

	maxAttempts = 4
)

// Client executes SPARQL queries against the Wikidata Query Service.
type Client struct {
	httpClient *http.Client
	userAgent  string
	endpoint   string
	gate       limiter
}

// NewClient returns a Client with rate limiting and a descriptive user agent.
// Set WIKIDATA_USER_AGENT to supply your own contact details.
func NewClient() *Client {
	agent := os.Getenv("WIKIDATA_USER_AGENT")
	if agent == "" {
		agent = defaultUserAgent
	}
	return &Client{
		httpClient: &http.Client{Timeout: queryTimeout},
		userAgent:  agent,
		endpoint:   endpoint,
	}
}

// results is the SPARQL JSON result shape. Every binding is a string value,
// whatever its RDF type, which is all this package needs.
type results struct {
	Results struct {
		Bindings []map[string]struct {
			Value string `json:"value"`
		} `json:"bindings"`
	} `json:"results"`
}

// binding is one row of a result set, flattened to plain strings.
type binding map[string]string

// query runs sparql and returns the rows, retrying throttled and transient
// failures with exponential backoff.
func (c *Client) query(ctx context.Context, sparql string) ([]binding, error) {
	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(1<<uint(attempt-1))*2*time.Second); err != nil {
				return nil, err
			}
		}
		if err := c.gate.wait(ctx); err != nil {
			return nil, err
		}

		rows, retryable, err := c.do(ctx, sparql)
		if err == nil {
			return rows, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// do performs a single query, reporting whether a failure is worth retrying.
func (c *Client) do(ctx context.Context, sparql string) (rows []binding, retryable bool, err error) {
	params := url.Values{}
	params.Set("query", sparql)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/sparql-results+json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("call query service: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("query service returned %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("query service returned %s", resp.Status)
	}

	var parsed results
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		// Museum pages run to several megabytes and the endpoint sometimes cuts
		// the body short, which surfaces here as an unexpected EOF. That is a
		// transport failure rather than a bad query, so it is worth retrying.
		return nil, ctx.Err() == nil, fmt.Errorf("decode query results: %w", err)
	}

	rows = make([]binding, 0, len(parsed.Results.Bindings))
	for _, raw := range parsed.Results.Bindings {
		row := make(binding, len(raw))
		for k, v := range raw {
			row[k] = v.Value
		}
		rows = append(rows, row)
	}
	return rows, false, nil
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
