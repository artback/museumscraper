package quality

import (
	"slices"
	"testing"
	"time"

	"museum/internal/models"
	"museum/pkg/exhibitions"
)

// findingsFor runs the museum checks and returns the findings for one check.
func findingsFor(museums []models.Museum, check string) []Finding {
	var out []Finding
	for _, f := range CheckMuseums(museums).Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func TestCheckCountry_ContradictionsAndFalsePositives(t *testing.T) {
	cases := []struct {
		name    string
		museum  models.Museum
		wantHit bool
	}{
		{
			// The record that motivated this check: Wikidata gives it French
			// coordinates, a French country and an Iranian description.
			name: "genuine contradiction",
			museum: models.Museum{Name: "Treasury of Islamic Heritage", Country: "France",
				Description: "museum in Isfahan Province, Iran"},
			wantHit: true,
		},
		{
			// Two spellings of one country are not a contradiction.
			name: "alias is not a contradiction",
			museum: models.Museum{Name: "Africké muzeum", Country: "Czech Republic",
				Description: "museum in Czechia"},
		},
		{
			// Georgia is a country and a US state; Atlanta is in the state.
			name: "subdivision sharing a country name",
			museum: models.Museum{Name: "APEX Museum", Country: "United States",
				Description: "museum located in Atlanta, Georgia"},
		},
		{
			name: "agreement",
			museum: models.Museum{Name: "Louvre", Country: "France",
				Description: "art museum in Paris, France"},
		},
		{
			name:   "no description to compare",
			museum: models.Museum{Name: "Some Museum", Country: "France"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			museum := tc.museum
			museum.Latitude, museum.Longitude = 48.86, 2.35 // keep other checks quiet

			hits := findingsFor([]models.Museum{museum}, CheckCountryContradicts)
			if got := len(hits) > 0; got != tc.wantHit {
				t.Errorf("flagged = %v, want %v (%v)", got, tc.wantHit, hits)
			}
		})
	}
}

func TestCheckPosition(t *testing.T) {
	cases := []struct {
		name   string
		museum models.Museum
		want   string
	}{
		{name: "missing", museum: models.Museum{Name: "A", Country: "France"}, want: CheckNoCoordinates},
		{
			// A failed parse lands records in the Gulf of Guinea.
			name:   "null island",
			museum: models.Museum{Name: "B", Country: "France", Latitude: 0.0001, Longitude: 0.0001},
			want:   CheckNullIsland,
		},
		{
			name:   "out of range",
			museum: models.Museum{Name: "C", Country: "France", Latitude: 91, Longitude: 2},
			want:   CheckImpossibleCoords,
		},
		{name: "fine", museum: models.Museum{Name: "D", Country: "France", Latitude: 48.86, Longitude: 2.35}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := CheckMuseums([]models.Museum{tc.museum})

			if tc.want == "" {
				for _, f := range report.Findings {
					if f.Check == CheckNoCoordinates || f.Check == CheckNullIsland || f.Check == CheckImpossibleCoords {
						t.Errorf("unexpected finding %+v", f)
					}
				}
				return
			}
			if report.Counts[tc.want] == 0 {
				t.Errorf("expected %s, got %+v", tc.want, report.Findings)
			}
		})
	}
}

func TestCheckName(t *testing.T) {
	cases := []struct {
		name    string
		museum  string
		wantHit bool
	}{
		{name: "leaked wikitext link", museum: "[[Museum of Art]]", wantHit: true},
		{name: "leaked template", museum: "{{Infobox museum}}", wantHit: true},
		{name: "leaked html tag", museum: "<b>Museum</b>", wantHit: true},
		{name: "leaked entity", museum: "Art &amp; Design Museum", wantHit: true},
		{name: "empty", museum: "   ", wantHit: true},

		// Every one of these is a real museum in the catalogue. A minimum
		// length, a "must contain a letter" rule and a bare-pipe check flagged
		// all of them and found nothing else — the names are simply unusual.
		{name: "M+ in Hong Kong", museum: "M+"},
		{name: "W5 in Belfast", museum: "W5"},
		{name: "70.8 in Liverpool", museum: "70.8"},
		{name: "pipe as a separator", museum: "Kunstmuseum Winterthur | Beim Stadthaus"},
		{name: "bilingual South Tyrolean name", museum: "Museum Steinegg|Collepietra"},
		{name: "ampersand in a name", museum: "Brot & Mühle | Museum | Kunst"},
		{name: "ordinary", museum: "Perfectly Normal Museum"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			museums := []models.Museum{
				{Name: tc.museum, Country: "Germany", Latitude: 52.5, Longitude: 13.4},
			}
			hits := findingsFor(museums, CheckSuspiciousName)

			if got := len(hits) > 0; got != tc.wantHit {
				t.Errorf("flagged %q = %v, want %v", tc.museum, got, tc.wantHit)
			}
		})
	}
}

func TestCheckClasses(t *testing.T) {
	found := findingsFor([]models.Museum{
		// Classified: nothing to report.
		{Name: "S/S Bohuslän", Country: "Sweden", Sources: []string{"wikidata"},
			Classes: []string{"steamboat"}},
		// From the source that states classes, but carrying none. This is the
		// one worth counting: a class query failing for a whole country looks
		// exactly like this, and nothing else would notice.
		{Name: "Unclassified Museum", Country: "Sweden", Sources: []string{"wikidata"}},
		// Sources with no notion of class at all. Counting these would report
		// the pipeline's shape as a fault and bury the signal above.
		{Name: "From A List", Country: "Sweden", Sources: []string{"lists"}},
		{Name: "From OpenStreetMap", Country: "Sweden", Sources: []string{"osm"}},
	}, CheckUnclassified)

	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(found), found)
	}
	if found[0].Subject != "Unclassified Museum" {
		t.Errorf("subject = %q", found[0].Subject)
	}
	// Info, not Warning: an unclassified museum is not a wrong record, and
	// -fail-on warning must not start failing because Wikidata is patchy.
	if found[0].Severity != Info {
		t.Errorf("severity = %v, want Info", found[0].Severity)
	}
}

func TestCheckDuplicates(t *testing.T) {
	museums := []models.Museum{
		{Name: "Art 42", Country: "France", Latitude: 48.8, Longitude: 2.3},
		{Name: "Art42", Country: "France", Latitude: 48.8, Longitude: 2.3},
		{Name: "Art42", Country: "Germany", Latitude: 52.5, Longitude: 13.4},
	}

	hits := findingsFor(museums, CheckDuplicate)
	if len(hits) != 1 {
		t.Fatalf("got %d duplicate findings, want 1: %+v", len(hits), hits)
	}
	// The German one shares a name but not a country, so it is a different
	// museum.
	if got := CheckMuseums(museums).Counts[CheckDuplicate]; got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}

func TestCheckURLs(t *testing.T) {
	museums := []models.Museum{
		{Name: "A", Country: "France", Latitude: 48.8, Longitude: 2.3, Website: "not a url"},
		{Name: "B", Country: "France", Latitude: 48.8, Longitude: 2.3, Website: "ftp://example.org"},
		{Name: "C", Country: "France", Latitude: 48.8, Longitude: 2.3, Website: "https://example.org"},
	}

	hits := findingsFor(museums, CheckBadURL)
	if len(hits) != 2 {
		t.Errorf("got %d unusable URLs, want 2: %+v", len(hits), hits)
	}
}

func TestCheckExhibitions(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	past := now.AddDate(0, -1, 0)
	future := now.AddDate(0, 1, 0)
	distant := now.AddDate(40, 0, 0)
	old := now.AddDate(0, -3, 0)

	found := []exhibitions.Exhibition{
		{Title: "Backwards", Start: &future, End: &past, ScrapedAt: now},
		{Title: "Far future", Start: &distant, ScrapedAt: now},
		{Title: "Find out more", End: &future, ScrapedAt: now},
		{Title: "Stale", End: &future, ScrapedAt: old},
		{Title: "Perfectly fine", Start: &past, End: &future, ScrapedAt: now},
	}

	report := CheckExhibitions(found, now)

	for check, want := range map[string]int{
		CheckEndBeforeStart:   1,
		CheckImplausibleDates: 1,
		CheckBoilerplateTitle: 1,
		CheckStaleScrape:      1,
	} {
		if got := report.Counts[check]; got != want {
			t.Errorf("%s = %d, want %d", check, got, want)
		}
	}
}

func TestReport_SeverityCounts(t *testing.T) {
	report := Report{}
	report.add(Finding{Check: "a", Severity: Error})
	report.add(Finding{Check: "b", Severity: Warning})
	report.add(Finding{Check: "c", Severity: Info})

	if report.Errors() != 1 {
		t.Errorf("Errors = %d, want 1", report.Errors())
	}
	if report.Warnings() != 1 {
		t.Errorf("Warnings = %d, want 1", report.Warnings())
	}
}

func TestCheckNotAMuseum(t *testing.T) {
	found := findingsFor([]models.Museum{
		// The contamination: hall-of-fame inductees admitted as museums.
		{Name: "Pat O'Dea", Description: "Australian rules footballer"},
		{Name: "Clifford", Description: "American-bred Thoroughbred racehorse"},
		{Name: "Dead Man's Curve", Description: "1963 single by Jan and Dean"},
		{Name: "Assis", Description: "railway station in Assis, Brazil"},
		// Real museums that must survive. Each would match a descriptor if the
		// museum keywords were not checked first: the football museum is about
		// players, the racing museum about racehorses, and Billy Sunday's house
		// is described by way of the baseball player who lived in it.
		{Name: "National Football Museum", Description: "football museum in Manchester"},
		{Name: "Billy Sunday Historic Home",
			Description: "historic house of baseball player Billy Sunday in Kosciusko County"},
		{Name: "National Museum of Racing and Hall of Fame",
			Description: "Thoroughbred racehorse museum in Saratoga Springs"},
		// No description is not evidence of anything.
		{Name: "Palacio Taranco"},
	}, CheckNotAMuseum)

	var flagged []string
	for _, f := range found {
		flagged = append(flagged, f.Subject)
	}
	want := []string{"Pat O'Dea", "Clifford", "Dead Man's Curve", "Assis"}
	if len(flagged) != len(want) {
		t.Fatalf("flagged %v, want exactly %v", flagged, want)
	}
	for _, subject := range want {
		if !slices.Contains(flagged, subject) {
			t.Errorf("%q was not flagged; flagged %v", subject, flagged)
		}
	}
}
