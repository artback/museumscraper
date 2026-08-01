package exhibitions

import (
	"context"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"museum/internal/models"
)

// tateCard mirrors the markup Tate uses: the anchor wraps an image and a pile
// of card text, and the only clean title is the aria-label. The noscript block
// reproduces the escaped markup that leaked into extracted titles until text
// extraction learned to skip it.
const tateCard = `<html><body>
<a href="/whats-on/tate-modern/frida-kahlo-the-making-of-an-icon"
   aria-label="Frida: The Making of an Icon">
  <div class="card-media">
    <noscript>&lt;img src="https://media.tate.org.uk/aztate-prd-ew-dg-wgtail/images/x.jpg"&gt;</noscript>
  </div>
  <div class="card-text">Exhibition Frida: The Making of an Icon Tate Modern Until 3 Jan 2027 FREE FOR ME</div>
</a>
</body></html>`

// pompidouCard mirrors Centre Pompidou: no aria-label, and the card leads with
// the venue and a type label before the title.
const pompidouCard = `<html><body>
<div class="card">
  <a href="/en/program/calendar/event/eLKK">Grand Palais, Paris Exhibition Hilma af Klint Until 30 Aug 2026</a>
</div>
</body></html>`

// headingCard mirrors sites that mark the title up as a heading.
const headingCard = `<html><body>
<article>
  <a href="/exhibitions/spring-show"><h3>The Spring Show</h3><p>12 March – 7 September 2026</p></a>
</article>
</body></html>`

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestExtractCandidates_UsesAriaLabelAndSkipsNoscript(t *testing.T) {
	got := ExtractCandidates(tateCard, mustURL(t, "https://www.tate.org.uk/whats-on"))

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Title != "Frida: The Making of an Icon" {
		t.Errorf("Title = %q, want the aria-label", got[0].Title)
	}
	if strings.Contains(got[0].Title, "<img") || strings.Contains(got[0].Title, "media.tate") {
		t.Errorf("Title = %q, noscript markup leaked in", got[0].Title)
	}
	if got[0].URL != "https://www.tate.org.uk/whats-on/tate-modern/frida-kahlo-the-making-of-an-icon" {
		t.Errorf("URL = %q", got[0].URL)
	}

	// The dates live in the card text, not in the aria-label.
	dates := datesFor(got[0], time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC))
	if dates.End == nil || !dates.End.Equal(time.Date(2027, time.January, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end date = %v, want 2027-01-03", dates.End)
	}
}

func TestExtractCandidates_StripsVenueAndTypeLabel(t *testing.T) {
	got := ExtractCandidates(pompidouCard, mustURL(t, "https://www.centrepompidou.fr/en/program"))

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Title != "Hilma af Klint" {
		t.Errorf("Title = %q, want the venue and type label removed", got[0].Title)
	}
}

func TestExtractCandidates_UsesHeading(t *testing.T) {
	got := ExtractCandidates(headingCard, mustURL(t, "https://example.org/exhibitions"))

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Title != "The Spring Show" {
		t.Errorf("Title = %q, want the heading text", got[0].Title)
	}
}

func TestExtractCandidates_RejectsNoise(t *testing.T) {
	const page = `<html><body>
	  <a href="/exhibitions">Exhibitions</a>                      <!-- the index itself -->
	  <a href="https://shop.example.com/exhibitions/x">Shop</a>    <!-- off-site -->
	  <a href="/about/our-history">Our history</a>                 <!-- wrong section -->
	  <a href="/exhibitions/view-all">View all</a>                 <!-- navigation -->
	  <a href="/exhibitions/tour">Guided tour of the collection 5 May 2026</a>
	  <a href="/exhibitions/real-one"><h3>A Real Exhibition</h3><p>Until 3 Jan 2027</p></a>
	</body></html>`

	got := ExtractCandidates(page, mustURL(t, "https://example.org/exhibitions"))

	titles := make([]string, 0, len(got))
	for _, c := range got {
		titles = append(titles, c.Title)
	}
	if !slices.Contains(titles, "A Real Exhibition") {
		t.Errorf("titles = %v, want the real exhibition", titles)
	}
	for _, unwanted := range []string{"Exhibitions", "Shop", "Our history", "View all"} {
		if slices.Contains(titles, unwanted) {
			t.Errorf("titles = %v, should not contain %q", titles, unwanted)
		}
	}
	// A guided tour is an event at the museum, not something on show.
	for _, title := range titles {
		if strings.Contains(strings.ToLower(title), "guided tour") {
			t.Errorf("titles = %v, guided tours are not exhibitions", titles)
		}
	}
}

func TestFindListingLinks_PrefersProgrammePages(t *testing.T) {
	const home = `<html><body>
	  <a href="/visit">Visit</a>
	  <a href="/whats-on">What's On</a>
	  <a href="/collection">Collection</a>
	  <a href="/whats-on/tate-modern/some-show">Some Show</a>
	  <a href="/support-us">Support us</a>
	</body></html>`

	got := FindListingLinks(home, mustURL(t, "https://www.tate.org.uk/"))

	if len(got) == 0 {
		t.Fatal("no listing links found")
	}
	if got[0] != "https://www.tate.org.uk/whats-on" {
		t.Errorf("first link = %q, want the shallow programme index first", got[0])
	}
}

func TestResolveURL(t *testing.T) {
	base := mustURL(t, "https://example.org/whats-on")

	cases := map[string]string{
		"/exhibitions/x":          "https://example.org/exhibitions/x",
		"exhibitions/y":           "https://example.org/exhibitions/y",
		"https://other.org/z":     "https://other.org/z",
		"/exhibitions/x#section":  "https://example.org/exhibitions/x",
		"#anchor":                 "",
		"javascript:void(0)":      "",
		"mailto:info@example.org": "",
		"tel:+123":                "",
	}

	for href, want := range cases {
		got, ok := resolveURL(base, href)
		if want == "" {
			if ok {
				t.Errorf("resolveURL(%q) = %q, want rejection", href, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("resolveURL(%q) = %q (ok=%v), want %q", href, got, ok, want)
		}
	}
}

func TestParseRobots(t *testing.T) {
	const body = `
User-agent: BadBot
Disallow: /

User-agent: *
Disallow: /private
Disallow: /admin
Allow: /private/public
`
	rules := parseRobots(body)

	cases := map[string]bool{
		"/whats-on":          true,
		"/private":           false,
		"/private/secret":    false,
		"/private/public/ok": true, // the longer Allow wins
		"/admin/x":           false,
		"/":                  true,
	}
	for path, want := range cases {
		if got := rules.allows(path); got != want {
			t.Errorf("allows(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParseRobots_GroupsForOtherAgentsAreIgnored(t *testing.T) {
	// A blanket Disallow aimed at a different crawler must not apply here.
	rules := parseRobots("User-agent: SomeOtherBot\nDisallow: /\n")
	if !rules.allows("/whats-on") {
		t.Error("a rule for another user-agent was applied to this crawler")
	}
}

// TestForMuseums_CancellationDoesNotLeak checks that stopping early lets every
// worker finish rather than blocking it on an unread result channel.
func TestForMuseums_CancellationDoesNotLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before any work starts

	museums := make([]models.Museum, 20)
	for i := range museums {
		museums[i] = models.Museum{Name: "M", Website: "https://example.invalid/"}
	}

	NewScraper().ForMuseums(ctx, museums, 4)

	// Goroutines wind down asynchronously; allow a moment before comparing.
	for range 50 {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutines grew from %d to %d; workers are stuck", before, runtime.NumGoroutine())
}

// TestExtractCandidates_PrefersExhibitionPathsOverEventPaths reproduces the
// Royal Academy's structure: exhibitions live under /exhibition/, while talks
// and late openings live under /event/ — including one whose slug contains the
// word "exhibition".
func TestExtractCandidates_PrefersExhibitionPathsOverEventPaths(t *testing.T) {
	const page = `<html><body>
	  <a href="/exhibition/flowers-for-you"><h3>Flowers for you</h3><p>Until 4 Oct 2026</p></a>
	  <a href="/exhibition/charlie-billingham"><h3>Charlie Billingham</h3><p>Until 4 Oct 2026</p></a>
	  <a href="/event/summer-exhibition-friday-lates-djs"><h3>Friday Lates</h3><p>Until 20 Nov 2026</p></a>
	  <a href="/event/drop-in-and-draw"><h3>Drop in and draw</h3><p>Until 16 Sep 2026</p></a>
	</body></html>`

	got := ExtractCandidates(page, mustURL(t, "https://www.royalacademy.org.uk/exhibitions"))

	titles := make([]string, 0, len(got))
	for _, c := range got {
		titles = append(titles, c.Title)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want the 2 exhibitions: %v", len(got), titles)
	}
	for _, want := range []string{"Flowers for you", "Charlie Billingham"} {
		if !slices.Contains(titles, want) {
			t.Errorf("titles = %v, want %q", titles, want)
		}
	}
	// "Friday Lates" must not survive on the strength of its slug.
	if slices.Contains(titles, "Friday Lates") {
		t.Errorf("titles = %v, an /event/ entry leaked in via its slug", titles)
	}
}

// TestExtractCandidates_FallsBackToGenericPaths covers Tate, which files
// exhibitions under /whats-on/ and has no exhibition-labelled paths at all.
func TestExtractCandidates_FallsBackToGenericPaths(t *testing.T) {
	const page = `<html><body>
	  <a href="/whats-on/tate-modern/tracey-emin" aria-label="Tracey Emin: A Second Life">
	    <span>Exhibition Tate Modern Until 31 Aug 2026</span>
	  </a>
	</body></html>`

	got := ExtractCandidates(page, mustURL(t, "https://www.tate.org.uk/whats-on"))

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Title != "Tracey Emin: A Second Life" {
		t.Errorf("Title = %q", got[0].Title)
	}
}

func TestUniqueBySite(t *testing.T) {
	// The Musée Charles X is a wing of the Louvre and carries the Louvre's own
	// website, so scraping both yields every Louvre exhibition twice.
	museums := []models.Museum{
		{Name: "Louvre Museum", Website: "https://www.louvre.fr/"},
		{Name: "Musée Charles X", Website: "https://www.louvre.fr/en/some-page"},
		{Name: "Petit Palais", Website: "https://www.petitpalais.paris.fr/"},
		{Name: "No site", Website: ""},
		{Name: "Louvre again", Website: "http://louvre.fr"},
	}

	got := uniqueBySite(museums)

	if len(got) != 2 {
		t.Fatalf("got %d museums, want 2 distinct sites: %+v", len(got), got)
	}
	// The first museum for a site keeps the attribution, so passing them in
	// distance order attributes each exhibition to the nearest venue.
	if got[0].Name != "Louvre Museum" || got[1].Name != "Petit Palais" {
		t.Errorf("kept %q and %q, want the first museum for each site", got[0].Name, got[1].Name)
	}
}

func TestSiteKey(t *testing.T) {
	cases := map[string]string{
		"https://www.louvre.fr/":    "louvre.fr",
		"http://louvre.fr/en/visit": "louvre.fr",
		"https://LOUVRE.FR":         "louvre.fr",
		"https://shop.louvre.fr/":   "shop.louvre.fr",
		"":                          "",
		"not a url":                 "",
	}
	for in, want := range cases {
		if got := siteKey(in); got != want {
			t.Errorf("siteKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExtractCandidates_FallsBackToSlugForButtonLinks covers cards whose link
// is a "Find out more" button: the anchor carries no title, but the URL does.
func TestExtractCandidates_FallsBackToSlugForButtonLinks(t *testing.T) {
	const page = `<html><body>
	  <div class="card">
	    <h3>Not inside the link</h3>
	    <p>Until 6 Sep 2026</p>
	    <a href="/en/exhibitions/event-details/e/kwame-akoto">Find out more</a>
	  </div>
	  <div class="card">
	    <p>Until 20 Sep 2026</p>
	    <a href="/expositions/we-are-still-here">Découvrir</a>
	  </div>
	</body></html>`

	got := ExtractCandidates(page, mustURL(t, "https://quaibranly.fr/en/exhibitions"))

	titles := make([]string, 0, len(got))
	for _, c := range got {
		titles = append(titles, c.Title)
	}
	for _, unwanted := range []string{"Find out more", "Découvrir"} {
		if slices.Contains(titles, unwanted) {
			t.Errorf("titles = %v, a button label was used as a title", titles)
		}
	}
	for _, want := range []string{"Kwame Akoto", "We Are Still Here"} {
		if !slices.Contains(titles, want) {
			t.Errorf("titles = %v, want %q derived from the slug", titles, want)
		}
	}
}

func TestTitleFromSlug(t *testing.T) {
	cases := map[string]string{
		"/expositions/we-are-still-here": "We Are Still Here",
		"/en/exhibitions/e/kwame-akoto":  "Kwame Akoto",
		"/exhibitions/ingres_et_la_mode": "Ingres Et La Mode",
		"/exhibitions/show.html":         "Show",
		"/":                              "",
		"/a":                             "",
	}
	for path, want := range cases {
		if got := titleFromSlug(path); got != want {
			t.Errorf("titleFromSlug(%q) = %q, want %q", path, got, want)
		}
	}
}

// Calendar plugins publish their own paging controls as links. They are not
// exhibitions, and the words on them differ per language, so the test is that
// the shape is recognised rather than the vocabulary.
func TestIsNavigationLink(t *testing.T) {
	base, err := url.Parse("https://www.kalmarkonstmuseum.se/event/")
	if err != nil {
		t.Fatal(err)
	}

	navigation := []string{
		"https://www.kalmarkonstmuseum.se/event/",                      // the page itself
		"https://www.kalmarkonstmuseum.se/event/?eventDisplay=list",    // a view switch
		"https://www.kalmarkonstmuseum.se/event/lista/",                // "Evenemang in Lista View"
		"https://www.kalmarkonstmuseum.se/event/lista/?eventDisplay=1", // "Föregående Evenemang"
		"https://www.kalmarkonstmuseum.se/event/lista/sida/2/",         // "Nästa Evenemang"
		"/event/lista/sida/3/",                                         // relative form
		"https://www.kalmarkonstmuseum.se/event/month/",
		"https://www.kalmarkonstmuseum.se/event/page/4/",
	}
	for _, link := range navigation {
		if !IsNavigationLink(link, base) {
			t.Errorf("%q should be treated as navigation", link)
		}
	}

	entries := []string{
		"https://www.kalmarkonstmuseum.se/event/konstparken/",
		"https://www.kalmarkonstmuseum.se/event/lista/where-is-my-mind/", // a view path then a name
		"https://www.kalmarkonstmuseum.se/utstallningar/grief-trails/",   // elsewhere on the site
		"https://example.org/event/lista/",                               // another site entirely
	}
	for _, link := range entries {
		if IsNavigationLink(link, base) {
			t.Errorf("%q is an entry, not navigation", link)
		}
	}
}
