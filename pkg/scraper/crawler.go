package scraper

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// museumSubpagePaths are well-known URL paths that museum websites commonly
// use for exhibition, visit, and ticket information. The crawler probes these
// when link discovery doesn't find enough data.
var museumSubpagePaths = []string{
	"/exhibitions",
	"/exhibitions/current",
	"/exhibitions/upcoming",
	"/whats-on",
	"/what-s-on",
	"/whatson",
	"/on-view",
	"/visit",
	"/plan-your-visit",
	"/tickets",
	"/admission",
	"/opening-hours",
	"/hours",
	"/events",
	"/calendar",
	"/program",
	"/programme",
	"/en/exhibitions",
	"/en/visit",
	"/en/whats-on",
}

// linkKeywords are URL path/text fragments that indicate exhibition or
// visit-related subpages worth crawling.
var linkKeywords = []string{
	"exhibit", "exposition", "ausstellung", "tentoonstelling",
	"what", "on-view", "on_view",
	"visit", "tickets", "admission", "hours", "opening",
	"event", "calendar", "program", "schedule",
	"collection", "gallery",
	"current", "upcoming", "now-showing",
}

// pdfLinkPattern matches links to PDF files.
var pdfLinkPattern = regexp.MustCompile(`(?i)\.pdf$`)

// DiscoveredLink represents a link found on a museum page.
type DiscoveredLink struct {
	URL   string
	Text  string
	IsPDF bool
}

// DiscoverLinks scans HTML for links to exhibition/visit/ticket subpages
// and PDF documents. It returns unique, same-domain links sorted by relevance.
func DiscoverLinks(rawHTML string, baseURL string) []DiscoveredLink {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}

	var links []DiscoveredLink
	seen := make(map[string]bool)

	tokenizer := html.NewTokenizer(strings.NewReader(rawHTML))
	var inLink bool
	var currentHref string
	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			break
		}

		switch tt {
		case html.StartTagToken:
			token := tokenizer.Token()
			if token.Data == "a" {
				inLink = true
				for _, a := range token.Attr {
					if a.Key == "href" {
						currentHref = a.Val
					}
				}
			}

		case html.TextToken:
			if inLink && currentHref != "" {
				text := strings.TrimSpace(tokenizer.Token().Data)
				resolved := resolveURL(base, currentHref)
				if resolved != "" && !seen[resolved] && isSameDomain(base, resolved) {
					isPDF := pdfLinkPattern.MatchString(resolved)
					if isPDF || isRelevantLink(resolved, text) {
						seen[resolved] = true
						links = append(links, DiscoveredLink{
							URL:   resolved,
							Text:  text,
							IsPDF: isPDF,
						})
					}
				}
			}

		case html.EndTagToken:
			if tokenizer.Token().Data == "a" {
				inLink = false
				currentHref = ""
			}
		}
	}

	return links
}

// ProbeSubpages generates a list of common museum subpage URLs to try
// based on the museum's base URL.
func ProbeSubpages(baseURL string) []string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	// Normalize to just scheme + host
	origin := base.Scheme + "://" + base.Host

	var urls []string
	for _, path := range museumSubpagePaths {
		urls = append(urls, origin+path)
	}
	return urls
}

// isRelevantLink checks if a link URL or text suggests exhibition/visit content.
func isRelevantLink(linkURL, text string) bool {
	lower := strings.ToLower(linkURL + " " + text)
	for _, kw := range linkKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// isSameDomain checks if a resolved URL belongs to the same domain.
func isSameDomain(base *url.URL, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Host == base.Host || u.Host == ""
}

// resolveURL resolves a potentially relative href against a base URL.
func resolveURL(base *url.URL, href string) string {
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") ||
		strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "tel:") {
		return ""
	}

	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}

	resolved := base.ResolveReference(ref)
	// Strip fragment
	resolved.Fragment = ""
	return resolved.String()
}
