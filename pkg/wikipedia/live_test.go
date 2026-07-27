package wikipedia

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestExtractLivePages runs the extractor against real Wikipedia list pages
// covering the three shapes these articles come in: a deeply nested bullet list
// grouped by city (France), a flat bullet list with a "See also" section
// (Uruguay), and a sortable wikitable (Ghana).
//
// It asserts that known museums are found and that known noise — the cities the
// French list groups by, the other list pages linked from "See also", and the
// town column of the Ghanaian table — is not.
func TestExtractLivePages(t *testing.T) {
	if testing.Short() {
		t.Skip("hits the live Wikipedia API")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	svc := NewCategoryService(NewClient())
	extractor := NewMuseumExtractor(nil)

	tests := []struct {
		name        string
		page        string
		wantMuseums []string
		wantAbsent  []string
		minMuseums  int
	}{
		{
			name: "nested bullet list grouped by city",
			page: "List of museums in France",
			wantMuseums: []string{
				"Musée du cheminot",
				"Musée dauphinois",
				"Musée Hector Berlioz",
			},
			// The single-star entries are the towns the list groups by.
			wantAbsent: []string{"Ambérieu-en-Bugey", "Bourg-en-Bresse", "Grenoble", "Ain"},
			minMuseums: 200,
		},
		{
			name: "flat bullet list with see-also section",
			page: "List of museums in Uruguay",
			wantMuseums: []string{
				"Museo Torres García",
				"Juan Manuel Blanes Museum",
				"Palacio Taranco",
			},
			wantAbsent: []string{"List of museums in Montevideo", "List of museums by country"},
			minMuseums: 10,
		},
		{
			name: "sortable wikitable",
			page: "List of museums in Ghana",
			wantMuseums: []string{
				"Cape Coast Castle Museum",
				"Bisa Aberwa Museum",
			},
			// Kumasi and Sekondi-Takoradi live in the Location column;
			// the photo column holds File: links.
			wantAbsent: []string{"Kumasi", "Sekondi-Takoradi", "Cape Coast"},
			minMuseums: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := svc.GetPageContent(ctx, tt.page)
			if err != nil {
				t.Fatalf("GetPageContent(%q): %v", tt.page, err)
			}

			extraction := extractor.Extract(content)
			found := make(map[string]string, len(extraction.Candidates))
			for _, c := range extraction.Candidates {
				found[c.Title] = c.Locality
			}

			if len(extraction.Candidates) < tt.minMuseums {
				t.Errorf("extracted %d museums, want at least %d", len(extraction.Candidates), tt.minMuseums)
			}
			for _, want := range tt.wantMuseums {
				if _, ok := found[want]; !ok {
					t.Errorf("missing museum %q", want)
				}
			}
			for _, unwanted := range tt.wantAbsent {
				if _, ok := found[unwanted]; ok {
					t.Errorf("extracted noise %q as a museum", unwanted)
				}
			}

			t.Logf("%s: %d museums, %d nested lists", tt.page, len(extraction.Candidates), len(extraction.NestedLists))
			for i, c := range extraction.Candidates {
				if i >= 8 {
					break
				}
				t.Logf("   %-55s locality=%q", c.Title, c.Locality)
			}
		})
	}
}

// TestResolveLiveMetadata checks that titles resolve to URLs and coordinates,
// that redirects are followed back to the requested title, and that the
// classifier keeps museums while dropping settlements and red links.
func TestResolveLiveMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("hits the live Wikipedia API")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc := NewCategoryService(NewClient())

	const (
		museum     = "Musée d'Orsay"
		settlement = "Kumasi"
		redLink    = "ThisArticleDoesNotExistXYZ123"
	)

	meta, err := svc.ResolveTitles(ctx, []string{museum, settlement, redLink})
	if err != nil {
		t.Fatalf("ResolveTitles: %v", err)
	}

	orsay, ok := meta[museum]
	if !ok {
		t.Fatalf("no metadata returned for %q", museum)
	}
	if !strings.Contains(orsay.URL, "wikipedia.org/wiki/") {
		t.Errorf("URL = %q, want a wikipedia article URL", orsay.URL)
	}
	if !orsay.HasCoordinates {
		t.Errorf("expected coordinates for %q", museum)
	}
	if orsay.Latitude < 48 || orsay.Latitude > 49 || orsay.Longitude < 2 || orsay.Longitude > 3 {
		t.Errorf("coordinates %v,%v are not in Paris", orsay.Latitude, orsay.Longitude)
	}
	if decision, reason := Classify(orsay); decision != Accept {
		t.Errorf("classifier rejected %q: %s", museum, reason)
	}

	if decision, reason := Classify(meta[settlement]); decision != Reject {
		t.Errorf("classifier did not reject the city %q", settlement)
	} else if reason != RejectSettlement {
		t.Errorf("rejected %q as %q, want %q", settlement, reason, RejectSettlement)
	}

	// A red link is kept, but without a URL or coordinates: the museum is real
	// even when the English article is not written yet.
	if decision, _ := Classify(meta[redLink]); decision != AcceptUnverified {
		t.Errorf("classifier did not mark red link %q as unverified", redLink)
	}
}
