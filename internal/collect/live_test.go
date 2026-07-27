package collect_test

import (
	"context"
	"testing"
	"time"

	"museum/internal/collect"
	"museum/internal/models"
	"museum/pkg/osm"
	"museum/pkg/wikidata"
	"museum/pkg/wikipedia"
)

// TestMergeLiveSources runs every source against one small country and checks
// that the merged result is coherent: the sources genuinely overlap, the
// overlap collapses rather than duplicating, and the union is larger than any
// single source on its own — which is the whole reason for running more than
// one.
//
// Andorra is used because it is small enough to crawl in a test yet is covered
// by all four catalogues.
func TestMergeLiveSources(t *testing.T) {
	if testing.Short() {
		t.Skip("hits the live Wikidata, Wikipedia and Overpass APIs")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const country = "Andorra"
	perSource := map[string][]models.Museum{}

	// Wikidata: Andorra is Q228.
	wd, err := wikidata.NewService(wikidata.NewClient()).
		CountryMuseums(ctx, wikidata.Country{ID: "Q228", Name: country})
	if err != nil {
		t.Fatalf("wikidata: %v", err)
	}
	perSource["wikidata"] = wd

	// OpenStreetMap.
	om, err := osm.NewService(osm.NewClient()).CountryMuseums(ctx, country, "AD")
	if err != nil {
		t.Fatalf("osm: %v", err)
	}
	perSource["openstreetmap"] = om

	// Wikipedia category tree.
	crawler := wikipedia.NewCategoryCrawler(wikipedia.NewCategoryService(wikipedia.NewClient()))
	var cat []models.Museum
	for museum := range crawler.Museums(ctx, "Category:Museums in Andorra") {
		cat = append(cat, museum)
	}
	perSource["wikipedia-category"] = cat

	largest := 0
	for name, museums := range perSource {
		t.Logf("%-20s %3d museums", name, len(museums))
		if len(museums) == 0 {
			t.Errorf("source %q returned nothing for %s", name, country)
		}
		largest = max(largest, len(museums))
	}

	merger := collect.NewMerger()
	total := 0
	for _, museums := range perSource {
		for _, museum := range museums {
			merger.Add(museum)
			total++
		}
	}

	distinct, merged := merger.Stats()
	t.Logf("merged: %d records -> %d distinct (%d folded)", total, distinct, merged)

	if merged == 0 {
		t.Error("no records merged at all — the sources should overlap on a country this small")
	}
	if distinct >= total {
		t.Errorf("distinct (%d) is not below the record count (%d); nothing was deduplicated", distinct, total)
	}
	if distinct <= largest {
		t.Errorf("distinct (%d) is no larger than the biggest single source (%d); "+
			"combining sources added nothing", distinct, largest)
	}

	// Every merged record must still be usable.
	for _, museum := range merger.Museums() {
		if museum.Name == "" {
			t.Error("merged a museum with no name")
		}
		if museum.Country != country {
			t.Errorf("%s: country = %q, want %q", museum.Name, museum.Country, country)
		}
		if len(museum.Sources) == 0 {
			t.Errorf("%s: no sources recorded", museum.Name)
		}
	}
}
