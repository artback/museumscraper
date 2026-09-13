package licence

import "testing"

func TestForKnownSources(t *testing.T) {
	cases := map[string]string{
		"openstreetmap":         "ODbL 1.0",
		"wikidata":              "CC0 1.0",
		"wikipedia-category":    "CC BY-SA 4.0",
		"wikipedia-list":        "CC BY-SA 4.0",
		"wikipedia-category-es": "CC BY-SA 4.0", // language editions share the licence
		"nominatim":             "ODbL 1.0",     // geocoding is OpenStreetMap data
	}
	for source, want := range cases {
		l, ok := For(source)
		if !ok {
			t.Errorf("For(%q) is unknown", source)
			continue
		}
		if l.Name != want {
			t.Errorf("For(%q).Name = %q, want %q", source, l.Name, want)
		}
	}

	if _, ok := For("some-future-source"); ok {
		t.Error("an unknown source reported a licence")
	}
}

// TestAttributionIsPresentWhereItIsRequired: ODbL and CC BY-SA both require a
// credit line, and a licence entry without one cannot be complied with. CC0
// requires none, and inventing one there would misstate the terms.
func TestAttributionIsPresentWhereItIsRequired(t *testing.T) {
	for _, source := range []string{"openstreetmap", "wikipedia-list", "nominatim"} {
		l, _ := For(source)
		if l.Attribution == "" {
			t.Errorf("%s (%s) carries no attribution text", source, l.Name)
		}
		if !l.ShareAlike {
			t.Errorf("%s (%s) is not marked share-alike", source, l.Name)
		}
		if l.URL == "" {
			t.Errorf("%s (%s) points at no licence text", source, l.Name)
		}
	}

	if l, _ := For("wikidata"); l.Attribution != "" || l.ShareAlike {
		t.Errorf("CC0 was given obligations it does not have: %+v", l)
	}
}

// TestForSourcesCollapsesEditions: a page naming four Wikipedia editions
// carries one obligation, not four, and a notice that repeats itself is one
// nobody reads.
func TestForSourcesCollapsesEditions(t *testing.T) {
	got := ForSources([]string{
		"wikipedia-category", "wikipedia-list-de", "wikipedia-category-ja",
		"wikidata", "openstreetmap", "openstreetmap", "unknown-thing",
	})

	if len(got) != 3 {
		t.Fatalf("got %d licences, want 3: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, l := range got {
		if seen[l.Name] {
			t.Errorf("%s appears twice", l.Name)
		}
		seen[l.Name] = true
	}
	for _, want := range []string{"ODbL 1.0", "CC BY-SA 4.0", "CC0 1.0"} {
		if !seen[want] {
			t.Errorf("%s missing from %+v", want, got)
		}
	}
}

func TestForSourcesIsStable(t *testing.T) {
	first := ForSources([]string{"openstreetmap", "wikidata"})
	second := ForSources([]string{"wikidata", "openstreetmap"})

	if len(first) != len(second) {
		t.Fatalf("different lengths: %+v vs %+v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("order depends on input: %+v vs %+v", first, second)
		}
	}
}

func TestAllCoversEverySourceTheCatalogueUses(t *testing.T) {
	all := All()
	if len(all) < 4 {
		t.Fatalf("All() returned %d licences: %+v", len(all), all)
	}
	for _, l := range all {
		if l.Source == "" || l.Name == "" {
			t.Errorf("incomplete entry: %+v", l)
		}
	}
}

// TestRegisterLicences: the two published registers carry different terms and
// the difference is the point — a US government work is outside copyright
// altogether, while Licence Ouverte makes attribution a condition. Collapsing
// them onto one "open data" notice would misstate both.
func TestRegisterLicences(t *testing.T) {
	imls, ok := For("imls")
	if !ok {
		t.Fatal("the United States register has no licence")
	}
	if imls.ShareAlike {
		t.Error("a US government work was marked share-alike")
	}
	if imls.Attribution == "" {
		t.Error("IMLS asks that reuse be acknowledged, so the credit line should be there")
	}

	fr, ok := For("museofile")
	if !ok {
		t.Fatal("the French register has no licence")
	}
	if fr.Name != "Licence Ouverte 2.0" || fr.Attribution == "" {
		t.Errorf("unexpected French register licence: %+v", fr)
	}
	if fr.ShareAlike {
		t.Error("Licence Ouverte puts no terms on derived work; marking it share-alike overstates it")
	}
}
