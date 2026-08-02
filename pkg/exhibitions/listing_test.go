package exhibitions

import (
	"strings"
	"testing"
	"time"
)

// TestTitleFromSlug_DropsTheOccurrenceCounter is what makes the recurring-event
// merge work. Hasselblad Center publishes one weekly introduction as seven
// pages; the counter made seven names out of one, so nothing merged and the
// museum listed the same event seven times.
func TestTitleFromSlug_DropsTheOccurrenceCounter(t *testing.T) {
	cases := map[string]string{
		"/en/event/introduction-to-the-exhibition-women-behind-the-camera-3/": "Introduction To The Exhibition Women Behind The Camera",
		"/en/event/introduction-to-the-exhibition-women-behind-the-camera-9/": "Introduction To The Exhibition Women Behind The Camera",
		// A year is not a counter.
		"/exhibitions/summer-show-2026/": "Summer Show 2026",
		// Nor is a number that is most of the name.
		"/exhibitions/documenta-14/": "Documenta 14",
	}
	for path, want := range cases {
		if got := titleFromSlug(path); got != want {
			t.Errorf("titleFromSlug(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestParseDateRange_QualifierMustBeNextToTheDate reproduces the Hasselblad
// archive leak: "From the Hasselblad Foundation Collection" was read as the
// date qualifier "from", so a run that ended in 2013 became one that started
// then and never closed.
func TestParseDateRange_QualifierMustBeNextToTheDate(t *testing.T) {
	now := date(2026, time.August, 2)

	got := ParseDateRange("From the Hasselblad Foundation Collection March 23 – May 19, 2013", now)
	if got.End == nil {
		t.Fatalf("expected a closing date, got %+v", got)
	}
	if got.Runs(now) {
		t.Error("a run that ended in 2013 is not on today")
	}

	// A qualifier that really does belong to the date still works.
	if open := ParseDateRange("From 5 March 2026", now); open.Start == nil || open.End != nil {
		t.Errorf("From 5 March 2026 = %+v, want an open-ended start", open)
	}
	if until := ParseDateRange("Until 3 Jan 2027", now); until.End == nil || until.Start != nil {
		t.Errorf("Until 3 Jan 2027 = %+v, want an open-ended close", until)
	}
}

// TestParseDateRange_PermanentKeepsItsOpeningDate: the opening date is what
// makes a permanence claim the entry's own rather than borrowed from the page
// around it.
func TestParseDateRange_PermanentKeepsItsOpeningDate(t *testing.T) {
	now := date(2026, time.August, 2)

	got := ParseDateRange("Permanent exhibition Opens May 23 2026", now)
	if !got.Permanent {
		t.Fatal("should be permanent")
	}
	if got.Start == nil || !got.Start.Equal(date(2026, time.May, 23)) {
		t.Errorf("Start = %v, want 2026-05-23", got.Start)
	}
	if got.End != nil {
		t.Errorf("End = %v, want none", got.End)
	}
}

// TestExtractCandidates_FlatPaths covers sites that do not nest. The Jewish
// Museum Berlin publishes "/ausstellung-gegenteil-von-jetzt" at the root, so
// there is no directory above it to classify it by, and every exhibition was
// rejected.
func TestExtractCandidates_FlatPaths(t *testing.T) {
	const page = `<html><head><title>Aktuelle Ausstellungen</title></head><body>
	  <a href="/ausstellung-gegenteil-von-jetzt">Das Gegenteil von Jetzt</a>
	  <p>4. September 2026 – 10. Januar 2027</p>
	  <a href="/spenden">Spenden</a>
	  <a href="/mitglied-werden">Mitglied werden</a>
	</body></html>`

	got := ExtractCandidates(page, mustURL(t, "https://www.jmberlin.de/aktuelle-ausstellungen"))

	titles := map[string]bool{}
	for _, c := range got {
		titles[c.Title] = true
	}
	if !titles["Das Gegenteil von Jetzt"] {
		t.Errorf("the exhibition was rejected for having a flat path: %v", titles)
	}
	// A flat path is judged by its slug, and only a strong word admits it.
	if titles["Spenden"] || titles["Mitglied werden"] {
		t.Errorf("navigation was admitted alongside it: %v", titles)
	}
}

// TestTextOf_StripsSoftHyphens guards every word list in the package. German
// sites hyphenate with U+00AD, and "Alle ver­gangenen Aus­stel­lun­gen" matched
// nothing at all, so the archive was recorded as a permanent exhibition.
func TestTextOf_StripsSoftHyphens(t *testing.T) {
	const page = "<html><body><a href=\"/x\">Alle ver­gangenen Aus­stel­lun­gen</a></body></html>"

	got := ExtractCandidates(page, mustURL(t, "https://www.jmberlin.de/aktuelle-ausstellungen"))
	for _, c := range got {
		if strings.Contains(c.Title, "­") {
			t.Errorf("a soft hyphen survived into %q", c.Title)
		}
	}
	if len(got) != 0 {
		t.Errorf("the archive link was kept: %+v", got)
	}
}

// TestCleanTitle_TypeLabelIsAWholeWord: cutting inside a word turned
// "Ausstellungsarchiv" into "sarchiv".
func TestCleanTitle_TypeLabelIsAWholeWord(t *testing.T) {
	if got := cleanTitle("Ausstellungsarchiv"); got != "Ausstellungsarchiv" {
		t.Errorf("cleanTitle(Ausstellungsarchiv) = %q, want it left alone", got)
	}
	// A label that really is one still comes off.
	if got := cleanTitle("Exhibition Hilma af Klint"); got != "Hilma af Klint" {
		t.Errorf("cleanTitle = %q, want the label removed", got)
	}
}

// TestCleanTitle_StripsLinkAffordances: titleOf prefers the accessible label,
// which often says what the link does before naming the thing, so every hall
// at the Vasamuseet was called "Gå till sidan …".
func TestCleanTitle_StripsLinkAffordances(t *testing.T) {
	if got := cleanTitle("Gå till sidan Kungens skepp"); got != "Kungens skepp" {
		t.Errorf("cleanTitle = %q, want the affordance removed", got)
	}
	if got := cleanTitle("Go to page Bronze Age Britain"); got != "Bronze Age Britain" {
		t.Errorf("cleanTitle = %q", got)
	}
}

// TestFindListingLinks_PrefersCurrentOverPast: an archive can only yield
// finished runs, every one of which is then discarded, so it must never take
// one of the three pages a museum gets.
func TestFindListingLinks_PrefersCurrentOverPast(t *testing.T) {
	const page = `<html><body>
	  <a href="/en/tidigare-utstallningar/">Past Exhibitions</a>
	  <a href="/en/calendar2/">Calendar</a>
	  <a href="/en/exhibition-current/">Current Exhibitions</a>
	</body></html>`

	got := FindListingLinks(page, mustURL(t, "https://www.hasselbladfoundation.org/"))

	if len(got) == 0 {
		t.Fatal("no listing links found")
	}
	if !strings.HasSuffix(got[0], "/en/exhibition-current/") {
		t.Errorf("first choice is %q, want the current exhibitions page", got[0])
	}
	for _, link := range got {
		if strings.Contains(link, "tidigare") {
			t.Errorf("the archive was offered at all: %q", link)
		}
	}
}

// TestNamesExhibitions_OnlyIndexPages: an exhibition's own page sits under the
// same word its index does, and the Mucem's "/expositions/mossi/" was read as
// a listing — so its eight real exhibitions, one directory up, were missed.
func TestNamesExhibitions_OnlyIndexPages(t *testing.T) {
	const blank = `<html><head><title>x</title></head><body></body></html>`

	if !namesExhibitions(blank, mustURL(t, "https://www.hasselbladfoundation.org/en/exhibition-current/")) {
		t.Error("an index should name itself")
	}
	if namesExhibitions(blank, mustURL(t, "https://mucem.org/expositions/mossi/")) {
		t.Error("an exhibition's own page is not an index")
	}
}
