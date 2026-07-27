// Package wikipedia provides a small client for the Wikipedia action API plus
// the parsing needed to turn "List of museums in X" articles into structured
// museum records.
package wikipedia

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// apiPath is the Wikipedia action API endpoint.
	apiPath = "https://en.wikipedia.org/w/api.php"

	// DefaultUserAgent identifies this client to Wikipedia. Their API policy
	// asks for a descriptive agent with contact details; replace the URL when
	// deploying this for real.
	DefaultUserAgent = "museum-pipeline/1.0 (https://github.com/example/museum)"

	// maxTitlesPerQuery is the number of titles the API accepts in a single
	// multi-title query for unauthenticated clients.
	maxTitlesPerQuery = 50

	// minRequestInterval spaces out calls. Wikipedia's API etiquette asks
	// unauthenticated clients to make requests serially and at a modest rate;
	// firing them back to back earns 429 responses.
	minRequestInterval = 200 * time.Millisecond

	// maxAttempts is how many times a throttled or failed request is retried.
	maxAttempts = 4
)

// Client talks to the Wikipedia action API. It is safe for concurrent use; the
// rate limiter is shared across callers.
type Client struct {
	httpClient *http.Client
	userAgent  string
	gate       limiter
}

// NewClient returns a Client with a sane timeout and a descriptive user agent.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		userAgent:  DefaultUserAgent,
	}
}

// get performs an API request with the given parameters and decodes the JSON
// response into out.
//
// Requests are spaced by minRequestInterval and retried on 429 and 5xx
// responses. Without this the crawler trips Wikipedia's rate limiter and whole
// list pages get skipped, which silently loses museums.
func (c *Client) get(ctx context.Context, params url.Values, out any) error {
	params.Set("format", "json")
	requestURL := apiPath + "?" + params.Encode()

	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return err
			}
		}
		if err := c.gate.wait(ctx); err != nil {
			return err
		}

		retryable, err := c.doRequest(ctx, requestURL, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// doRequest issues a single request, reporting whether a failure is worth
// retrying.
func (c *Client) doRequest(ctx context.Context, requestURL string, out any) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A cancelled context is deliberate; anything else may be transient.
		return ctx.Err() == nil, fmt.Errorf("call wikipedia api: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return true, fmt.Errorf("wikipedia api returned %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("wikipedia api returned %s", resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, fmt.Errorf("decode wikipedia response: %w", err)
	}
	return false, nil
}

// backoff returns the delay before the given retry attempt.
func backoff(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt-1)) * time.Second
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

// limiter spaces requests at least minRequestInterval apart.
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
	l.next = now.Add(delay + minRequestInterval)
	l.mu.Unlock()

	if delay == 0 {
		return ctx.Err()
	}
	return sleepCtx(ctx, delay)
}

// FetchCategoryMembers returns one page of a category's members. Pass the
// cmcontinue token from a previous response to fetch the next page.
func (c *Client) FetchCategoryMembers(ctx context.Context, categoryTitle, cmContinue string) (*APIResponse, error) {
	params := url.Values{}
	params.Set("action", "query")
	params.Set("list", "categorymembers")
	params.Set("cmtitle", strings.ReplaceAll(categoryTitle, " ", "_"))
	params.Set("cmlimit", "500")
	if cmContinue != "" {
		params.Set("cmcontinue", cmContinue)
	}

	var resp APIResponse
	if err := c.get(ctx, params, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// FetchPageContent returns the raw wikitext of a page.
func (c *Client) FetchPageContent(ctx context.Context, pageTitle string) (*PageAPIResponse, error) {
	params := url.Values{}
	params.Set("action", "query")
	params.Set("prop", "revisions")
	params.Set("rvprop", "content")
	params.Set("rvslots", "main")
	params.Set("titles", strings.ReplaceAll(pageTitle, " ", "_"))

	var resp PageAPIResponse
	if err := c.get(ctx, params, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// FetchPageMetadata resolves up to maxTitlesPerQuery article titles in a single
// request, returning the canonical URL, coordinates, short description and
// Wikidata id for each.
//
// The result is keyed by the *requested* title, with redirects and title
// normalisation followed transparently, so callers can look up what they asked
// for. Titles that do not exist come back with Missing set rather than being
// omitted.
func (c *Client) FetchPageMetadata(ctx context.Context, titles []string) (map[string]PageMetadata, error) {
	if len(titles) == 0 {
		return map[string]PageMetadata{}, nil
	}
	if len(titles) > maxTitlesPerQuery {
		return nil, fmt.Errorf("too many titles: %d (max %d)", len(titles), maxTitlesPerQuery)
	}

	params := url.Values{}
	params.Set("action", "query")
	params.Set("prop", "coordinates|info|pageprops")
	params.Set("inprop", "url")
	params.Set("coprimary", "primary")
	params.Set("redirects", "1")
	params.Set("formatversion", "2")
	params.Set("titles", strings.Join(titles, "|"))

	var resp metadataResponse
	if err := c.get(ctx, params, &resp); err != nil {
		return nil, err
	}

	// Follow the normalisation and redirect chains so each requested title maps
	// onto the page the API actually returned.
	resolved := make(map[string]string, len(titles))
	for _, t := range titles {
		resolved[t] = t
	}
	for _, mappings := range [][]titleMapping{resp.Query.Normalized, resp.Query.Redirects} {
		for _, m := range mappings {
			for requested, current := range resolved {
				if current == m.From {
					resolved[requested] = m.To
				}
			}
		}
	}

	pages := make(map[string]pageResult, len(resp.Query.Pages))
	for _, p := range resp.Query.Pages {
		pages[p.Title] = p
	}

	out := make(map[string]PageMetadata, len(titles))
	for _, requested := range titles {
		page, ok := pages[resolved[requested]]
		if !ok {
			out[requested] = PageMetadata{Title: requested, Missing: true}
			continue
		}
		out[requested] = page.toMetadata()
	}
	return out, nil
}

// toMetadata flattens the API's page shape into the exported PageMetadata.
func (p pageResult) toMetadata() PageMetadata {
	meta := PageMetadata{
		PageID:      p.PageID,
		Title:       p.Title,
		Namespace:   p.NS,
		Missing:     p.Missing,
		URL:         p.FullURL,
		Description: p.PageProps["wikibase-shortdesc"],
		WikidataID:  p.PageProps["wikibase_item"],
	}
	if _, isDisambiguation := p.PageProps["disambiguation"]; isDisambiguation {
		meta.IsDisambiguation = true
	}
	if len(p.Coordinates) > 0 {
		meta.Latitude = p.Coordinates[0].Lat
		meta.Longitude = p.Coordinates[0].Lon
		meta.HasCoordinates = true
	}
	return meta
}
