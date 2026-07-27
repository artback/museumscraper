package collect

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"museum/internal/models"
)

func TestMerger_MergesOnWikidataID(t *testing.T) {
	m := NewMerger()

	m.Add(models.Museum{
		Name:       "Musée d'Orsay",
		Country:    "France",
		WikidataID: "Q23402",
		Latitude:   48.8599,
		Longitude:  2.3265,
		Sources:    []string{"wikidata"},
	})
	// Same museum from another source, under a different display name and
	// carrying facts the first record lacked.
	m.Add(models.Museum{
		Name:         "Musee d Orsay",
		Country:      "France",
		WikidataID:   "Q23402",
		WikipediaURL: "https://en.wikipedia.org/wiki/Mus%C3%A9e_d%27Orsay",
		Locality:     "Paris",
		Verified:     true,
		Sources:      []string{"wikipedia-category"},
	})

	museums := m.Museums()
	if len(museums) != 1 {
		t.Fatalf("got %d museums, want 1: %+v", len(museums), museums)
	}

	got := museums[0]
	if got.Name != "Musée d'Orsay" {
		t.Errorf("Name = %q, want the first source's spelling", got.Name)
	}
	if got.Locality != "Paris" {
		t.Errorf("Locality = %q, want it filled from the second source", got.Locality)
	}
	if got.WikipediaURL == "" {
		t.Error("WikipediaURL should have been filled from the second source")
	}
	if !got.Verified {
		t.Error("Verified should be true once any source establishes it")
	}
	if !got.HasCoordinates() {
		t.Error("coordinates from the first source should survive")
	}
	if want := []string{"wikidata", "wikipedia-category"}; !slices.Equal(got.Sources, want) {
		t.Errorf("Sources = %v, want %v", got.Sources, want)
	}
}

func TestMerger_MergesOnNameAndCountry(t *testing.T) {
	m := NewMerger()

	m.Add(models.Museum{Name: "The Louvre", Country: "France", Sources: []string{"lists"}})
	m.Add(models.Museum{Name: "the  louvre!", Country: "france", Latitude: 48.86, Longitude: 2.33, Sources: []string{"wikidata"}})

	if museums := m.Museums(); len(museums) != 1 {
		t.Fatalf("got %d museums, want 1: %+v", len(museums), museums)
	} else if !museums[0].HasCoordinates() {
		t.Error("coordinates should have been filled by the second record")
	}
}

func TestMerger_KeepsDistinctMuseums(t *testing.T) {
	m := NewMerger()

	// Same name, different countries.
	m.Add(models.Museum{Name: "National Museum", Country: "France"})
	m.Add(models.Museum{Name: "National Museum", Country: "Ghana"})
	// Same name and country, but the sources disagree on identity, so the
	// weaker name match must not override the explicit ids.
	m.Add(models.Museum{Name: "City Museum", Country: "Germany", WikidataID: "Q1"})
	m.Add(models.Museum{Name: "City Museum", Country: "Germany", WikidataID: "Q2"})

	if got := len(m.Museums()); got != 4 {
		t.Errorf("got %d museums, want 4", got)
	}
}

func TestMerger_UnknownCountryDoesNotCollapse(t *testing.T) {
	m := NewMerger()

	// Two unrelated museums whose country is unknown must stay separate: a
	// bare name is not enough evidence to merge on.
	m.Add(models.Museum{Name: "City Museum", Country: "unknown"})
	m.Add(models.Museum{Name: "City Museum", Country: "unknown"})

	if got := len(m.Museums()); got != 2 {
		t.Errorf("got %d museums, want 2", got)
	}
}

func TestMerger_RealCountryReplacesUnknown(t *testing.T) {
	m := NewMerger()

	m.Add(models.Museum{Name: "Herat National Museum", Country: "unknown", WikidataID: "Q5732778"})
	m.Add(models.Museum{Name: "Herat National Museum", Country: "Afghanistan", WikidataID: "Q5732778"})

	museums := m.Museums()
	if len(museums) != 1 {
		t.Fatalf("got %d museums, want 1", len(museums))
	}
	if museums[0].Country != "Afghanistan" {
		t.Errorf("Country = %q, want the real country to replace the placeholder", museums[0].Country)
	}
}

func TestMerger_ConcurrentAdd(t *testing.T) {
	m := NewMerger()

	var wg sync.WaitGroup
	for source := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				m.Add(models.Museum{
					Name:       fmt.Sprintf("Museum %d", i),
					Country:    "Testland",
					WikidataID: fmt.Sprintf("Q%d", i),
					Sources:    []string{fmt.Sprintf("source-%d", source)},
				})
			}
		}()
	}
	wg.Wait()

	museums := m.Museums()
	if len(museums) != 50 {
		t.Fatalf("got %d museums, want 50", len(museums))
	}
	for _, museum := range museums {
		if len(museum.Sources) != 4 {
			t.Errorf("%s: sources = %v, want all 4", museum.Name, museum.Sources)
		}
	}
}

func TestMerger_WikidataCountryOverridesInference(t *testing.T) {
	m := NewMerger()

	// The category crawl infers the country from an ancestor category, which
	// is wrong for a satellite institution.
	m.Add(models.Museum{
		Name: "Centre Pompidou Hanwha", Country: "France",
		WikidataID: "Q120716244", Sources: []string{"wikipedia-category"},
	})
	m.Add(models.Museum{
		Name: "Centre Pompidou Hanwha", Country: "South Korea",
		WikidataID: "Q120716244", Sources: []string{"wikidata"},
	})

	museums := m.Museums()
	if len(museums) != 1 {
		t.Fatalf("got %d museums, want 1", len(museums))
	}
	if museums[0].Country != "South Korea" {
		t.Errorf("Country = %q, want Wikidata's statement to win", museums[0].Country)
	}
}

func TestMerger_InferredCountryDoesNotOverrideWikidata(t *testing.T) {
	m := NewMerger()

	m.Add(models.Museum{
		Name: "Centre Pompidou Hanwha", Country: "South Korea",
		WikidataID: "Q120716244", Sources: []string{"wikidata"},
	})
	m.Add(models.Museum{
		Name: "Centre Pompidou Hanwha", Country: "France",
		WikidataID: "Q120716244", Sources: []string{"wikipedia-category"},
	})

	if got := m.Museums()[0].Country; got != "South Korea" {
		t.Errorf("Country = %q, want Wikidata's statement to survive regardless of arrival order", got)
	}
}

func TestMerger_MatchesOnAlternativeNames(t *testing.T) {
	m := NewMerger()

	// Wikidata labels this one in English.
	m.Add(models.Museum{Name: "Iron Museum", Country: "Andorra", Sources: []string{"wikidata"}})
	// OpenStreetMap names it locally but records the English name too, which is
	// the only thing that lets the two records meet.
	m.Add(models.Museum{
		Name:        "Museu de la Farga Rossell",
		AlsoKnownAs: []string{"Iron Museum"},
		Country:     "Andorra",
		Latitude:    42.55, Longitude: 1.48,
		Sources: []string{"openstreetmap"},
	})

	museums := m.Museums()
	if len(museums) != 1 {
		t.Fatalf("got %d museums, want 1: %+v", len(museums), museums)
	}
	if !museums[0].HasCoordinates() {
		t.Error("coordinates from the OSM record should have been merged in")
	}
	if !slices.Contains(museums[0].AlsoKnownAs, "Museu de la Farga Rossell") {
		t.Errorf("AlsoKnownAs = %v, want the local name kept", museums[0].AlsoKnownAs)
	}
	if want := []string{"openstreetmap", "wikidata"}; !slices.Equal(museums[0].Sources, want) {
		t.Errorf("Sources = %v, want %v", museums[0].Sources, want)
	}
}

func TestMerger_AliasDoesNotCrossCountries(t *testing.T) {
	m := NewMerger()

	m.Add(models.Museum{Name: "Iron Museum", Country: "Andorra"})
	m.Add(models.Museum{Name: "Local Name", AlsoKnownAs: []string{"Iron Museum"}, Country: "Spain"})

	if got := len(m.Museums()); got != 2 {
		t.Errorf("got %d museums, want 2 — an alias must not match across countries", got)
	}
}

func TestCleanName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Both of these are real catalogue records: invisible formatting
		// characters that survived from a source label, looking identical to a
		// correct name while breaking sorting and matching.
		{name: "left-to-right mark", in: "Landrichterhaus Neustadtgödens‎", want: "Landrichterhaus Neustadtgödens"},
		{name: "line separator", in: "Riedmuseum Ottersdorf ", want: "Riedmuseum Ottersdorf"},
		{name: "escaped entity", in: "Art &amp; Design Museum", want: "Art & Design Museum"},
		{name: "zero-width joiner", in: "Mus‍eum", want: "Museum"},
		{name: "collapses whitespace", in: "  Museum   of   Art  ", want: "Museum of Art"},
		{name: "control character", in: "Museum\tof Art", want: "Museum of Art"},

		// Unusual but genuine names must survive untouched.
		{name: "short name", in: "M+", want: "M+"},
		{name: "numeric name", in: "70.8", want: "70.8"},
		{name: "pipe separator", in: "Kunstmuseum Winterthur | Beim Stadthaus", want: "Kunstmuseum Winterthur | Beim Stadthaus"},
		{name: "accents", in: "Musée d'Orsay", want: "Musée d'Orsay"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CleanName(tc.in); got != tc.want {
				t.Errorf("CleanName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMerger_CleansNamesOnEntry(t *testing.T) {
	m := NewMerger()
	m.Add(models.Museum{Name: "Riedmuseum Ottersdorf ", Country: "Germany"})

	got := m.Museums()[0].Name
	if got != "Riedmuseum Ottersdorf" {
		t.Errorf("stored name = %q, want it cleaned on the way in", got)
	}
}
