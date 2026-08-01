package postgres

import (
	"context"
	"museum/internal/models"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"museum/pkg/exhibitions"
)

func TestValidUTF8(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean text is untouched", "Musée d'Orsay", "Musée d'Orsay"},
		{"empty is untouched", "", ""},
		// 0xc1 0x54 is the sequence that failed a batch of 9,148 exhibitions:
		// a Latin-1 page served without declaring its encoding.
		{"the sequence that broke the batch", "Fest\xc1Tival", "Fest�Tival"},
		{"a lone continuation byte", "caf\xe9", "caf�"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validUTF8(tt.in)
			if got != tt.want {
				t.Errorf("validUTF8(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("validUTF8(%q) returned invalid UTF-8", tt.in)
			}
		})
	}
}

// One mis-encoded page must not cost the whole batch. A refresh of 6,000 sites
// found 9,148 exhibitions and stored none of them, because a single title
// carried bytes Postgres would not accept and they were sent as one batch.
func TestSaveExhibitions_SurvivesInvalidUTF8(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	ends := time.Now().AddDate(0, 1, 0)
	batch := []exhibitions.Exhibition{
		{URL: "https://example.org/good", Title: "A Perfectly Fine Exhibition",
			Museum: "Good Museum", End: &ends, Latitude: 48.86, Longitude: 2.35},
		{URL: "https://example.org/bad", Title: "Fest\xc1Tival de la Photographie",
			Museum: "Mis\xe9encoded Museum", End: &ends, Latitude: 48.87, Longitude: 2.36},
		{URL: "https://example.org/also-good", Title: "Another Fine Exhibition",
			Museum: "Good Museum", End: &ends, Latitude: 48.88, Longitude: 2.37},
	}

	written, err := store.SaveExhibitions(ctx, batch)
	if err != nil {
		t.Fatalf("one bad byte sequence failed the whole batch: %v", err)
	}
	if written != 3 {
		t.Errorf("wrote %d exhibitions, want all 3", written)
	}

	found, err := store.ExhibitionsNearby(ctx, 48.87, 2.36, 20, true, 10)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(found) != 3 {
		t.Fatalf("read back %d exhibitions, want 3", len(found))
	}

	// The mis-encoded record is stored, readable, and marked where it was bad
	// rather than silently truncated.
	var repaired bool
	for _, e := range found {
		if !utf8.ValidString(e.Title) || !utf8.ValidString(e.Museum) {
			t.Errorf("stored invalid UTF-8: %q / %q", e.Title, e.Museum)
		}
		if strings.Contains(e.Title, "�") && strings.Contains(e.Title, "Tival de la Photographie") {
			repaired = true
		}
	}
	if !repaired {
		t.Error("the mis-encoded title was not stored with its readable part intact")
	}
}

// One museum with a mis-encoded name must not take the batch down with it.
//
// pgx runs a batch in an implicit transaction, so a single rejected row rolls
// back every row queued alongside it. That is the mechanism that turned one bad
// title into 9,148 lost exhibitions, and SaveMuseums had the same shape: a
// crawl batches 2,000 museums at a time.
func TestSaveMuseums_SurvivesInvalidUTF8(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// 0xC1 0x54 is the byte pair that actually failed a run: Latin-1 text
	// served without declaring its charset.
	bad := string([]byte{0x43, 0xC1, 0x54, 0x65})

	museums := []models.Museum{
		{Name: "Good Museum One", Country: "Sweden", WikidataID: "Q1"},
		{Name: "Mus" + bad + "um", Country: "Sweden", WikidataID: "Q2",
			Description: "desc " + bad, AlsoKnownAs: []string{"alias " + bad}},
		{Name: "Good Museum Two", Country: "Sweden", WikidataID: "Q3"},
	}

	written, err := store.SaveMuseums(ctx, museums)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if written != 3 {
		t.Errorf("written = %d, want all 3 — a bad row must not cost its neighbours", written)
	}

	// The good records either side must be present and unharmed.
	for _, want := range []string{"Good Museum One", "Good Museum Two"} {
		page, err := store.Search(ctx, want, 1, 0)
		if err != nil {
			t.Fatalf("search %q: %v", want, err)
		}
		if len(page.Hits) == 0 || page.Hits[0].Museum.Name != want {
			t.Errorf("%q was lost alongside the mis-encoded record", want)
		}
	}
}
