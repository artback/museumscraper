package postgres

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"museum/internal/models"
	"museum/pkg/exhibitions"
)

// testStore opens the database named by TEST_DATABASE_URL, skipping when there
// is none so the suite still runs offline.
//
// Each test gets its own schema, created here and dropped afterwards. That is
// not fastidiousness: an earlier version truncated the tables in whichever
// database the variable pointed at, and running the suite against the local
// stack deleted the entire loaded catalogue. Isolating by schema means the
// tests cannot reach real data however the variable is set.
func testStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the database tests")
	}

	ctx := context.Background()
	schema := fmt.Sprintf("test_%s_%d", sanitize(t.Name()), time.Now().UnixNano()%1e6)

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			return
		}
		defer cleanup.Close()
		cleanup.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})

	// public stays on the path because PostGIS and pg_trgm install their
	// functions there.
	scoped := dsn
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	scoped += separator + "options=" + url.QueryEscape("-csearch_path="+schema+",public")

	store, err := Open(ctx, scoped)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(store.Close)

	return store
}

// sanitize reduces a test name to something usable as a schema identifier.
func sanitize(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()[:min(24, b.Len())]
}

func TestSaveAndSearch_TypoTolerance(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	museums := []models.Museum{
		{Name: "Rijksmuseum", Country: "Netherlands", Locality: "Amsterdam",
			Latitude: 52.36, Longitude: 4.885, WikidataID: "Q190804"},
		{Name: "Van Gogh Museum", Country: "Netherlands", Locality: "Amsterdam",
			Latitude: 52.358, Longitude: 4.881, WikidataID: "Q224124"},
		{Name: "Solomon R. Guggenheim Museum", Country: "United States", Locality: "New York",
			Latitude: 40.783, Longitude: -73.959, WikidataID: "Q201469"},
		{Name: "Louvre Museum", Country: "France", Locality: "Paris",
			Latitude: 48.8606, Longitude: 2.3376, WikidataID: "Q19675"},
	}
	if _, err := store.SaveMuseums(ctx, museums); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Each of these failed against the hand-rolled index, which could only do
	// exact and prefix matching.
	cases := []struct {
		query string
		want  string
	}{
		{query: "rijksmuseum", want: "Rijksmuseum"},
		{query: "rijkmuseum", want: "Rijksmuseum"},
		{query: "rijks museum", want: "Rijksmuseum"},
		{query: "van gogh museum", want: "Van Gogh Museum"},
		{query: "van gough museum", want: "Van Gogh Museum"},
		{query: "guggenhiem", want: "Solomon R. Guggenheim Museum"},
		{query: "guggenheim", want: "Solomon R. Guggenheim Museum"},
		{query: "louvre", want: "Louvre Museum"},
		{query: "louvr", want: "Louvre Museum"},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			hits, err := store.Search(ctx, tc.query, 5)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(hits) == 0 {
				t.Fatalf("no results for %q", tc.query)
			}
			if hits[0].Museum.Name != tc.want {
				t.Errorf("top result = %q, want %q", hits[0].Museum.Name, tc.want)
			}
		})
	}
}

func TestSearch_FindsRecordWithoutCoordinates(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// A quarter of the catalogue has no position; search is the only way it is
	// reachable at all.
	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Musée du cheminot", Country: "France", Locality: "Ambérieu-en-Bugey", WikidataID: "Q111"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	hits, err := store.Search(ctx, "cheminot", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].Museum.HasCoordinates() {
		t.Error("expected the record with no coordinates")
	}
}

func TestNearby(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Louvre Museum", Country: "France", Latitude: 48.8606, Longitude: 2.3376, WikidataID: "Q1"},
		{Name: "Musée d'Orsay", Country: "France", Latitude: 48.8600, Longitude: 2.3266, WikidataID: "Q2"},
		{Name: "Château de Versailles", Country: "France", Latitude: 48.8049, Longitude: 2.1204, WikidataID: "Q3"},
		{Name: "Unplaced", Country: "France", WikidataID: "Q4"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	hits, err := store.Nearby(ctx, 48.8566, 2.3522, 2, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}

	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 — Versailles is 17 km away and one record has no position: %+v", len(hits), hits)
	}
	if hits[0].DistanceKm > hits[1].DistanceKm {
		t.Error("results are not ordered by distance")
	}
	if hits[0].DistanceKm <= 0 || hits[0].DistanceKm > 2 {
		t.Errorf("distance %v km is outside the radius", hits[0].DistanceKm)
	}
}

func TestSaveMuseums_UpsertsRatherThanDuplicating(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// A second crawl of the same museum must update the row, not add one.
	first := models.Museum{Name: "Louvre Museum", Country: "France", WikidataID: "Q19675"}
	second := models.Museum{Name: "Louvre Museum", Country: "France", WikidataID: "Q19675",
		Latitude: 48.8606, Longitude: 2.3376, Website: "https://louvre.fr", Verified: true}

	if _, err := store.SaveMuseums(ctx, []models.Museum{first}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := store.SaveMuseums(ctx, []models.Museum{second}); err != nil {
		t.Fatalf("save: %v", err)
	}

	counts, err := store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Museums != 1 {
		t.Fatalf("museums = %d, want 1", counts.Museums)
	}

	hits, _ := store.Search(ctx, "louvre", 1)
	if len(hits) != 1 {
		t.Fatal("expected the merged record")
	}
	if !hits[0].Museum.HasCoordinates() || hits[0].Museum.Website == "" || !hits[0].Museum.Verified {
		t.Errorf("the second crawl's facts were not merged in: %+v", hits[0].Museum)
	}
}

func TestSaveMuseums_KeepsKnownCoordinates(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	placed := models.Museum{Name: "Louvre Museum", Country: "France", WikidataID: "Q19675",
		Latitude: 48.8606, Longitude: 2.3376}
	unplaced := models.Museum{Name: "Louvre Museum", Country: "France", WikidataID: "Q19675"}

	if _, err := store.SaveMuseums(ctx, []models.Museum{placed}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// A later source with no position must not erase one already established.
	if _, err := store.SaveMuseums(ctx, []models.Museum{unplaced}); err != nil {
		t.Fatalf("save: %v", err)
	}

	hits, _ := store.Nearby(ctx, 48.8566, 2.3522, 5, 5)
	if len(hits) != 1 {
		t.Errorf("the museum lost its coordinates on the second write")
	}
}

func TestExhibitions(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	now := time.Now()
	past := now.AddDate(0, -1, 0)
	soon := now.AddDate(0, 0, 20)
	future := now.AddDate(0, 2, 0)
	later := now.AddDate(0, 4, 0)

	if _, err := store.SaveExhibitions(ctx, []exhibitions.Exhibition{
		{URL: "https://example.org/closed", Title: "Closed", Museum: "M", End: &past,
			Latitude: 48.86, Longitude: 2.35},
		{URL: "https://example.org/now", Title: "On now", Museum: "M", Start: &past, End: &soon,
			Latitude: 48.86, Longitude: 2.35},
		{URL: "https://example.org/later", Title: "On later", Museum: "M", Start: &past, End: &later,
			Latitude: 48.86, Longitude: 2.35},
		{URL: "https://example.org/upcoming", Title: "Not open yet", Museum: "M", Start: &future, End: &later,
			Latitude: 48.86, Longitude: 2.35},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	running, err := store.ExhibitionsNearby(ctx, 48.8566, 2.3522, 2, false, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(running) != 2 {
		t.Fatalf("got %d running, want 2: %+v", len(running), running)
	}
	// Soonest to close first is the order a visitor acts on.
	if running[0].Title != "On now" {
		t.Errorf("first = %q, want the one closing soonest", running[0].Title)
	}

	withUpcoming, err := store.ExhibitionsNearby(ctx, 48.8566, 2.3522, 2, true, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(withUpcoming) != 3 {
		t.Errorf("got %d with upcoming, want 3", len(withUpcoming))
	}
}
