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

	"golang.org/x/net/html/charset"
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

// Validators are the cache tags a site gave for a page last time it was read,
// and are what a conditional request offers back to ask whether anything has
// changed.
//
// Worth carrying because the answer is so much cheaper than the question. A
// site that has not changed replies 304 with no body at all: no transfer, no
// parse, and a definitive "nothing to do" rather than the inference the
// scraper would otherwise have to make by re-reading and re-comparing the
// page. On a sweep of thousands of sites, most of which change a few times a
// year, that is the difference between what can be swept weekly and what
// cannot.
type Validators struct {
	ETag         string
	LastModified string
}

// none reports whether there is nothing to ask the site about.
func (v Validators) none() bool { return v.ETag == "" && v.LastModified == "" }

// Page is a fetched page, or a site's word that it has not changed.
type Page struct {
	Body string
	// URL is where the request ended up after redirects.
	URL string
	// Validators are the tags this response carried, to be offered back next
	// time.
	Validators Validators
	// Unchanged is set when the site answered 304. Body is empty then, and
	// that is an answer rather than a failure.
	Unchanged bool
}

// Get fetches a page and returns its body as text, along with the URL it ended
// up at after redirects. It refuses URLs that robots.txt disallows.
func (f *Fetcher) Get(ctx context.Context, rawURL string) (body string, finalURL string, err error) {
	page, err := f.Fetch(ctx, rawURL, Validators{})
	if err != nil {
		return "", "", err
	}
	return page.Body, page.URL, nil
}

// Fetch retrieves a page, telling the site what we already hold so it can
// answer 304 instead of sending it again.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string, known Validators) (Page, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return Page{}, fmt.Errorf("parse %q: %w", rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return Page{}, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}

	allowed, err := f.allowed(ctx, parsed)
	if err != nil {
		return Page{}, err
	}
	if !allowed {
		return Page{}, fmt.Errorf("%w: %s", ErrDisallowed, parsed.Path)
	}

	return f.get(ctx, parsed, known)
}

// ErrDisallowed means robots.txt forbids the path.
var ErrDisallowed = fmt.Errorf("disallowed by robots.txt")

// fetch performs an unconditional rate-limited request.
func (f *Fetcher) fetch(ctx context.Context, target *url.URL) (string, string, error) {
	page, err := f.get(ctx, target, Validators{})
	if err != nil {
		return "", "", err
	}
	return page.Body, page.URL, nil
}

// get performs the rate-limited request, offering back what we already hold.
func (f *Fetcher) get(ctx context.Context, target *url.URL, known Validators) (Page, error) {
	if err := f.waitFor(ctx, target.Host); err != nil {
		return Page{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Page{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	// English preferred, and measured rather than assumed.
	//
	// A museum's English pages are sometimes an abridgement of its own — the
	// Louvre serves three exhibitions at /en/exhibitions-and-events/exhibitions
	// against fourteen in French — which reads as an argument for taking each
	// site's default instead. Tried across thirty-four museums, it lost far
	// more than it gained: Tate fell from 18 to 13, the Rijksmuseum from 4 to
	// 2, the Kunsthaus from 32 to 25. English listing pages are more
	// consistently structured, and that outweighs the occasional short one.
	req.Header.Set("Accept-Language", "en;q=0.9,*;q=0.5")
	if known.ETag != "" {
		req.Header.Set("If-None-Match", known.ETag)
	}
	if known.LastModified != "" {
		req.Header.Set("If-Modified-Since", known.LastModified)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("fetch %s: %w", target, err)
	}
	defer resp.Body.Close()

	final := target.String()
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}

	// The site's word that nothing has changed. Not an error, and the whole
	// point of having asked.
	if resp.StatusCode == http.StatusNotModified {
		return Page{URL: final, Validators: known, Unchanged: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Page{}, fmt.Errorf("fetch %s: status %s", target, resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		return Page{}, fmt.Errorf("fetch %s: content type %s is not HTML", target, ct)
	}

	// Decode whatever encoding the page is actually in, rather than assuming
	// UTF-8. A good number of museum sites still serve Latin-1 or a Windows
	// code page, and reading those bytes as UTF-8 produces text Postgres will
	// not store at all: one such page failed a batch of 9,148 exhibitions,
	// losing an hour of scraping to a single character. charset.NewReader reads
	// the Content-Type header and the document's own meta tag, and falls back to
	// sniffing, so the accented names come through as themselves.
	body, err := charset.NewReader(io.LimitReader(resp.Body, maxBodyBytes),
		resp.Header.Get("Content-Type"))
	if err != nil {
		// An encoding nothing recognises is not a reason to discard the page;
		// the bytes are still mostly readable, and the storage layer replaces
		// what it cannot accept.
		body = io.LimitReader(resp.Body, maxBodyBytes)
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return Page{}, fmt.Errorf("read %s: %w", target, err)
	}

	return Page{
		Body: string(data),
		URL:  final,
		Validators: Validators{
			ETag:         resp.Header.Get("ETag"),
			LastModified: resp.Header.Get("Last-Modified"),
		},
	}, nil
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
