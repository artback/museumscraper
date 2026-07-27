package wikidata

import (
	"testing"
)

func TestParsePoint(t *testing.T) {
	cases := []struct {
		name    string
		literal string
		wantLat float64
		wantLon float64
		wantOK  bool
	}{
		// Wikidata writes WKT, which puts longitude first. Reading them the
		// other way round moves every museum to the wrong hemisphere.
		{name: "paris", literal: "Point(2.3380277 48.8611473)", wantLat: 48.8611473, wantLon: 2.3380277, wantOK: true},
		{name: "negative longitude", literal: "Point(-1.2414 5.1036)", wantLat: 5.1036, wantLon: -1.2414, wantOK: true},
		{name: "padded", literal: "  Point( 7.826702  45.764066 )  ", wantLat: 45.764066, wantLon: 7.826702, wantOK: true},
		{name: "empty", literal: "", wantOK: false},
		{name: "not a point", literal: "48.86,2.33", wantOK: false},
		{name: "malformed", literal: "Point(abc def)", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lat, lon, ok := parsePoint(tc.literal)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if lat != tc.wantLat || lon != tc.wantLon {
				t.Errorf("got (%v, %v), want (%v, %v)", lat, lon, tc.wantLat, tc.wantLon)
			}
		})
	}
}

func TestEntityID(t *testing.T) {
	cases := map[string]string{
		"http://www.wikidata.org/entity/Q23402": "Q23402",
		"Q23402":                                "Q23402",
		"":                                      "",
	}
	for in, want := range cases {
		if got := entityID(in); got != want {
			t.Errorf("entityID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMuseumsFromRows_CollapsesRepeatedEntities(t *testing.T) {
	// Optional joins multiply rows: a museum with two locality statements and
	// two websites comes back as several rows that must fold into one museum.
	rows := []binding{
		{
			"item":          "http://www.wikidata.org/entity/Q23402",
			"itemLabel":     "Musée d'Orsay",
			"desc":          "Art museum in Paris, France",
			"coord":         "Point(2.326527 48.859972)",
			"localityLabel": "Paris",
			"countryLabel":  "France",
			"article":       "https://en.wikipedia.org/wiki/Mus%C3%A9e_d%27Orsay",
		},
		{
			"item":          "http://www.wikidata.org/entity/Q23402",
			"itemLabel":     "Musée d'Orsay",
			"localityLabel": "7th arrondissement of Paris",
			"website":       "https://www.musee-orsay.fr/",
		},
		{
			"item":         "http://www.wikidata.org/entity/Q1",
			"itemLabel":    "Second Museum",
			"countryLabel": "France",
		},
	}

	museums, entities := museumsFromRows(rows, "France")

	if len(museums) != 2 {
		t.Fatalf("got %d museums, want 2: %+v", len(museums), museums)
	}
	if entities != 2 {
		t.Errorf("entities = %d, want 2", entities)
	}

	orsay := museums[0]
	if orsay.WikidataID != "Q23402" {
		t.Errorf("WikidataID = %q", orsay.WikidataID)
	}
	if orsay.Locality != "Paris" {
		t.Errorf("Locality = %q, want the first statement to win", orsay.Locality)
	}
	if orsay.Website != "https://www.musee-orsay.fr/" {
		t.Errorf("Website = %q, want it filled from the later row", orsay.Website)
	}
	if orsay.Latitude != 48.859972 || orsay.Longitude != 2.326527 {
		t.Errorf("coordinates = (%v, %v)", orsay.Latitude, orsay.Longitude)
	}
	if !orsay.Verified {
		t.Error("Verified should be true when an English article exists")
	}
	if len(orsay.Sources) != 1 || orsay.Sources[0] != SourceName {
		t.Errorf("Sources = %v", orsay.Sources)
	}

	if museums[1].Verified {
		t.Error("a museum with no article should not be marked verified")
	}
}

func TestMuseumsFromRows_SkipsUnlabelledEntities(t *testing.T) {
	// The label service echoes the bare Q-id when no label exists in any
	// requested language. Such a record has no usable name.
	rows := []binding{
		{"item": "http://www.wikidata.org/entity/Q999", "itemLabel": "Q999", "countryLabel": "France"},
		{"item": "http://www.wikidata.org/entity/Q998"},
		{"item": "http://www.wikidata.org/entity/Q997", "itemLabel": "Real Museum"},
	}

	museums, entities := museumsFromRows(rows, "France")

	if len(museums) != 1 {
		t.Fatalf("got %d museums, want 1: %+v", len(museums), museums)
	}
	// The entity count must report all three, not just the survivor: it is what
	// decides whether a page was full, and undercounting truncates the crawl.
	if entities != 3 {
		t.Errorf("entities = %d, want 3", entities)
	}
	if museums[0].Name != "Real Museum" {
		t.Errorf("Name = %q", museums[0].Name)
	}
}

func TestMuseumsFromRows_FallsBackToCountry(t *testing.T) {
	rows := []binding{{"item": "http://www.wikidata.org/entity/Q1", "itemLabel": "Museum"}}

	if uz, _ := museumsFromRows(rows, "Uzbekistan"); uz[0].Country != "Uzbekistan" {
		got := uz[0].Country
		t.Errorf("Country = %q, want the fallback to apply", got)
	}
	if none, _ := museumsFromRows(rows, ""); none[0].Country != "unknown" {
		t.Errorf("Country = %q, want the unknown placeholder", none[0].Country)
	}
}
