package exhibitions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// robotsServer serves one robots.txt as text/plain — the way every correctly
// configured site serves it — and HTML for everything else.
func robotsServer(t *testing.T, robots string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(robots))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>page</body></html>"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetcherHonoursPlainTextRobots is the regression test for robots.txt
// having been unreadable in practice: it was fetched through the same code
// path that rejects any response which is not HTML, so a text/plain robots.txt
// — i.e. every correct one — failed, and a failed read means "nothing is
// forbidden". The rules parsed fine; they were simply never reached.
func TestFetcherHonoursPlainTextRobots(t *testing.T) {
	srv := robotsServer(t, "User-agent: *\nDisallow: /private\n")
	f := NewFetcher()

	if _, _, err := f.Get(context.Background(), srv.URL+"/private/page"); !errors.Is(err, ErrDisallowed) {
		t.Errorf("fetching a disallowed path: got %v, want ErrDisallowed", err)
	}
	if _, _, err := f.Get(context.Background(), srv.URL+"/whats-on"); err != nil {
		t.Errorf("fetching an allowed path: %v", err)
	}
}

func TestParseRobotsPrefersTheGroupNamingThisCrawler(t *testing.T) {
	const body = `
User-agent: *
Disallow: /

User-agent: museum-catalogue
Allow: /
Disallow: /staff
`
	rules := parseRobots(body, "museum-catalogue/1.0 (+https://example.org)")

	// The specific group replaces the wildcard one outright rather than adding
	// to it: a site that wrote rules for this crawler has said what it wants.
	if !rules.allows("/whats-on") {
		t.Error("the wildcard Disallow was applied despite a group naming this crawler")
	}
	if rules.allows("/staff") {
		t.Error("the named group's own Disallow was not applied")
	}
}

func TestParseRobotsMergesRepeatedGroupsAndReadsCrawlDelay(t *testing.T) {
	const body = `
User-agent: *
Disallow: /private
Crawl-delay: 5

User-agent: *
Disallow: /admin
`
	rules := parseRobots(body, "museum-catalogue/1.0")

	if rules.allows("/private/x") || rules.allows("/admin/x") {
		t.Error("rules from a repeated group header were dropped")
	}
	if rules.crawlDelay != 5*time.Second {
		t.Errorf("crawlDelay = %s, want 5s", rules.crawlDelay)
	}
	if got := rules.interval(); got != 5*time.Second {
		t.Errorf("interval() = %s, want the site's own 5s", got)
	}
}

// TestRobotsIntervalNeverGoesBelowTheDefault: a site asking for less than the
// default does not get the crawler to speed up. Crawl-delay is a floor the site
// sets, not a budget it grants.
func TestRobotsIntervalNeverGoesBelowTheDefault(t *testing.T) {
	rules := parseRobots("User-agent: *\nCrawl-delay: 0.1\n", "museum-catalogue/1.0")
	if got := rules.interval(); got != perHostInterval {
		t.Errorf("interval() = %s, want the default %s", got, perHostInterval)
	}
}

func TestFetcherRefusesAnUnservableCrawlDelay(t *testing.T) {
	srv := robotsServer(t, "User-agent: *\nCrawl-delay: 3600\n")

	_, _, err := NewFetcher().Get(context.Background(), srv.URL+"/whats-on")
	if !errors.Is(err, ErrCrawlDelayTooLong) {
		t.Errorf("got %v, want ErrCrawlDelayTooLong", err)
	}
}

// TestRobotsWildcardPatterns covers the two wildcards RFC 9309 defines. A plain
// prefix comparison reads "Disallow: /*.pdf$" as forbidding only paths that
// literally contain an asterisk, which quietly ignores a site's broadest rules.
func TestRobotsWildcardPatterns(t *testing.T) {
	const body = `
User-agent: *
Disallow: /*/private
Disallow: /*.pdf$
Disallow: /search?
Allow: /docs/private/public
`
	rules := parseRobots(body, "museum-catalogue/1.0")

	cases := map[string]bool{
		"/whats-on":               true,
		"/docs/private":           false, // matched through the * wildcard
		"/a/b/private/x":          false,
		"/docs/private/public/ok": true,  // the longer Allow wins
		"/files/guide.pdf":        false, // anchored at the end
		"/files/guide.pdf.html":   true,  // ... so this is not the same path
		"/search?q=x":             false,
		"/private":                true, // no leading segment for the *
	}
	for path, want := range cases {
		if got := rules.allows(path); got != want {
			t.Errorf("allows(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestRobotsEmptyDisallowAllowsEverything: "Disallow:" with no value is the
// explicit "nothing is forbidden", and storing it as a prefix would match every
// path and lock the crawler out of a site that had just opened itself up.
func TestRobotsEmptyDisallowAllowsEverything(t *testing.T) {
	rules := parseRobots("User-agent: *\nDisallow:\n", "museum-catalogue/1.0")
	if !rules.allows("/anything") {
		t.Error("an empty Disallow was read as forbidding everything")
	}
}

// TestRobotsMissingFileAllows: a site with no robots.txt is crawlable, which is
// the convention. The 404 must not be read as a refusal.
func TestRobotsMissingFileAllows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()

	if _, _, err := NewFetcher().Get(context.Background(), srv.URL+"/whats-on"); err != nil {
		t.Errorf("a site with no robots.txt should be crawlable: %v", err)
	}
}
