package exhibitions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html/charset"

	"museum/pkg/useragent"
)

const (
	// perHostInterval is the minimum gap between requests to the same site.
	// The crawl is wide rather than deep — a few pages per museum across many
	// museums — so a full second per host costs little and keeps the load on
	// any single site negligible. A site that states a longer Crawl-delay gets
	// the longer one.
	perHostInterval = 1 * time.Second

	// maxCrawlDelay bounds what a site's Crawl-delay can ask for. Beyond this
	// the host is left alone rather than silently crawled faster than it
	// asked: a site saying "one request per hour" has said no to a sweep of
	// this shape, and honouring the number by holding a worker for an hour
	// would not serve it either.
	maxCrawlDelay = 60 * time.Second

	// maxRobotsBytes caps robots.txt. The file is a few kilobytes on any real
	// site, and one that is not should not be parsed into memory.
	maxRobotsBytes = 512 << 10

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
//
// Museum sites are small operations; being identifiable is the least a scraper
// owes them. The agent carries the contact detail from MUSEUM_CONTACT, and
// EXHIBITIONS_USER_AGENT overrides the whole header.
func NewFetcher() *Fetcher {
	agent := useragent.For("exhibition listings", "EXHIBITIONS_USER_AGENT")
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

	rules, err := f.rulesFor(ctx, parsed)
	if err != nil {
		return Page{}, err
	}
	if !rules.allows(parsed.Path) {
		return Page{}, fmt.Errorf("%w: %s", ErrDisallowed, parsed.Path)
	}
	if rules.crawlDelay > maxCrawlDelay {
		return Page{}, fmt.Errorf("%w: %s asks for %s between requests",
			ErrCrawlDelayTooLong, parsed.Host, rules.crawlDelay)
	}

	return f.get(ctx, parsed, known, rules.interval())
}

// ErrDisallowed means robots.txt forbids the path.
var ErrDisallowed = fmt.Errorf("disallowed by robots.txt")

// ErrCrawlDelayTooLong means the site asked to be crawled more slowly than
// this sweep is willing to go. Distinct from ErrDisallowed because the site did
// not refuse — it named a rate, and the answer is that we cannot keep to it.
var ErrCrawlDelayTooLong = fmt.Errorf("crawl-delay longer than %s", maxCrawlDelay)

// get performs the rate-limited request, offering back what we already hold.
// interval is the minimum gap to leave before contacting this host.
func (f *Fetcher) get(ctx context.Context, target *url.URL, known Validators, interval time.Duration) (Page, error) {
	if err := f.waitFor(ctx, target.Host, interval); err != nil {
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

// waitFor blocks until this host may be contacted again, leaving at least
// interval between one request to it and the next.
func (f *Fetcher) waitFor(ctx context.Context, host string, interval time.Duration) error {
	if interval < perHostInterval {
		interval = perHostInterval
	}

	f.mu.Lock()
	now := time.Now()
	next := f.nextAt[host]
	delay := next.Sub(now)
	if delay < 0 {
		delay = 0
	}
	f.nextAt[host] = now.Add(delay + interval)
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

// rulesFor returns the robots.txt rules that apply to this crawler on the
// target's origin, reading and caching robots.txt the first time an origin is
// seen. A site with no robots.txt, or one that cannot be read, is treated as
// permitting the fetch — which is the convention, and the alternative would be
// to refuse most sites.
func (f *Fetcher) rulesFor(ctx context.Context, target *url.URL) (*robotsRules, error) {
	origin := target.Scheme + "://" + target.Host

	f.mu.Lock()
	rules, known := f.robots[origin]
	f.mu.Unlock()
	if known {
		return rules, nil
	}

	robotsURL, err := url.Parse(origin + "/robots.txt")
	if err != nil {
		return &robotsRules{}, nil
	}

	body, err := f.fetchRobots(ctx, robotsURL)
	if err != nil {
		// A cancelled sweep is not a site's permission to crawl it. Every other
		// failure — no robots.txt, a 500, a timeout — leaves the fetch allowed.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rules = &robotsRules{}
	} else {
		rules = parseRobots(body, f.userAgent)
	}

	f.mu.Lock()
	f.robots[origin] = rules
	f.mu.Unlock()

	return rules, nil
}

// fetchRobots reads a site's robots.txt.
//
// It does not go through get, and the difference is the whole reason this
// function exists: get rejects any response that is not HTML, and robots.txt is
// text/plain on every site that serves it correctly. Fetching it through get
// meant every well-configured site's robots.txt failed to parse, was treated as
// unreadable, and so as permitting everything — the rules were read only from
// the sites that misconfigured them. The parser below was therefore dead code
// against real sites for as long as it has existed.
func (f *Fetcher) fetchRobots(ctx context.Context, target *url.URL) (string, error) {
	if err := f.waitFor(ctx, target.Host, perHostInterval); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/plain,*/*;q=0.5")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", target, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: status %s", target, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRobotsBytes))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", target, err)
	}
	return string(data), nil
}

// robotsRules holds the rules from the robots.txt group that applies to this
// crawler.
type robotsRules struct {
	disallow []string
	allow    []string
	// crawlDelay is the gap the site asked for between requests, zero when it
	// asked for none.
	crawlDelay time.Duration
}

// interval is the gap to leave between requests to this site: the site's own
// figure where it stated one, and the default otherwise.
func (r *robotsRules) interval() time.Duration {
	if r == nil || r.crawlDelay < perHostInterval {
		return perHostInterval
	}
	return r.crawlDelay
}

// allows reports whether path may be fetched. The longest matching rule wins,
// and Allow wins a tie, which is how RFC 9309 resolves the two against each
// other.
func (r *robotsRules) allows(path string) bool {
	if r == nil {
		return true
	}
	if path == "" {
		path = "/"
	}

	longestDisallow, longestAllow := -1, -1
	for _, pattern := range r.disallow {
		if len(pattern) > longestDisallow && robotsMatch(pattern, path) {
			longestDisallow = len(pattern)
		}
	}
	for _, pattern := range r.allow {
		if len(pattern) > longestAllow && robotsMatch(pattern, path) {
			longestAllow = len(pattern)
		}
	}
	return longestAllow >= longestDisallow
}

// robotsMatch reports whether a robots.txt path pattern matches path.
//
// Patterns are prefix matches with two wildcards: "*" stands for any run of
// characters, and a trailing "$" anchors the pattern to the end of the path.
// Both are in RFC 9309 and in wide use — "Disallow: /*?" and "Disallow: /*.pdf$"
// are ordinary lines in an ordinary robots.txt. Matching them literally, as a
// plain prefix comparison does, reads such a rule as forbidding only paths that
// contain an asterisk, so the site's broadest exclusions are the ones most
// likely to be missed.
func robotsMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}

	segments := strings.Split(pattern, "*")
	for i, segment := range segments {
		last := i == len(segments)-1

		if i == 0 {
			// The first segment is a prefix: robots patterns always start at
			// the beginning of the path.
			if !strings.HasPrefix(path, segment) {
				return false
			}
			path = path[len(segment):]
		} else if last && anchored {
			// The final segment of an anchored pattern must land on the end.
			return strings.HasSuffix(path, segment)
		} else {
			index := strings.Index(path, segment)
			if index < 0 {
				return false
			}
			path = path[index+len(segment):]
		}

		if last && anchored && path != "" {
			return false
		}
	}
	return true
}

// parseRobots reads the rules that apply to the given user agent.
//
// Groups are matched by the longest user-agent token that appears in the
// agent string, falling back to the "*" group, which is what RFC 9309 asks for
// and matters more than it sounds: a site that has written a rule naming this
// crawler specifically has gone to the trouble of saying something to us in
// particular, and reading only the "*" group ignores exactly that. Where
// several groups name the same agent their rules are merged, since a robots.txt
// may repeat a group header.
func parseRobots(body, agent string) *robotsRules {
	type group struct {
		agents []string
		rules  robotsRules
	}

	var (
		groups  []*group
		current *group
		// header tracks whether the last line was a user-agent line, so
		// consecutive user-agent lines join one group rather than starting
		// several.
		header bool
	)

	for line := range strings.Lines(body) {
		if idx := strings.IndexByte(line, '#'); idx != -1 {
			line = line[:idx]
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimSpace(value)

		switch field {
		case "user-agent":
			if value == "" {
				continue
			}
			if !header || current == nil {
				current = &group{}
				groups = append(groups, current)
			}
			current.agents = append(current.agents, strings.ToLower(value))
			header = true
			continue
		}

		if current == nil {
			// A rule before any user-agent line belongs to no group.
			continue
		}
		header = false

		switch field {
		case "disallow":
			// An empty Disallow is the explicit "nothing is forbidden" and must
			// not be stored as a prefix, which would match every path.
			if value != "" {
				current.rules.disallow = append(current.rules.disallow, value)
			}
		case "allow":
			if value != "" {
				current.rules.allow = append(current.rules.allow, value)
			}
		case "crawl-delay":
			if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
				current.rules.crawlDelay = time.Duration(seconds * float64(time.Second))
			}
		}
	}

	// Pick the most specific matching agent token across all groups. A token
	// matching nothing scores below "*", which scores below any named match.
	lowered := strings.ToLower(agent)
	best := -1
	for _, g := range groups {
		for _, token := range g.agents {
			switch {
			case token == "*":
				if best < 0 {
					best = 0
				}
			case strings.Contains(lowered, token) && len(token) > best:
				best = len(token)
			}
		}
	}
	if best < 0 {
		return &robotsRules{}
	}

	merged := &robotsRules{}
	for _, g := range groups {
		for _, token := range g.agents {
			matched := (best == 0 && token == "*") ||
				(best > 0 && len(token) == best && token != "*" && strings.Contains(lowered, token))
			if !matched {
				continue
			}
			merged.disallow = append(merged.disallow, g.rules.disallow...)
			merged.allow = append(merged.allow, g.rules.allow...)
			if g.rules.crawlDelay > merged.crawlDelay {
				merged.crawlDelay = g.rules.crawlDelay
			}
			break
		}
	}
	return merged
}
