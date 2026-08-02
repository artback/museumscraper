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

// Göteborgs stadsmuseum publishes eleven exhibitions under /utstallningar/ and
// labels every one of them "Läs mer". Both halves of that defeated extraction:
// the path hint list knew Norwegian "utstilling" but not Swedish
// "utstallning", so the links scored zero; and the anchor text was taken as the
// title, so all of them would have become one exhibition called "Läs mer".
func TestExtractCandidates_SwedishListingWithReadMoreLinks(t *testing.T) {
	const page = `<html><body>
	  <div class="card"><h3>Vikingr</h3>
	    <a href="/utstallningar/vikingr/">Läs mer</a></div>
	  <div class="card"><h3>Urbanum</h3>
	    <a href="/utstallningar/urbanum/">Läs mer</a></div>
	  <div class="card"><h3>Spåren talar</h3>
	    <a href="/utstallningar/sparen-talar/">Upptäck mer</a></div>
	  <div class="card"><h3>En värld i miniatyr</h3>
	    <a href="/utstallningar/en-varld-i-miniatyr/">Upptäck mer</a></div>
	  <a href="/besok-oss/">Besök oss</a>
	</body></html>`

	base, err := url.Parse("https://goteborgsstadsmuseum.se/utstallningar/")
	if err != nil {
		t.Fatal(err)
	}

	got := ExtractCandidates(page, base)

	titles := make(map[string]string, len(got))
	for _, c := range got {
		titles[c.Title] = c.URL
	}
	if len(got) < 3 {
		t.Fatalf("found %d candidates, want the three exhibitions: %+v", len(got), got)
	}
	// Derived from the slug, which is what a button link falls back to.
	for _, want := range []string{"Vikingr", "Urbanum", "Sparen Talar", "En Varld I Miniatyr"} {
		if _, ok := titles[want]; !ok {
			t.Errorf("missing %q; got %v", want, titles)
		}
	}
	// The button text must never survive as a title, however many cards use it.
	for title := range titles {
		if strings.EqualFold(title, "Läs mer") || strings.EqualFold(title, "Upptäck mer") {
			t.Errorf("%q was kept as an exhibition title", title)
		}
	}
}

// A card that is nothing but a linked photograph still names its exhibition —
// in the URL.
//
// Göteborgs stadsmuseum lists every exhibition this way: an <a> containing an
// <img alt=""> and an empty div, with no text anywhere inside the link. The
// minimum-length check rejected those anchors outright, which meant the slug
// fallback below it could never run for the case it was written for, and the
// museum yielded nothing at all.
func TestExtractCandidates_ReadsCardsThatAreOnlyALinkedImage(t *testing.T) {
	const page = `<html><body>
	  <a class="hero" href="/utstallningar/vikingr/">
	    <div class="background"><img src="/img/v.jpg" alt=""/></div>
	    <div class="content"></div>
	  </a>
	</body></html>`

	got := ExtractCandidates(page, mustURL(t, "https://goteborgsstadsmuseum.se/utstallningar/"))

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Title != "Vikingr" {
		t.Errorf("Title = %q, want the slug, which is the only name on offer", got[0].Title)
	}
}

// An entry one segment below a programme index is an entry, whatever the words
// in its path.
//
// The vocabulary runs out: this museum's own English section is spelled
// /en/exihibitions/, and no list will ever hold that. What stays true is that
// the page is the index and the entries sit under it.
func TestEntryUnder(t *testing.T) {
	cases := []struct {
		section, entry string
		want           bool
		why            string
	}{
		{"/utstallningar/", "https://x.se/utstallningar/vikingr/", true, "an entry below the index"},
		{"/en/exihibitions/", "https://x.se/en/exihibitions/vikingr/", true, "the museum's own spelling"},
		{"/utstallningar/", "https://x.se/utstallningar/", false, "the index is not its own entry"},
		{"/utstallningar/", "https://x.se/utstallningar/tidigare-utstallningar/", false,
			"a sibling index of closed shows, not an entry"},
		{"/utstallningar/", "https://x.se/utstallningar/vikingr/bilder/", false, "two levels down"},
		{"/utstallningar/", "https://x.se/besok-oss/", false, "a different section"},
		{"", "https://x.se/utstallningar/vikingr/", false, "no index means no claim"},
	}

	for _, c := range cases {
		if got := EntryUnder(c.section, c.entry); got != c.want {
			t.Errorf("EntryUnder(%q, %q) = %v, want %v — %s", c.section, c.entry, got, c.want, c.why)
		}
	}
}

// The front page indexes nothing, so it cannot make entries of everything it
// links to.
func TestProgrammeSection(t *testing.T) {
	if got := ProgrammeSection(mustURL(t, "https://x.se/")); got != "" {
		t.Errorf("front page section = %q, want none", got)
	}
	if got := ProgrammeSection(mustURL(t, "https://x.se/en/exihibitions/")); got != "/en/exihibitions/" {
		t.Errorf("section = %q, want the page's own path", got)
	}
}

// A museum's table of contents is not a thing to go and see.
//
// Göteborgs naturhistoriska museum lists /utstallningar/permanenta-utstallningar/
// and /utstallningar/tillfalliga-utstallningar/ beside its actual halls, and
// both were stored as exhibitions — the site's own navigation, offered to a
// visitor as something on show.
func TestSubIndexUnder(t *testing.T) {
	cases := []struct {
		section, entry string
		want           bool
	}{
		{"/utstallningar/", "https://gnm.se/utstallningar/permanenta-utstallningar/", true},
		{"/utstallningar/", "https://gnm.se/utstallningar/tillfalliga-utstallningar/", true},
		{"/utstallningar/", "https://gnm.se/utstallningar/daggdjurssalen/", false},
		// Judged only against the page being read: a site whose single permanent
		// display lives at the top level is naming the display, not indexing it.
		{"/whats-on/", "https://x.org/permanent-exhibition/", false},
	}
	for _, c := range cases {
		if got := SubIndexUnder(c.section, c.entry); got != c.want {
			t.Errorf("SubIndexUnder(%q, %q) = %v, want %v", c.section, c.entry, got, c.want)
		}
	}
}

// An exhibition word in a link outranks a generic programme word.
//
// Kalmar konstmuseum offers "Kalender" at /event/ and "Utställningar" at
// /aktuella-utstallningar/. Scored the same, the order they appeared in the
// navigation decided it, the calendar won, and because the search stops at the
// first page that yields anything the museum's three exhibitions were never
// reached — its programme was read as seven guided tours of shows that were
// themselves missing.
func TestFindListingLinks_PrefersExhibitionsOverTheCalendar(t *testing.T) {
	const home = `<html><body>
	  <a href="/event/">Kalender</a>
	  <a href="/aktuella-utstallningar/">Utställningar</a>
	</body></html>`

	got := FindListingLinks(home, mustURL(t, "https://www.kalmarkonstmuseum.se/"))

	if len(got) == 0 {
		t.Fatal("no listing links found")
	}
	if got[0] != "https://www.kalmarkonstmuseum.se/aktuella-utstallningar/" {
		t.Errorf("first link = %q, want the exhibitions index ahead of the calendar", got[0])
	}
}

func TestVenueScope(t *testing.T) {
	cases := map[string]string{
		// A venue inside a larger institution's site. Göteborgs stadsmuseum
		// publishes one programme at /utstallningar/ and gives Hem i Haga a
		// page; reading from the root gave Hem i Haga all ten of the museum's
		// exhibitions, none of which are specifically there.
		"https://goteborgsstadsmuseum.se/besok-oss/hem-i-haga/":        "/besok-oss/hem-i-haga/",
		"https://goteborgsstadsmuseum.se/besok-oss/lilla-anggarden":    "/besok-oss/lilla-anggarden/",
		"https://www.glasgowlife.org.uk/museums/gallery-of-modern-art": "/museums/gallery-of-modern-art/",
		// The site itself, however it is written.
		"https://www.rijksmuseum.nl":            "",
		"https://www.rijksmuseum.nl/":           "",
		"https://www.rijksmuseum.nl/index.html": "",
		"http://example.org/default.aspx":       "",
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got := venueScope(u); got != want {
			t.Errorf("venueScope(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestWithinScope(t *testing.T) {
	base, _ := url.Parse("https://goteborgsstadsmuseum.se/besok-oss/hem-i-haga/")
	scope := venueScope(base)

	within := []string{
		"https://goteborgsstadsmuseum.se/besok-oss/hem-i-haga/",
		"https://goteborgsstadsmuseum.se/besok-oss/hem-i-haga/utstallningar",
	}
	for _, u := range within {
		if !withinScope(u, base, scope) {
			t.Errorf("withinScope(%q) = false, want true", u)
		}
	}

	outside := []string{
		// The institution's own programme — the exact page that was being
		// attributed to this one venue.
		"https://goteborgsstadsmuseum.se/utstallningar/",
		// A sibling venue.
		"https://goteborgsstadsmuseum.se/besok-oss/lilla-anggarden/",
		// A path that merely starts with the same letters.
		"https://goteborgsstadsmuseum.se/besok-oss/hem-i-haga-butiken/",
		"https://someone-else.example/utstallningar/",
	}
	for _, u := range outside {
		if withinScope(u, base, scope) {
			t.Errorf("withinScope(%q) = true, want false", u)
		}
	}

	// A museum whose website is the site keeps the run of the whole site.
	root, _ := url.Parse("https://www.rijksmuseum.nl/")
	if !withinScope("https://www.rijksmuseum.nl/en/whats-on", root, venueScope(root)) {
		t.Error("an unscoped site should not be restricted")
	}
}

func TestCandidateListingURLs_StayInsideTheVenue(t *testing.T) {
	base, _ := url.Parse("https://goteborgsstadsmuseum.se/besok-oss/hem-i-haga/")
	for _, got := range candidateListingURLs(base, venueScope(base)) {
		if !strings.Contains(got, "/besok-oss/hem-i-haga/") {
			t.Errorf("candidate %q escapes the venue", got)
		}
	}
}
