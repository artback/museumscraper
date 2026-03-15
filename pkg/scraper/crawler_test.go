package scraper

import (
	"net/url"
	"testing"
)

func TestDiscoverLinks(t *testing.T) {
	html := `<html><body>
	<a href="/exhibitions">Current Exhibitions</a>
	<a href="/visit">Plan Your Visit</a>
	<a href="/tickets">Buy Tickets</a>
	<a href="/about">About Us</a>
	<a href="/blog/random-post">Blog Post</a>
	<a href="/media/schedule.pdf">Download Schedule</a>
	<a href="https://other-site.com/exhibitions">External Link</a>
	<a href="#top">Back to Top</a>
	<a href="javascript:void(0)">JS Link</a>
	</body></html>`

	links := DiscoverLinks(html, "https://museum.example.com")

	// Check we found relevant links
	foundExhibitions := false
	foundVisit := false
	foundTickets := false
	foundPDF := false
	foundExternal := false
	foundAbout := false

	for _, l := range links {
		switch {
		case l.URL == "https://museum.example.com/exhibitions":
			foundExhibitions = true
		case l.URL == "https://museum.example.com/visit":
			foundVisit = true
		case l.URL == "https://museum.example.com/tickets":
			foundTickets = true
		case l.IsPDF:
			foundPDF = true
		case l.URL == "https://other-site.com/exhibitions":
			foundExternal = true
		case l.URL == "https://museum.example.com/about":
			foundAbout = true
		}
	}

	if !foundExhibitions {
		t.Error("should discover /exhibitions link")
	}
	if !foundVisit {
		t.Error("should discover /visit link")
	}
	if !foundTickets {
		t.Error("should discover /tickets link")
	}
	if !foundPDF {
		t.Error("should discover PDF link")
	}
	if foundExternal {
		t.Error("should NOT include external domain links")
	}
	if foundAbout {
		t.Error("should NOT include /about (not a relevant keyword)")
	}
}

func TestDiscoverLinks_DeduplicatesURLs(t *testing.T) {
	html := `<html><body>
	<a href="/exhibitions">Exhibitions</a>
	<a href="/exhibitions">See All Exhibitions</a>
	<a href="/exhibitions#current">Current</a>
	</body></html>`

	links := DiscoverLinks(html, "https://museum.example.com")

	count := 0
	for _, l := range links {
		if l.URL == "https://museum.example.com/exhibitions" {
			count++
		}
	}
	if count > 1 {
		t.Errorf("expected deduplicated URLs, got %d entries for /exhibitions", count)
	}
}

func TestDiscoverLinks_RelativeURLs(t *testing.T) {
	html := `<html><body>
	<a href="exhibitions/current">Current Shows</a>
	<a href="../events">Events</a>
	</body></html>`

	links := DiscoverLinks(html, "https://museum.example.com/en/")

	if len(links) == 0 {
		t.Fatal("expected to find links from relative URLs")
	}

	for _, l := range links {
		if l.URL == "" {
			t.Error("link URL should be resolved, not empty")
		}
		if l.URL[0] != 'h' {
			t.Errorf("link URL should be absolute: %s", l.URL)
		}
	}
}

func TestProbeSubpages(t *testing.T) {
	urls := ProbeSubpages("https://museum.example.com/en/home")

	if len(urls) == 0 {
		t.Fatal("expected probe URLs")
	}

	// All should be on the same origin
	for _, u := range urls {
		if u[:30] != "https://museum.example.com/ex" &&
			u[:30] != "https://museum.example.com/wh" &&
			u[:30] != "https://museum.example.com/vi" &&
			u[:30] != "https://museum.example.com/ti" &&
			u[:30] != "https://museum.example.com/ad" &&
			u[:30] != "https://museum.example.com/op" &&
			u[:30] != "https://museum.example.com/ho" &&
			u[:30] != "https://museum.example.com/ev" &&
			u[:30] != "https://museum.example.com/ca" &&
			u[:30] != "https://museum.example.com/pr" &&
			u[:30] != "https://museum.example.com/on" &&
			u[:30] != "https://museum.example.com/en" {
			// just check it starts with the right origin
			if len(u) < 26 || u[:26] != "https://museum.example.com" {
				t.Errorf("unexpected URL: %s", u)
			}
		}
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		href string
		want string
	}{
		{"absolute", "https://a.com/", "https://a.com/exhibitions", "https://a.com/exhibitions"},
		{"relative", "https://a.com/en/", "exhibitions", "https://a.com/en/exhibitions"},
		{"relative root", "https://a.com/en/", "/exhibitions", "https://a.com/exhibitions"},
		{"fragment only", "https://a.com/", "#top", ""},
		{"javascript", "https://a.com/", "javascript:void(0)", ""},
		{"mailto", "https://a.com/", "mailto:info@a.com", ""},
		{"strips fragment", "https://a.com/", "/page#section", "https://a.com/page"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, _ := parseURL(tt.base)
			got := resolveURL(base, tt.href)
			if got != tt.want {
				t.Errorf("resolveURL(%q, %q) = %q, want %q", tt.base, tt.href, got, tt.want)
			}
		})
	}
}

func TestIsRelevantLink(t *testing.T) {
	tests := []struct {
		url  string
		text string
		want bool
	}{
		{"/exhibitions", "Exhibitions", true},
		{"/visit", "Plan Your Visit", true},
		{"/tickets", "Buy Tickets", true},
		{"/events/calendar", "Event Calendar", true},
		{"/about", "About Us", false},
		{"/contact", "Contact", false},
		{"/blog", "Blog", false},
		{"/program", "Program", true},
		{"/en/collection", "The Collection", true},
	}

	for _, tt := range tests {
		got := isRelevantLink(tt.url, tt.text)
		if got != tt.want {
			t.Errorf("isRelevantLink(%q, %q) = %v, want %v", tt.url, tt.text, got, tt.want)
		}
	}
}

// helper to parse URLs in tests
func parseURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}
