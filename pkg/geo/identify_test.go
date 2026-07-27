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
