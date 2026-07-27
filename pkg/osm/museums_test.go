package osm

import (
	"context"
	"testing"
	"time"

	"museum/internal/models"
)

func TestElementToMuseum(t *testing.T) {
	cases := []struct {
		name    string
		element element
		want    func(*testing.T, models.Museum, bool)
	}{
		{
			name: "node with full tags",
			element: element{
				Type: "node", ID: 123, Lat: 48.86, Lon: 2.33,
				Tags: map[string]string{
					"tourism":   "museum",
					"name":      "Musée du Louvre",
					"name:en":   "Louvre Museum",
					"website":   "https://louvre.fr",
					"wikidata":  "Q19675",
					"wikipedia": "en:Louvre",
					"addr:city": "Paris",
				},
			},
			want: func(t *testing.T, museum models.Museum, ok bool) {
				if !ok {
					t.Fatal("expected a museum")
				}
				// The English name is preferred so records line up with the
				// wiki sources.
				if museum.Name != "Louvre Museum" {
					t.Errorf("Name = %q, want the English name", museum.Name)
				}
				if museum.WikidataID != "Q19675" {
					t.Errorf("WikidataID = %q", museum.WikidataID)
				}
				if museum.WikipediaURL != "https://en.wikipedia.org/wiki/Louvre" {
					t.Errorf("WikipediaURL = %q", museum.WikipediaURL)
				}
				if !museum.Verified {
					t.Error("an English article link should mark the record verified")
				}
				if museum.Locality != "Paris" || museum.Website != "https://louvre.fr" {
					t.Errorf("Locality = %q, Website = %q", museum.Locality, museum.Website)
				}
				if museum.Latitude != 48.86 || museum.Longitude != 2.33 {
					t.Errorf("coordinates = (%v, %v)", museum.Latitude, museum.Longitude)
				}
				if museum.SourcePage != "node/123" {
					t.Errorf("SourcePage = %q", museum.SourcePage)
				}
			},
		},
		{
			name: "way uses the computed centre",
			element: func() element {
				e := element{Type: "way", ID: 9, Tags: map[string]string{"name": "Outlined Museum"}}
				e.Center.Lat, e.Center.Lon = 5.10, -1.24
				return e
			}(),
			want: func(t *testing.T, museum models.Museum, ok bool) {
				if !ok {
					t.Fatal("expected a museum")
				}
				// Ways and relations carry no position of their own.
				if museum.Latitude != 5.10 || museum.Longitude != -1.24 {
					t.Errorf("coordinates = (%v, %v), want the centre", museum.Latitude, museum.Longitude)
				}
			},
		},
		{
			name:    "unnamed element is not a catalogue entry",
			element: element{Type: "node", ID: 1, Tags: map[string]string{"tourism": "museum"}},
			want: func(t *testing.T, museum models.Museum, ok bool) {
				if ok {
					t.Error("an unnamed pin should be skipped")
				}
			},
		},
		{
			name: "non-English wikipedia tag is not used",
			element: element{
				Type: "node", ID: 2,
				Tags: map[string]string{"name": "Musée", "wikipedia": "fr:Musée du Louvre"},
			},
			want: func(t *testing.T, museum models.Museum, ok bool) {
				if !ok {
					t.Fatal("expected a museum")
				}
				if museum.WikipediaURL != "" {
					t.Errorf("WikipediaURL = %q, want empty for a non-English link", museum.WikipediaURL)
				}
				if museum.Verified {
					t.Error("a non-English link must not mark the record verified")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			museum, ok := tc.element.toMuseum("France")
			tc.want(t, museum, ok)
		})
	}
}

func TestWikipediaURL(t *testing.T) {
	cases := map[string]string{
		"en:Louvre":          "https://en.wikipedia.org/wiki/Louvre",
		"en:Musée du Louvre": "https://en.wikipedia.org/wiki/Musée_du_Louvre",
		"fr:Musée du Louvre": "",
		"Louvre":             "",
		"":                   "",
		"en:":                "",
	}
	for in, want := range cases {
		if got := wikipediaURL(in); got != want {
			t.Errorf("wikipediaURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCountryMuseumsLive checks the Overpass query against a small country.
func TestCountryMuseumsLive(t *testing.T) {
	if testing.Short() {
		t.Skip("hits the live Overpass API")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	museums, err := NewService(NewClient()).CountryMuseums(ctx, "Andorra", "AD")
	if err != nil {
		t.Fatalf("CountryMuseums: %v", err)
	}
	if len(museums) < 5 {
		t.Errorf("got %d museums for Andorra, expected at least 5", len(museums))
	}

	withCoords := 0
	for _, m := range museums {
		if m.Country != "Andorra" {
			t.Errorf("%s: country = %q", m.Name, m.Country)
		}
		if m.Name == "" {
			t.Error("emitted a museum with no name")
		}
		if m.HasCoordinates() {
			withCoords++
		}
	}
	if withCoords == 0 {
		t.Error("expected at least some museums to carry coordinates")
	}
	t.Logf("Andorra: %d museums, %d with coordinates", len(museums), withCoords)
}
