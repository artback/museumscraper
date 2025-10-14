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
