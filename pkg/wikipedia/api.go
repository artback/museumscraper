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
	"time"

	"museum/internal/ratelimit"
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

	// maxRequestInterval is how far apart the limiter will space requests once
	// the API has pushed back. Slow, but a slow crawl finds every museum and a
	// throttled one silently loses whole countries.
	maxRequestInterval = 5 * time.Second

	// maxAttempts is how many times a throttled or failed request is retried.
	// Raised from four: a category that exhausts its attempts is skipped
	// outright, and each skip drops every museum on that page.
	maxAttempts = 6
)

// apiGate spaces every request this process makes to the Wikipedia API.
//
// The limit belongs to the endpoint, not to a Client. A crawl builds one client
// for the category source and another for the lists source, and while each
// limiter honoured the interval on its own, together they ran at twice the
// intended rate. Wikipedia answered with 429s, both sources burned their
// retries, and the lists crawl was cut down to 14 candidates — "Lists of
// museums in the United States" and "Lists of museums in England by county"
// were skipped entirely, which is thousands of museums lost to a race between
// two halves of the same program.
var apiGate = ratelimit.NewGate(minRequestInterval, maxRequestInterval)

// Client talks to the Wikipedia action API. It is safe for concurrent use; the
// rate limiter is shared across callers.
type Client struct {
	httpClient *http.Client
	userAgent  string
	gate       *ratelimit.Gate
}

// NewClient returns a Client with a sane timeout and a descriptive user agent.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		userAgent:  DefaultUserAgent,
		gate:       apiGate,
	}
}

// limiter returns the gate this client spaces its requests with, falling back
// to the shared one. A Client built as a bare struct literal — which tests do —
// is therefore still usable rather than a nil dereference waiting to happen.
func (c *Client) limiter() *ratelimit.Gate {
	if c.gate == nil {
		return apiGate
	}
	return c.gate
}

// get performs an API request with the given parameters and decodes the JSON
// response into out.
//
// Requests are spaced by minRequestInterval and retried on 429 and 5xx
// responses. Without this the crawler trips Wikipedia's rate limiter and whole
// list pages get skipped, which silently loses museums.
func (c *Client) get(ctx context.Context, params url.Values, out any) error {
	params.Set("format", "json")
	return c.getFrom(ctx, apiPath+"?"+params.Encode(), out)
}

// getFrom is the retry and rate-limiting loop, taking a full URL so it can be
// exercised against a test server.
func (c *Client) getFrom(ctx context.Context, requestURL string, out any) error {
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
		if err := c.limiter().Wait(ctx); err != nil {
			return err
		}

		retryable, retryAfter, err := c.doRequest(ctx, requestURL, out)
		if err == nil {
			c.limiter().SpeedUp()
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
		c.limiter().SlowDown()

		// The server's own Retry-After beats a guess: backing off for one
		// second when it asked for sixty just spends another attempt.
		if retryAfter > wait {
			wait = retryAfter
		}
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// doRequest issues a single request, reporting whether a failure is worth
// retrying.
func (c *Client) doRequest(ctx context.Context, requestURL string, out any) (retryable bool, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return false, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A cancelled context is deliberate; anything else may be transient.
		return ctx.Err() == nil, 0, fmt.Errorf("call wikipedia api: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return true, ratelimit.RetryAfter(resp.Header.Get("Retry-After")),
			fmt.Errorf("wikipedia api returned %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return false, 0, fmt.Errorf("wikipedia api returned %s", resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, 0, fmt.Errorf("decode wikipedia response: %w", err)
	}
	return false, 0, nil
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
