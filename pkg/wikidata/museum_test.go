package wikidata

import "testing"

func TestExtractMuseumDetails(t *testing.T) {
	tests := []struct {
		name      string
		extratags map[string]string
		wantNil   bool
		checkFn   func(t *testing.T, d *MuseumDetails)
	}{
		{
			name:    "nil extratags",
			wantNil: true,
		},
		{
			name:      "empty extratags",
			extratags: map[string]string{},
			wantNil:   true,
		},
		{
			name: "only irrelevant tags",
			extratags: map[string]string{
				"building": "yes",
				"source":   "survey",
			},
			wantNil: true,
		},
		{
			name: "full museum details",
			extratags: map[string]string{
				"opening_hours":   "Mo-Fr 09:00-17:00; Sa-Su 10:00-18:00",
				"fee":             "yes",
				"charge":          "15 EUR",
				"website":         "https://www.louvre.fr",
				"phone":           "+33 1 40 20 50 50",
				"email":           "info@louvre.fr",
				"wheelchair":      "yes",
				"description:en":  "The world's largest art museum",
			},
			checkFn: func(t *testing.T, d *MuseumDetails) {
				if d.OpeningHours != "Mo-Fr 09:00-17:00; Sa-Su 10:00-18:00" {
					t.Errorf("OpeningHours = %q", d.OpeningHours)
				}
				if d.Admission != "yes" { // "fee" takes priority over "charge"
					t.Errorf("Admission = %q, want 'yes'", d.Admission)
				}
				if d.Website != "https://www.louvre.fr" {
					t.Errorf("Website = %q", d.Website)
				}
				if d.Phone != "+33 1 40 20 50 50" {
					t.Errorf("Phone = %q", d.Phone)
				}
				if d.Email != "info@louvre.fr" {
					t.Errorf("Email = %q", d.Email)
				}
				if d.Wheelchair != "yes" {
					t.Errorf("Wheelchair = %q", d.Wheelchair)
				}
				if d.Description != "The world's largest art museum" {
					t.Errorf("Description = %q", d.Description)
				}
			},
		},
		{
			name: "contact prefixed fields",
			extratags: map[string]string{
				"contact:website": "https://museum.example.com",
				"contact:phone":   "+1 555-0100",
				"contact:email":   "hello@museum.example.com",
			},
			checkFn: func(t *testing.T, d *MuseumDetails) {
				if d.Website != "https://museum.example.com" {
					t.Errorf("Website = %q", d.Website)
				}
				if d.Phone != "+1 555-0100" {
					t.Errorf("Phone = %q", d.Phone)
				}
				if d.Email != "hello@museum.example.com" {
					t.Errorf("Email = %q", d.Email)
				}
			},
		},
		{
			name: "opening hours only",
			extratags: map[string]string{
				"opening_hours": "24/7",
			},
			checkFn: func(t *testing.T, d *MuseumDetails) {
				if d.OpeningHours != "24/7" {
					t.Errorf("OpeningHours = %q", d.OpeningHours)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractMuseumDetails(tt.extratags)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil MuseumDetails")
			}
			if tt.checkFn != nil {
				tt.checkFn(t, got)
			}
		})
	}
}

func TestTruncateDate(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"2024-01-15T00:00:00Z", "2024-01-15"},
		{"2024-06-30", "2024-06-30"},
		{"2024", "2024"},
		{"", ""},
	}
	for _, tt := range tests {
		got := truncateDate(tt.input)
		if got != tt.want {
			t.Errorf("truncateDate(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractQID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://www.wikidata.org/entity/Q19675", "Q19675"},
		{"Q19675", "Q19675"},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractQID(tt.input)
		if got != tt.want {
			t.Errorf("extractQID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
