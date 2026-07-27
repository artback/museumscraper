package exhibitions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultUserAgent identifies the crawler and points at a page explaining
	// it. Museum sites are small operations; being identifiable is the least a
	// scraper owes them. Override with EXHIBITIONS_USER_AGENT.
	DefaultUserAgent = "museum-pipeline/1.0 (+https://github.com/example/museum; exhibition listings)"

	// perHostInterval is the minimum gap between requests to the same site.
	// The crawl is wide rather than deep — a few pages per museum across many
	// museums — so a full second per host costs little and keeps the load on
	// any single site negligible.
	perHostInterval = 1 * time.Second

	// maxBodyBytes caps how much of a page is read. Listing pages are large but
	// not unbounded, and a runaway response should not exhaust memory.
	maxBodyBytes = 4 << 20

	requestTimeout = 20 * time.Second
)

// Fetcher retrieves pages politely: one request per host per second, robots.txt
// respected, redirects followed, bodies capped.
type Fetcher struct {
	client    *http.Client
	userAgent string

	mu     sync.Mutex
	nextAt map[string]time.Time
	robots map[string]*robotsRules
}

// NewFetcher returns a Fetcher ready for use.
func NewFetcher() *Fetcher {
	agent := os.Getenv("EXHIBITIONS_USER_AGENT")
	if agent == "" {
		agent = DefaultUserAgent
	}
	return &Fetcher{
		client:    &http.Client{Timeout: requestTimeout},
		userAgent: agent,
		nextAt:    make(map[string]time.Time),
		robots:    make(map[string]*robotsRules),
	}
}

// Get fetches a page and returns its body as text, along with the URL it ended
// up at after redirects. It refuses URLs that robots.txt disallows.
func (f *Fetcher) Get(ctx context.Context, rawURL string) (body string, finalURL string, err error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parse %q: %w", rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}

	allowed, err := f.allowed(ctx, parsed)
	if err != nil {
		return "", "", err
	}
	if !allowed {
		return "", "", fmt.Errorf("%w: %s", ErrDisallowed, parsed.Path)
	}

	return f.fetch(ctx, parsed)
}

// ErrDisallowed means robots.txt forbids the path.
var ErrDisallowed = fmt.Errorf("disallowed by robots.txt")

// fetch performs the rate-limited request.
func (f *Fetcher) fetch(ctx context.Context, target *url.URL) (string, string, error) {
	if err := f.waitFor(ctx, target.Host); err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en;q=0.9,*;q=0.5")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch %s: %w", target, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch %s: status %s", target, resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		return "", "", fmt.Errorf("fetch %s: content type %s is not HTML", target, ct)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", target, err)
	}

	final := target.String()
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return string(data), final, nil
}

// waitFor blocks until this host may be contacted again.
func (f *Fetcher) waitFor(ctx context.Context, host string) error {
	f.mu.Lock()
	now := time.Now()
	next := f.nextAt[host]
	delay := next.Sub(now)
	if delay < 0 {
		delay = 0
	}
	f.nextAt[host] = now.Add(delay + perHostInterval)
	f.mu.Unlock()

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

// allowed reports whether robots.txt permits fetching the path. A site with no
// robots.txt, or one that cannot be read, is treated as permitting the fetch —
// which is the convention, and the alternative would be to refuse most sites.
func (f *Fetcher) allowed(ctx context.Context, target *url.URL) (bool, error) {
	origin := target.Scheme + "://" + target.Host

	f.mu.Lock()
	rules, known := f.robots[origin]
	f.mu.Unlock()

	if !known {
		robotsURL, err := url.Parse(origin + "/robots.txt")
		if err != nil {
			return true, nil
		}
		body, _, err := f.fetch(ctx, robotsURL)
		if err != nil {
			// No robots.txt, or it is unreachable: nothing forbids the fetch.
			rules = &robotsRules{}
		} else {
			rules = parseRobots(body)
		}

		f.mu.Lock()
		f.robots[origin] = rules
		f.mu.Unlock()
	}

	return rules.allows(target.Path), nil
}

// robotsRules holds the disallow prefixes that apply to this crawler.
type robotsRules struct {
	disallow []string
	allow    []string
}

// allows reports whether path may be fetched. The longest matching rule wins,
// which is how the de-facto standard resolves Allow against Disallow.
func (r *robotsRules) allows(path string) bool {
	if path == "" {
		path = "/"
	}

	longestDisallow, longestAllow := -1, -1
	for _, prefix := range r.disallow {
		if strings.HasPrefix(path, prefix) && len(prefix) > longestDisallow {
			longestDisallow = len(prefix)
		}
	}
	for _, prefix := range r.allow {
		if strings.HasPrefix(path, prefix) && len(prefix) > longestAllow {
			longestAllow = len(prefix)
		}
	}
	return longestAllow >= longestDisallow
}

// parseRobots reads the groups that apply to "*", which is the group this
// crawler falls into.
func parseRobots(body string) *robotsRules {
	rules := &robotsRules{}
	applies := false

	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, "#"); idx != -1 {
			line = strings.TrimSpace(line[:idx])
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimSpace(value)

		switch field {
		case "user-agent":
			applies = value == "*"
		case "disallow":
			if applies && value != "" {
				rules.disallow = append(rules.disallow, value)
			}
		case "allow":
			if applies && value != "" {
				rules.allow = append(rules.allow, value)
			}
		}
	}
	return rules
}
