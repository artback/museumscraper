package exhibitions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"museum/internal/models"
)

// radiomuseetSite reproduces the shape that this feature exists for: a small
// museum's site with no exhibitions page, no calendar and no date anywhere,
// whose museum information page describes rooms full of things on show.
//
// Modelled on radiomuseet.se, whose front page names the information page in
// both its link text and its path, and whose information page says what is on
// show without ever using the word "exhibition".
func radiomuseetSite() *httptest.Server {
	pages := map[string]string{
		"/": `<html><body>
			<a href="/museiinformation/">Museiinformation</a>
			<a href="/foreningen/">Föreningen</a>
			<a href="/medlemskap/">Bli medlem</a>
			<a href="/museibutik/">Försäljning</a>
		</body></html>`,
		"/museiinformation/": `<html><head><title>Museiinformation</title></head><body>
			<h1>Museiinformation</h1>
			<p>Njut av radiohistoria! Här visas teknikhistorien bakom dagens
			ingenjörskonst. Här finns kristallmottagare, amatörradio,
			militärradio och ett bibliotek med handböcker.</p>
		</body></html>`,
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(body))
	}))
}

func TestForMuseum_RecordsTheMuseumItselfWhenItListsNothing(t *testing.T) {
	site := radiomuseetSite()
	defer site.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	found, err := NewScraper().ForMuseum(ctx, models.Museum{
		Name: "Radiomuseet", Website: site.URL,
		Latitude: 57.69, Longitude: 11.98,
	})
	if err != nil {
		t.Fatalf("ForMuseum: %v", err)
	}

	if len(found) != 1 {
		t.Fatalf("got %d entries, want one for the museum itself: %+v", len(found), found)
	}
	entry := found[0]

	if !entry.Permanent {
		t.Error("the entry should be marked permanent")
	}
	if !entry.Running {
		t.Error("a permanent display is on today")
	}
	if entry.Upcoming {
		t.Error("a permanent display is never upcoming")
	}
	if entry.Start != nil || entry.End != nil {
		t.Errorf("permanent display carries dates: start=%v end=%v", entry.Start, entry.End)
	}
	if entry.Title != "Radiomuseet" {
		t.Errorf("Title = %q, want the museum's name", entry.Title)
	}
	if !strings.HasSuffix(entry.URL, "/museiinformation/") {
		t.Errorf("URL = %q, want the page describing the museum", entry.URL)
	}
	if entry.Latitude != 57.69 {
		t.Errorf("Latitude = %v, the venue's position was not carried over", entry.Latitude)
	}
}

// TestForMuseum_NoDisplayNoEntry checks the guard on the fallback: a site with
// no programme and nothing on show anywhere is left alone rather than recorded
// on the strength of having an "/about" page.
func TestForMuseum_NoDisplayNoEntry(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			w.Write([]byte(`<html><body><a href="/about/">About us</a></body></html>`))
		case "/about/":
			w.Write([]byte(`<html><body><h1>About us</h1>
				<p>The society was founded in 1971 and meets on Tuesdays.
				Membership costs 200 a year.</p></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	found, err := NewScraper().ForMuseum(ctx, models.Museum{
		Name: "Radio Society", Website: site.URL,
	})
	if err != nil {
		t.Fatalf("ForMuseum: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("got %d entries, want none: %+v", len(found), found)
	}
}

func TestNamesPermanent(t *testing.T) {
	cases := map[string]bool{
		"Permanent exhibition":               true,
		"Exposition permanente":              true,
		"Dauerausstellung":                   true,
		"Fasta utställningar":                true,
		"Vaste opstelling":                   true,
		"Wystawa stała":                      true,
		"Постоянная экспозиция":              true,
		"Frida: The Making of an Icon":       false,
		"Guided tour":                        false,
		"This gallery is permanently closed": false,
	}

	for text, want := range cases {
		if got := namesPermanent(text); got != want {
			t.Errorf("namesPermanent(%q) = %v, want %v", text, got, want)
		}
	}
}

// TestIsPermanentListing covers the signal that matters most: a page of
// permanent displays says so once, at the top, and its entries then say
// nothing about themselves.
func TestIsPermanentListing(t *testing.T) {
	const page = `<html><head><title>Fasta utställningar | Museet</title></head>
		<body><h1>Fasta utställningar</h1>
		<a href="/utstallningar/bathallen/">Båthallen</a>
		<a href="/utstallningar/kustland/">Kustland</a>
		</body></html>`

	byHeading := mustURL(t, "https://example.org/utstallningar/")
	if !isPermanentListing(page, byHeading) {
		t.Error("a page headed \"Fasta utställningar\" holds permanent displays")
	}

	byPath := mustURL(t, "https://example.org/permanent-exhibitions/")
	if !isPermanentListing(`<html><body><h1>What you can see</h1></body></html>`, byPath) {
		t.Error("a page at /permanent-exhibitions/ holds permanent displays")
	}

	programme := mustURL(t, "https://example.org/whats-on/")
	if isPermanentListing(`<html><head><title>What's on</title></head><body></body></html>`, programme) {
		t.Error("the temporary programme was read as permanent")
	}
}

// TestCandidateIsPermanent_UndatedEntriesStillNeedNaming guards the rule that
// keeps the noise out: without a date, an entry is kept only when something
// says it is permanent.
func TestCandidateIsPermanent_UndatedEntriesStillNeedNaming(t *testing.T) {
	plain := Candidate{Title: "Newsletter", URL: "https://example.org/exhibitions/newsletter"}
	if candidateIsPermanent(plain, false) {
		t.Error("an undated entry with nothing naming it permanent was kept")
	}
	if !candidateIsPermanent(plain, true) {
		t.Error("an entry on a permanent listing page should be permanent")
	}

	byText := Candidate{Title: "Kustland", Context: "Permanent utställning", URL: "https://example.org/x/kustland"}
	if !candidateIsPermanent(byText, false) {
		t.Error("a card labelled permanent should be permanent")
	}

	byPath := Candidate{Title: "Kustland", URL: "https://example.org/dauerausstellung/kustland"}
	if !candidateIsPermanent(byPath, false) {
		t.Error("a URL naming a permanent display should be permanent")
	}
}

// TestFindPermanentLinks_IgnoresHeadlines reproduces the Technisches Museum
// Wien's front page, where a news headline mentioning a permanent exhibition
// led to a magazine article whose every link was then recorded as permanent.
func TestFindPermanentLinks_IgnoresHeadlines(t *testing.T) {
	const page = `<html><body>
	  <a href="/dauerausstellung">Dauerausstellung</a>
	  <a href="/museum/tmw-zine/magazin_detail">Eröffnung der Dauerausstellung „Wissenschaft im Wandel“ mit Gästen</a>
	  <a href="/whats-on">What's on</a>
	</body></html>`

	got := FindPermanentLinks(page, mustURL(t, "https://www.technischesmuseum.at/"))

	if len(got) != 1 {
		t.Fatalf("got %d links, want only the labelled one: %v", len(got), got)
	}
	if !strings.HasSuffix(got[0], "/dauerausstellung") {
		t.Errorf("followed %q instead of the permanent exhibitions page", got[0])
	}
}

// TestFindInfoLinks_RequiresAShallowPath reproduces Moderna Museet, whose
// front page links a guided tour whose slug contains "collection". Matching it
// recorded a tour as the museum's permanent display.
func TestFindInfoLinks_RequiresAShallowPath(t *testing.T) {
	const page = `<html><body>
	  <a href="/en/visit/">Visit</a>
	  <a href="/en/stockholm/events/the-moderna-museet-collection-guided-tour/">The collection: guided tour</a>
	</body></html>`

	got := FindInfoLinks(page, mustURL(t, "https://www.modernamuseet.se/"))

	for _, link := range got {
		if strings.Contains(link, "guided-tour") {
			t.Errorf("followed a deep event page as the museum's own description: %q", link)
		}
	}
	if len(got) == 0 || !strings.HasSuffix(got[0], "/en/visit/") {
		t.Errorf("got %v, want the visit page first", got)
	}
}

func TestDateRange_ResolveOpenEnd(t *testing.T) {
	now := date(2026, time.August, 1)

	// The Technisches Museum Wien closes its permanent halls in the year 3000.
	far := date(3000, time.September, 1)
	opened := date(2020, time.November, 7)
	got := DateRange{Start: &opened, End: &far}.resolveOpenEnd(now)

	if !got.Permanent {
		t.Error("a closing date a thousand years out is not a closing date")
	}
	if got.End != nil {
		t.Errorf("End = %v, want none", got.End)
	}
	if got.Start == nil || !got.Start.Equal(opened) {
		t.Errorf("Start = %v, want the real opening date kept", got.Start)
	}

	// A long but real run is left alone.
	soon := date(2027, time.May, 3)
	if kept := (DateRange{End: &soon}).resolveOpenEnd(now); kept.Permanent || kept.End == nil {
		t.Errorf("a real closing date was discarded: %+v", kept)
	}
}

func TestMachineDates_ReadsISOAttributesInAnyLanguage(t *testing.T) {
	// Danish text the month table cannot read, with the dates marked up for
	// machines beside it.
	const card = `<html><body><article>
	  <a href="/udstilling/wonder/"><h3>Wonder</h3></a>
	  <p><time datetime="2026-02-06">6. februar 2026</time> –
	     <time datetime="2026-08-09">9. august 2026</time></p>
	</article></body></html>`

	got := ExtractCandidates(card, mustURL(t, "https://designmuseum.dk/udstilling"))
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}

	dates := datesFor(got[0], date(2026, time.August, 1))
	if dates.Start == nil || !dates.Start.Equal(date(2026, time.February, 6)) {
		t.Errorf("Start = %v, want 2026-02-06", dates.Start)
	}
	if dates.End == nil || !dates.End.Equal(date(2026, time.August, 9)) {
		t.Errorf("End = %v, want 2026-08-09", dates.End)
	}
}
