package wikidata

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	sparqlEndpoint = "https://query.wikidata.org/sparql"
	userAgent      = "museumscraper/1.0"
	maxRetries     = 3
)

// Client queries the Wikidata SPARQL endpoint. Free, no API key required.
type Client struct {
	httpClient *http.Client
}

// NewClient creates a Wikidata SPARQL client.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
	}
}

// sparqlResponse is the raw SPARQL JSON response format.
type sparqlResponse struct {
	Results struct {
		Bindings []map[string]struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"bindings"`
	} `json:"results"`
}

// query executes a SPARQL query and returns the raw bindings.
func (c *Client) query(ctx context.Context, sparql string) (*sparqlResponse, error) {
	params := url.Values{}
	params.Set("query", sparql)
	params.Set("format", "json")

	reqURL := fmt.Sprintf("%s?%s", sparqlEndpoint, params.Encode())

	var lastErr error
	for attempt := range maxRetries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("wikidata: create request: %w", err)
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/sparql-results+json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			backoff(ctx, attempt)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			backoff(ctx, attempt)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("wikidata: HTTP %d", resp.StatusCode)
		}

		var result sparqlResponse
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("wikidata: decode response: %w", err)
		}

		return &result, nil
	}
	return nil, fmt.Errorf("wikidata: all retries exhausted: %w", lastErr)
}

func backoff(ctx context.Context, attempt int) {
	if attempt == 0 {
		return
	}
	wait := time.Duration(1<<uint(attempt-1)) * time.Second
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
