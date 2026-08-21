package postgres

import (
	"context"
	"testing"
	"time"

	"museum/pkg/exhibitions"
)

// TestExhibitionProvenance_SurvivesTheRoundTrip covers the whole path the
// field exists for: stamped by the scraper, stored, and read back by both
// query paths a caller can reach it through.
func TestExhibitionProvenance_SurvivesTheRoundTrip(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	past := time.Now().AddDate(0, -1, 0)
	soon := time.Now().AddDate(0, 0, 20)

	if _, err := store.SaveExhibitions(ctx, []exhibitions.Exhibition{
		{URL: "https://example.org/declared", Title: "Declared Show", Museum: "M",
			Start: &past, End: &soon, Latitude: 48.86, Longitude: 2.35,
			Provenance: exhibitions.ProvenanceDeclared},
		{URL: "https://example.org/generated", Title: "Generated Show", Museum: "M",
			Start: &past, End: &soon, Latitude: 48.86, Longitude: 2.35,
			Provenance: exhibitions.ProvenanceGenerated},
		// A row written before the column existed, which is most of the table
		// on the day this ships.
		{URL: "https://example.org/legacy", Title: "Legacy Show", Museum: "M",
			Start: &past, End: &soon, Latitude: 48.86, Longitude: 2.35},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	want := map[string]exhibitions.Provenance{
		"Declared Show":  exhibitions.ProvenanceDeclared,
		"Generated Show": exhibitions.ProvenanceGenerated,
		"Legacy Show":    "",
	}

	nearby, err := store.ExhibitionsNearby(ctx, 48.8566, 2.3522, 2, false, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(nearby) != len(want) {
		t.Fatalf("got %d nearby, want %d: %+v", len(nearby), len(want), nearby)
	}
	for _, hit := range nearby {
		if got := hit.Provenance; got != want[hit.Title] {
			t.Errorf("nearby: %q has provenance %q, want %q", hit.Title, got, want[hit.Title])
		}
	}

	for title, wantProvenance := range want {
		found, _, err := store.SearchExhibitions(ctx, title, 0, 0, 0, false, false, 10, 0)
		if err != nil {
			t.Fatalf("search %q: %v", title, err)
		}
		if len(found) == 0 {
			t.Fatalf("search %q found nothing", title)
		}
		if got := found[0].Provenance; got != wantProvenance {
			t.Errorf("search: %q has provenance %q, want %q", title, got, wantProvenance)
		}
	}
}

// TestExhibitionProvenance_UpsertFollowsTheMove checks that a site moving
// between rungs is re-counted rather than keeping the rung it was first read
// by. A site the heuristics stop reading is exactly the case the fallback
// exists for, and the ratio has to show it.
func TestExhibitionProvenance_UpsertFollowsTheMove(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	soon := time.Now().AddDate(0, 0, 20)
	listing := exhibitions.Exhibition{
		URL: "https://example.org/one", Title: "One", Museum: "M", End: &soon,
		Latitude: 48.86, Longitude: 2.35, Provenance: exhibitions.ProvenanceHeuristic,
	}
	if _, err := store.SaveExhibitions(ctx, []exhibitions.Exhibition{listing}); err != nil {
		t.Fatalf("first save: %v", err)
	}

	listing.Provenance = exhibitions.ProvenanceGenerated
	if _, err := store.SaveExhibitions(ctx, []exhibitions.Exhibition{listing}); err != nil {
		t.Fatalf("second save: %v", err)
	}

	found, err := store.ExhibitionsNearby(ctx, 48.8566, 2.3522, 2, false, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d entries, want the one upserted: %+v", len(found), found)
	}
	if got := found[0].Provenance; got != exhibitions.ProvenanceGenerated {
		t.Errorf("Provenance = %q after re-reading by another rung, want %q",
			got, exhibitions.ProvenanceGenerated)
	}
}

// TestCounts_ByProvenance is the standing ratio, as the health endpoint
// reports it.
func TestCounts_ByProvenance(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	soon := time.Now().AddDate(0, 0, 20)
	batch := []exhibitions.Exhibition{
		{URL: "https://example.org/a", Title: "A", End: &soon, Provenance: exhibitions.ProvenanceHeuristic},
		{URL: "https://example.org/b", Title: "B", End: &soon, Provenance: exhibitions.ProvenanceHeuristic},
		{URL: "https://example.org/c", Title: "C", End: &soon, Provenance: exhibitions.ProvenanceGenerated},
		{URL: "https://example.org/d", Title: "D", End: &soon},
	}
	if _, err := store.SaveExhibitions(ctx, batch); err != nil {
		t.Fatalf("save: %v", err)
	}

	counts, err := store.countNow(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}

	want := map[string]int64{"heuristic": 2, "generated": 1, "unknown": 1}
	for provenance, wantN := range want {
		if got := counts.ByProvenance[provenance]; got != wantN {
			t.Errorf("ByProvenance[%q] = %d, want %d (got %v)",
				provenance, got, wantN, counts.ByProvenance)
		}
	}
}
