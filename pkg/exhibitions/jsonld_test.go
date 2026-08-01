package exhibitions

import (
	"testing"
	"time"
)

// tmwDeclaration mirrors the Technisches Museum Wien, the clearest case for
// reading schema.org: nineteen declared events, exact ISO dates, and German
// prose the rest of the scraper has to guess at. Its permanent halls are
// declared as closing in the year 3000.
const tmwDeclaration = `<html><head>
<script type="application/ld+json">
{"@context":"https://schema.org","@graph":[
  {"@type":"ExhibitionEvent","name":"medien.welten",
   "url":"https://www.technischesmuseum.at/ausstellung/medienwelten",
   "startDate":"2020-11-07","endDate":"3000-09-01"},
  {"@type":"ExhibitionEvent","name":"In Bewegung",
   "url":"https://www.technischesmuseum.at/ausstellung/in_bewegung",
   "startDate":"2020-11-09","endDate":"2026-11-29"},
  {"@type":"Organization","name":"Technisches Museum Wien",
   "url":"https://www.technischesmuseum.at/"}
]}
</script></head><body></body></html>`

func TestExtractJSONLDCandidates(t *testing.T) {
	base := mustURL(t, "https://www.technischesmuseum.at/museum/ausstellungen")

	got := ExtractJSONLDCandidates(tmwDeclaration, base)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want the two exhibitions: %+v", len(got), got)
	}

	byTitle := make(map[string]Candidate, len(got))
	for _, c := range got {
		byTitle[c.Title] = c
	}

	running, ok := byTitle["In Bewegung"]
	if !ok {
		t.Fatalf("missing In Bewegung: %+v", byTitle)
	}
	if running.Dates.Start == nil || !running.Dates.Start.Equal(date(2020, time.November, 9)) {
		t.Errorf("Start = %v, want 2020-11-09", running.Dates.Start)
	}
	if running.Dates.End == nil || !running.Dates.End.Equal(date(2026, time.November, 29)) {
		t.Errorf("End = %v, want 2026-11-29", running.Dates.End)
	}

	// The year 3000 is how this site writes "no end", and datesFor is where
	// that is read.
	permanent := byTitle["medien.welten"]
	dates := datesFor(permanent, date(2026, time.August, 1))
	if !dates.Permanent {
		t.Errorf("medien.welten should be permanent, got %+v", dates)
	}
	if dates.End != nil {
		t.Errorf("End = %v, want none", dates.End)
	}
}

// TestExtractJSONLDCandidates_PlainEventsNeedCorroboration keeps the precision
// the HTML reader has: "Event" is what a site uses for concerts and late
// openings as well as exhibitions, so the URL has to agree.
func TestExtractJSONLDCandidates_PlainEventsNeedCorroboration(t *testing.T) {
	const page = `<html><head>
	<script type="application/ld+json">
	[{"@type":"Event","name":"Summer Jazz Night",
	  "url":"https://example.org/tickets/summer-jazz","startDate":"2026-08-14"},
	 {"@type":"Event","name":"Bronze Age Britain",
	  "url":"https://example.org/exhibitions/bronze-age","startDate":"2026-08-14"},
	 {"@type":"Event","name":"Guided tour of the galleries",
	  "url":"https://example.org/exhibitions/guided-tour","startDate":"2026-08-14"}]
	</script></head><body></body></html>`

	got := ExtractJSONLDCandidates(page, mustURL(t, "https://example.org/whats-on"))

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want only the exhibition: %+v", len(got), got)
	}
	if got[0].Title != "Bronze Age Britain" {
		t.Errorf("kept %q", got[0].Title)
	}
}

// TestExtractJSONLDCandidates_LanguageTaggedNames covers the multilingual
// sites this reader exists for, where the name is an object rather than a
// string.
func TestExtractJSONLDCandidates_LanguageTaggedNames(t *testing.T) {
	const page = `<html><head>
	<script type="application/ld+json">
	{"@type":"ExhibitionEvent","name":{"@value":"Wystawa stała","@language":"pl"},
	 "url":"https://www.mnw.art.pl/wystawy/galeria","startDate":"2026-01-01","endDate":"2026-12-31"}
	</script></head><body></body></html>`

	got := ExtractJSONLDCandidates(page, mustURL(t, "https://www.mnw.art.pl/"))
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Title != "Wystawa stała" {
		t.Errorf("Title = %q", got[0].Title)
	}
}

// TestExtractJSONLDCandidates_SkipsOffsiteAndBroken checks that one bad block
// does not cost the good ones beside it.
func TestExtractJSONLDCandidates_SkipsOffsiteAndBroken(t *testing.T) {
	const page = `<html><head>
	<script type="application/ld+json">{ this is not json </script>
	<script type="application/ld+json">
	{"@type":"ExhibitionEvent","name":"Partner show",
	 "url":"https://someone-else.example/exhibitions/partner"}
	</script>
	<script type="application/ld+json">
	{"@type":"ExhibitionEvent","name":"Our show",
	 "url":"https://example.org/exhibitions/ours","startDate":"2026-08-01"}
	</script></head><body></body></html>`

	got := ExtractJSONLDCandidates(page, mustURL(t, "https://example.org/whats-on"))

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want only the museum's own: %+v", len(got), got)
	}
	if got[0].Title != "Our show" {
		t.Errorf("kept %q", got[0].Title)
	}
}

// TestCandidatesOn_DeclaredEventsWinButDoNotReplace covers the merge: sites
// declare a few events and list many, so reading only the declaration loses
// the rest.
func TestCandidatesOn_DeclaredEventsWinButDoNotReplace(t *testing.T) {
	const page = `<html><head>
	<script type="application/ld+json">
	{"@type":"ExhibitionEvent","name":"Declared show",
	 "url":"https://example.org/exhibitions/declared",
	 "startDate":"2026-03-01","endDate":"2026-09-30"}
	</script></head><body>
	  <a href="/exhibitions/declared"><h3>Declared show</h3><p>1 March – 30 September 2026</p></a>
	  <a href="/exhibitions/only-in-html"><h3>Only in HTML</h3><p>Until 4 Oct 2026</p></a>
	</body></html>`

	got := candidatesOn(page, mustURL(t, "https://example.org/exhibitions"), "")

	if len(got) != 2 {
		t.Fatalf("got %d candidates, want both: %+v", len(got), got)
	}
	if got[0].Title != "Declared show" || !got[0].Dates.Known() {
		t.Errorf("the declared event should lead, with its dates: %+v", got[0])
	}
	if got[1].Title != "Only in HTML" {
		t.Errorf("the listed-only entry was lost: %+v", got[1])
	}
}
