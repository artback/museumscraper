package wikidata

import (
	"slices"
	"testing"

	"museum/internal/models"
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

func TestClassIDsFromRows(t *testing.T) {
	// The class query pairs each museum with each of its P31 values, so a
	// museum ship arrives as several rows. The OPTIONAL that keeps the query
	// off a full P31 scan also lets a museum come back with no class at all.
	rows := []binding{
		{"item": "http://www.wikidata.org/entity/Q10659234", "class": "http://www.wikidata.org/entity/Q178193"},
		{"item": "http://www.wikidata.org/entity/Q10659234", "class": "http://www.wikidata.org/entity/Q2055880"},
		{"item": "http://www.wikidata.org/entity/Q10659234", "class": "http://www.wikidata.org/entity/Q10416961"},
		// Repeated by a second path through the data; it must not be stored twice.
		{"item": "http://www.wikidata.org/entity/Q10659234", "class": "http://www.wikidata.org/entity/Q178193"},
		{"item": "http://www.wikidata.org/entity/Q23402", "class": "http://www.wikidata.org/entity/Q207694"},
		{"item": "http://www.wikidata.org/entity/Q999"},
	}

	ids := classIDsFromRows(rows)

	want := []string{"Q178193", "Q2055880", "Q10416961"}
	if got := ids["Q10659234"]; !slices.Equal(got, want) {
		t.Errorf("classes = %v, want %v", got, want)
	}
	if got := ids["Q23402"]; !slices.Equal(got, []string{"Q207694"}) {
		t.Errorf("classes = %v", got)
	}
	// An item row with no class is not an item with an empty class.
	if _, present := ids["Q999"]; present {
		t.Errorf("an unclassified museum should contribute no entry, got %v", ids["Q999"])
	}
}

func TestAttachClasses(t *testing.T) {
	museums := []models.Museum{
		{Name: "Bohuslän", WikidataID: "Q10659234"},
		{Name: "Musée d'Orsay", WikidataID: "Q23402"},
		{Name: "No Wikidata id"},
	}

	attachClasses(museums, map[string][]string{
		"Q10659234": {"steamboat", "passenger ship", "working life museum"},
		// A class for a museum that is not on this page must not be attached to
		// anything.
		"Q7": {"art museum"},
	})

	// This is the case the whole change exists for: "Bohuslän" is also a Swedish
	// province, and only the classes say the record is a ship.
	want := []string{"steamboat", "passenger ship", "working life museum"}
	if !slices.Equal(museums[0].Classes, want) {
		t.Errorf("Classes = %v, want %v", museums[0].Classes, want)
	}
	if museums[1].Classes != nil {
		t.Errorf("Classes = %v, want none", museums[1].Classes)
	}
	if museums[2].Classes != nil {
		t.Errorf("Classes = %v, want none", museums[2].Classes)
	}
}

func TestAttachClasses_NoClassesLeavesMuseumsIntact(t *testing.T) {
	// A failed class query returns nil, which must cost the classes and not the
	// museums — the whole reason the class query is a separate request.
	museums := []models.Museum{{Name: "Bohuslän", WikidataID: "Q10659234", Website: "https://example.org"}}

	attachClasses(museums, nil)

	if len(museums) != 1 || museums[0].Website != "https://example.org" {
		t.Errorf("museums = %+v, want them untouched", museums)
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
