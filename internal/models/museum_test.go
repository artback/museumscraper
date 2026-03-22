package models

import (
	"testing"
)

func TestMuseum_StorageKey(t *testing.T) {
	tests := []struct {
		name    string
		museum  Museum
		wantKey string
	}{
		{
			name:    "simple lowercase",
			museum:  Museum{Country: "france", Name: "louvre"},
			wantKey: "raw_data/france/louvre.json",
		},
		{
			name:    "spaces replaced with dashes",
			museum:  Museum{Country: "United States", Name: "National Gallery"},
			wantKey: "raw_data/united-states/national-gallery.json",
		},
		{
			name:    "uppercase converted to lowercase",
			museum:  Museum{Country: "GERMANY", Name: "Berlin Museum"},
			wantKey: "raw_data/germany/berlin-museum.json",
		},
		{
			name:    "mixed case and spaces",
			museum:  Museum{Country: "United Kingdom", Name: "British Museum"},
			wantKey: "raw_data/united-kingdom/british-museum.json",
		},
		{
			name:    "single word",
			museum:  Museum{Country: "Italy", Name: "Uffizi"},
			wantKey: "raw_data/italy/uffizi.json",
		},
		{
			name:    "multiple spaces",
			museum:  Museum{Country: "South Korea", Name: "National Museum of Korea"},
			wantKey: "raw_data/south-korea/national-museum-of-korea.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.museum.StorageKey()
			if got != tt.wantKey {
				t.Errorf("StorageKey() = %q, want %q", got, tt.wantKey)
			}
		})
	}
}

func TestSanitizeKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "lowercase no spaces", input: "paris", want: "paris"},
		{name: "uppercase", input: "PARIS", want: "paris"},
		{name: "mixed case", input: "Paris", want: "paris"},
		{name: "spaces to dashes", input: "new york", want: "new-york"},
		{name: "multiple spaces", input: "los  angeles", want: "los--angeles"},
		{name: "already dashed", input: "new-york", want: "new-york"},
		{name: "empty string", input: "", want: ""},
		{name: "special characters preserved", input: "münchen", want: "münchen"},
		{name: "numbers preserved", input: "museum123", want: "museum123"},
		{name: "trailing space", input: "paris ", want: "paris-"},
		{name: "leading space", input: " paris", want: "-paris"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeKey(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
