package scraper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var (
	errNoURL  = errors.New("no website URL provided")
	errNoData = errors.New("no structured data found")
)

const (
	userAgent  = "Mozilla/5.0 (compatible; museumscraper/1.0; +https://github.com/artback/museumscraper)"
	maxBodySize = 5 * 1024 * 1024 // 5 MB
	maxRetries  = 2
)

// Fetcher retrieves web pages with appropriate rate limiting and error handling.
type Fetcher struct {
	client  *http.Client
	limiter *time.Ticker
}

// NewFetcher creates a website fetcher that limits requests to the given rate.
func NewFetcher(requestInterval time.Duration) *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 5,
				MaxConnsPerHost:     5,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		limiter: time.NewTicker(requestInterval),
	}
}

// Close releases the fetcher's resources.
func (f *Fetcher) Close() {
	f.limiter.Stop()
}

// Fetch retrieves the HTML content of a URL.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("empty URL")
	}

	// Ensure HTTPS
	if strings.HasPrefix(rawURL, "http://") {
		rawURL = "https://" + rawURL[7:]
	}
	if !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + rawURL
	}

	// Rate limit
	select {
	case <-f.limiter.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	var lastErr error
	for attempt := range maxRetries + 1 {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
		if err != nil {
			return "", fmt.Errorf("scraper: create request for %s: %w", rawURL, err)
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")

		resp, err := f.client.Do(req)
		if err != nil {
			lastErr = err
			backoff(ctx, attempt)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
			backoff(ctx, attempt)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("scraper: HTTP %d from %s", resp.StatusCode, rawURL)
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("scraper: read body from %s: %w", rawURL, err)
		}

		return string(body), nil
	}

	return "", fmt.Errorf("scraper: all retries exhausted for %s: %w", rawURL, lastErr)
}

func backoff(ctx context.Context, attempt int) {
	if attempt == 0 {
		return
	}
	wait := time.Duration(1<<uint(attempt-1)) * time.Second
	if wait > 8*time.Second {
		wait = 8 * time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
