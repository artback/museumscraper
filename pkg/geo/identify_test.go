package geo

import "testing"

func TestIsCountry(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		expects bool
	}{
		{"exact match", "France", true},
		{"case-insensitive", "gErMaNy", true},
		{"unknown", "Atlantis", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCountry(tc.input); got != tc.expects {
				t.Fatalf("IsCountry(%q) = %v; want %v", tc.input, got, tc.expects)
			}
		})
	}
}

func TestIdentifyPlace(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{"country detected", "Italy", "country"},
		{"city fallback", "Paris", "city"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IdentifyPlace(tc.input); got != tc.expected {
				t.Fatalf("IdentifyPlace(%q) = %q; want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestExtractCountry(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{"in known country", "List of museums in France", "France"},
		{"at known country", "Museums at United States", "United States"},
		{"no preposition", "Museums of Canada", ""},
		{"unknown candidate returned", "Museums in Middle Earth", "Middle Earth"},
		{"trailing spaces trimmed", " Museums in  Spain  ", "Spain"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractCountry(tc.input); got != tc.expected {
				t.Fatalf("ExtractCountry(%q) = %q; want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestCanonical_CollapsesAliases(t *testing.T) {
	cases := map[string]string{
		// Two spellings of one country must resolve to one name, or every
		// consumer sees two countries: records never merge across them, and an
		// audit reports the difference in wording as a contradiction.
		"Czechia":         "Czech Republic",
		"Czech Republic":  "Czech Republic",
		"Cabo Verde":      "Cape Verde",
		"Cape Verde":      "Cape Verde",
		"Côte d'Ivoire":   "Ivory Coast",
		"Ivory Coast":     "Ivory Coast",
		"Holland":         "Netherlands",
		"the Netherlands": "Netherlands",
		"USA":             "United States",
		"Great Britain":   "United Kingdom",
		"Burma":           "Myanmar",
		"Timor-Leste":     "East Timor",
	}

	for in, want := range cases {
		got, ok := Canonical(in)
		if !ok {
			t.Errorf("Canonical(%q) did not resolve", in)
			continue
		}
		if got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonical_AliasesHaveISOCodes(t *testing.T) {
	// Canonical now rewrites some names, so every result it can produce must
	// still resolve to a country code or the OpenStreetMap source loses them.
	for alias := range countryAliases {
		canonical, ok := Canonical(alias)
		if !ok {
			t.Errorf("alias %q does not resolve", alias)
			continue
		}
		if _, ok := ISOCode(canonical); !ok {
			t.Errorf("alias %q resolves to %q, which has no ISO code", alias, canonical)
		}
	}
}

// TestTerritoriesAreRecognised: these have their own ISO code and their own
// museums, and Wikidata attributes museums to them directly rather than to the
// state they belong to. Unrecognised, those records carried a country nothing
// could canonicalise, which excluded them from every check keyed on knowing
// where a museum is.
func TestTerritoriesAreRecognised(t *testing.T) {
	cases := map[string]string{
		"Isle of Man":      "IM",
		"Greenland":        "GL",
		"Jersey":           "JE",
		"Hong Kong":        "HK",
		"Kosovo":           "XK",
		"Puerto Rico":      "PR",
		"Réunion":          "RE",
		"New Caledonia":    "NC",
		"Åland Islands":    "AX",
		"Faroe Islands":    "FO",
		"Cayman Islands":   "KY",
		"French Polynesia": "PF",
	}
	for name, code := range cases {
		if !IsCountry(name) {
			t.Errorf("IsCountry(%q) = false", name)
		}
		if got, ok := ISOCode(name); !ok || got != code {
			t.Errorf("ISOCode(%q) = %q (ok=%v), want %q", name, got, ok, code)
		}
	}
}

// TestTerritoryAliases: the sources spell these several ways, and two
// spellings of one place are two places to everything downstream.
func TestTerritoryAliases(t *testing.T) {
	cases := map[string]string{
		"Macao":             "Macau",
		"Curacao":           "Curaçao",
		"Aland Islands":     "Åland Islands",
		"Reunion":           "Réunion",
		"US Virgin Islands": "United States Virgin Islands",
		"saint barthelemy":  "Saint Barthélemy",
	}
	for spelling, want := range cases {
		if got, ok := Canonical(spelling); !ok || got != want {
			t.Errorf("Canonical(%q) = %q (ok=%v), want %q", spelling, got, ok, want)
		}
	}
}

// TestCrawlAreasCoversBothAndDoesNotRepeat: the OSM crawl iterates this, so a
// duplicate is a duplicate query against a rate-limited public service.
func TestCrawlAreasCoversBothAndDoesNotRepeat(t *testing.T) {
	areas := CrawlAreas()

	seen := make(map[string]bool, len(areas))
	for _, a := range areas {
		if seen[a] {
			t.Errorf("%q appears twice in CrawlAreas", a)
		}
		seen[a] = true
	}
	if !seen["France"] || !seen["Greenland"] {
		t.Error("CrawlAreas must cover countries and territories alike")
	}

	// Every area must resolve to an ISO code, or the crawl silently skips it.
	for _, a := range areas {
		if _, ok := ISOCode(a); !ok {
			t.Errorf("%q has no ISO code, so the OSM crawl will skip it", a)
		}
	}
}

// TestTerritoryNamesDoNotCollideWithSubdivisions: matching is exact, so
// "New Jersey" must not resolve to Jersey.
func TestTerritoryNamesDoNotCollideWithSubdivisions(t *testing.T) {
	for _, place := range []string{"New Jersey", "Georgia, United States", "New Caledonia County"} {
		if _, ok := Canonical(place); ok {
			t.Errorf("Canonical(%q) matched a territory", place)
		}
	}
}
