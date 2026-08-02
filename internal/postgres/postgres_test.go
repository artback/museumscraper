package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
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
			page, err := store.Search(ctx, tc.query, 5, 0)
			hits := page.Hits
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

	page, err := store.Search(ctx, "cheminot", 5, 0)
	hits := page.Hits
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

	page, err := store.Nearby(ctx, 48.8566, 2.3522, 2, 10, 0)
	hits := page.Hits
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

	page, _ := store.Search(ctx, "louvre", 1, 0)
	hits := page.Hits
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

	page, _ := store.Nearby(ctx, 48.8566, 2.3522, 5, 5, 0)
	hits := page.Hits
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

// The bug this guards: a museum first stored without a Wikidata id, then seen
// again with one. Its identity changes when the id arrives, so the upsert found
// no conflict and inserted a second copy. One Wikidata crawl made 632 of these.
func TestSaveMuseums_MergesWhenAMuseumGainsAWikidataID(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// A list crawl finds the museum but no identifier for it.
	fromList := models.Museum{Name: "Galata Museo del Mare", Country: "Italy",
		Website: "https://galatamuseodelmare.it", Sources: []string{"wikipedia-list"}}
	// Wikidata later supplies the same museum, with an id and coordinates.
	fromWikidata := models.Museum{Name: "Galata Museo del Mare", Country: "Italy",
		WikidataID: "Q1916826", Latitude: 44.4118, Longitude: 8.9223,
		Sitelinks: 7, Sources: []string{"wikidata"}}

	if _, err := store.SaveMuseums(ctx, []models.Museum{fromList}); err != nil {
		t.Fatalf("save list record: %v", err)
	}
	if _, err := store.SaveMuseums(ctx, []models.Museum{fromWikidata}); err != nil {
		t.Fatalf("save wikidata record: %v", err)
	}

	counts, err := store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Museums != 1 {
		t.Fatalf("museums = %d, want 1 — the record was duplicated rather than merged", counts.Museums)
	}

	page, err := store.Search(ctx, "galata museo del mare", 5, 0)
	hits := page.Hits
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("search returned %d hits, want 1", len(hits))
	}

	// Both sources' facts must survive the merge.
	got := hits[0].Museum
	if got.WikidataID != "Q1916826" {
		t.Errorf("wikidata id = %q, want Q1916826", got.WikidataID)
	}
	if got.Website != "https://galatamuseodelmare.it" {
		t.Errorf("website from the list crawl was lost: %q", got.Website)
	}
	if !got.HasCoordinates() {
		t.Error("coordinates from Wikidata were lost")
	}
}

// MergeDuplicates repairs the pairs already in the table, which the promotion
// cannot: there the Wikidata id is taken, so promoting would collide.
func TestMergeDuplicates(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Galata Museo del Mare", Country: "Italy", WikidataID: "Q1916826", Latitude: 44.4118, Longitude: 8.9223},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Reproduce the stored damage: a name-keyed twin of a row whose id is taken.
	if _, err := store.pool.Exec(ctx, `
INSERT INTO museums (name, normalized, search_text, locality_normalized, country, website, aliases, sources, verified, sitelinks)
VALUES ('Galata Museo del Mare', 'galata museo del mare', 'galata museo del mare', '', 'Italy',
        'https://galatamuseodelmare.it', '{}', '{wikipedia-list}', true, 0)`); err != nil {
		t.Fatalf("seed duplicate: %v", err)
	}

	removed, err := store.MergeDuplicates(ctx)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	page, err := store.Search(ctx, "galata museo del mare", 5, 0)
	hits := page.Hits
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("search returned %d hits, want 1", len(hits))
	}
	got := hits[0].Museum
	if got.WikidataID != "Q1916826" {
		t.Errorf("the row carrying the Wikidata id should have been kept, got %q", got.WikidataID)
	}
	if got.Website != "https://galatamuseodelmare.it" {
		t.Errorf("the discarded row's website was not carried over: %q", got.Website)
	}
	if !got.Verified {
		t.Error("verified should survive the merge")
	}

	// Running again must be a no-op.
	if again, err := store.MergeDuplicates(ctx); err != nil || again != 0 {
		t.Errorf("second merge removed %d (err %v), want 0 — not idempotent", again, err)
	}
}

// Ranking regression: a generic word shared with the query must not beat the
// distinctive one. word_similarity finds "museum zurich" inside "National
// Museum Zurich" and scored it above "Kunsthaus Zürich", which whole-name
// similarity ranks higher. Blending the two measures is what fixes it.
func TestSearch_DistinctiveNameBeatsGenericWord(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Kunsthaus Zürich", Country: "Switzerland", Locality: "Zurich",
			WikidataID: "Q685038", Sitelinks: 27},
		{Name: "National Museum Zurich", Country: "Switzerland", Locality: "Zurich",
			WikidataID: "Q671384", Sitelinks: 26},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	page, err := store.Search(ctx, "kunstmuseum zurich", 2, 0)
	hits := page.Hits
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Museum.Name != "Kunsthaus Zürich" {
		t.Errorf("top hit = %q, want %q", hits[0].Museum.Name, "Kunsthaus Zürich")
	}
}

// The locality bonus is matched on word boundaries. A plain substring test
// fired on three-letter towns inside unrelated words — "Sé" (Funchal) matched
// every query containing "mu-se-um" — promoting an unrelated museum a full
// point above equally-named rivals.
func TestSearch_LocalityBonusRespectsWordBoundaries(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Sacred Art Museum", Country: "Spain", Locality: "La Rioja", WikidataID: "Q1"},
		{Name: "Sacred Art Museum of Funchal", Country: "Portugal", Locality: "Sé", WikidataID: "Q2"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	page, err := store.Search(ctx, "sacred art museum", 2, 0)
	hits := page.Hits
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Museum.Name != "Sacred Art Museum" {
		t.Errorf("top hit = %q, want the exact match; %q was promoted by its town matching inside \"museum\"",
			hits[0].Museum.Name, hits[0].Museum.Name)
	}
}

// Paging must reach every match. London holds more museums than one page can
// carry, and without a total the caller cannot tell a full page from the end
// of the results — 114 of 614 were unreachable by any request.
func TestNearby_PagesThroughEveryMatch(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	const total = 25
	museums := make([]models.Museum, 0, total)
	for i := range total {
		museums = append(museums, models.Museum{
			Name: fmt.Sprintf("Museum %02d", i), Country: "France",
			WikidataID: fmt.Sprintf("Q%d", 1000+i),
			// Spread along a line so the ordering by distance is deterministic.
			Latitude: 48.8566 + float64(i)*0.001, Longitude: 2.3522,
		})
	}
	if _, err := store.SaveMuseums(ctx, museums); err != nil {
		t.Fatalf("save: %v", err)
	}

	seen := make(map[int64]bool)
	for offset := 0; offset < total; offset += 10 {
		page, err := store.Nearby(ctx, 48.8566, 2.3522, 50, 10, offset)
		if err != nil {
			t.Fatalf("nearby at offset %d: %v", offset, err)
		}
		if page.Total != total {
			t.Errorf("total at offset %d = %d, want %d", offset, page.Total, total)
		}
		for _, hit := range page.Hits {
			if hit.ID == 0 {
				t.Fatal("hit has no id, so nothing can page on it")
			}
			if seen[hit.ID] {
				t.Errorf("museum %d returned on two pages", hit.ID)
			}
			seen[hit.ID] = true
		}
	}
	if len(seen) != total {
		t.Errorf("paged through %d museums, want %d", len(seen), total)
	}
}

func TestMuseumByID(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Louvre Museum", Country: "France", WikidataID: "Q19675",
			Latitude: 48.8606, Longitude: 2.3376},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Findable by the Wikidata id...
	byWikidata, err := store.MuseumByID(ctx, "Q19675")
	if err != nil {
		t.Fatalf("by wikidata id: %v", err)
	}
	if byWikidata.Museum.Name != "Louvre Museum" {
		t.Errorf("name = %q", byWikidata.Museum.Name)
	}

	// ...and by the numeric id the responses carry, which is what the 4% of
	// the catalogue with no Wikidata id has to rely on.
	byNumeric, err := store.MuseumByID(ctx, strconv.FormatInt(byWikidata.ID, 10))
	if err != nil {
		t.Fatalf("by numeric id: %v", err)
	}
	if byNumeric.ID != byWikidata.ID {
		t.Errorf("id = %d, want %d", byNumeric.ID, byWikidata.ID)
	}

	if _, err := store.MuseumByID(ctx, "Q000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// The signal that would have made an empty result readable.
func TestExhibitionCoverage(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "With Site", Country: "France", WikidataID: "Q1",
			Latitude: 48.8566, Longitude: 2.3522, Website: "https://example.org"},
		{Name: "Without Site", Country: "France", WikidataID: "Q2",
			Latitude: 48.8570, Longitude: 2.3525},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	coverage, err := store.ExhibitionCoverage(ctx, 48.8566, 2.3522, 5)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if coverage.MuseumsInArea != 2 {
		t.Errorf("museums in area = %d, want 2", coverage.MuseumsInArea)
	}
	if coverage.MuseumsWithSite != 1 {
		t.Errorf("museums with a website = %d, want 1", coverage.MuseumsWithSite)
	}
	// Never scraped, which is exactly what an empty result needs to say.
	if coverage.LastScraped != nil {
		t.Errorf("last scraped = %v, want nil for an area never refreshed", coverage.LastScraped)
	}
}

// An acronym is an exact match or nothing. search_text holds the aliases, but
// only for fuzzy matching, and whole-string similarity cannot find "moma"
// inside "museum of modern art moma museum of modern art new york" — so the
// Museum of Modern Art was not even a candidate, while MOMA Tainan and MOMA
// Machynlleth were.
func TestSearch_ExactAliasBeatsNamePrefix(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Museum of Modern Art", Country: "United States", WikidataID: "Q188740",
			AlsoKnownAs: []string{"MoMA", "The Museum of Modern Art, New York"}, Sitelinks: 67},
		{Name: "MOMA Tainan", Country: "Taiwan", WikidataID: "Q2", Sitelinks: 1},
		{Name: "MOMA Machynlleth", Country: "United Kingdom", WikidataID: "Q3", Sitelinks: 1},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	page, err := store.Search(ctx, "moma", 3, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(page.Hits) == 0 {
		t.Fatal("no hits")
	}
	if page.Hits[0].Museum.Name != "Museum of Modern Art" {
		t.Errorf("top hit = %q, want the museum that calls itself MoMA", page.Hits[0].Museum.Name)
	}
}

// Two sources know a museum by different names. Neither may erase the other's:
// OpenStreetMap carries the local name and Wikidata the English one, and the
// catalogue is only searchable in both languages if it keeps both.
func TestSaveMuseums_UnionsAliasesAcrossSources(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	fromWikidata := models.Museum{Name: "The Maritime Museum and Aquarium", Country: "Sweden",
		WikidataID: "Q10545689", AlsoKnownAs: []string{"Maritime Museum"}, Sources: []string{"wikidata"}}
	fromOSM := models.Museum{Name: "The Maritime Museum and Aquarium", Country: "Sweden",
		WikidataID: "Q10545689", AlsoKnownAs: []string{"Sjöfartsmuseet Akvariet"}, Sources: []string{"osm"}}

	if _, err := store.SaveMuseums(ctx, []models.Museum{fromWikidata}); err != nil {
		t.Fatalf("save wikidata: %v", err)
	}
	if _, err := store.SaveMuseums(ctx, []models.Museum{fromOSM}); err != nil {
		t.Fatalf("save osm: %v", err)
	}

	page, err := store.Search(ctx, "maritime museum aquarium", 1, 0)
	if err != nil || len(page.Hits) == 0 {
		t.Fatalf("search: %v (%d hits)", err, len(page.Hits))
	}
	got := page.Hits[0].Museum

	for _, want := range []string{"Maritime Museum", "Sjöfartsmuseet Akvariet"} {
		if !slices.Contains(got.AlsoKnownAs, want) {
			t.Errorf("alias %q was lost; have %v", want, got.AlsoKnownAs)
		}
	}
	for _, want := range []string{"wikidata", "osm"} {
		if !slices.Contains(got.Sources, want) {
			t.Errorf("source %q was lost; have %v", want, got.Sources)
		}
	}

	// And the local name must actually be searchable, which is the point.
	swedish, err := store.Search(ctx, "sjöfartsmuseet akvariet", 1, 0)
	if err != nil {
		t.Fatalf("swedish search: %v", err)
	}
	if len(swedish.Hits) == 0 || swedish.Hits[0].Museum.WikidataID != "Q10545689" {
		t.Error("the museum is not findable by the name OpenStreetMap knows it by")
	}
}

// A museum with no coordinates is findable by name and invisible to every
// radius and place query. A fifth of the catalogue is in that state, and
// nothing could repair it: enrichment geocodes only what arrives through the
// event pipeline.
func TestUnplacedMuseumsAndSetLocation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Placed Museum", Country: "Sweden", Locality: "Gothenburg",
			WikidataID: "Q1", Latitude: 57.70, Longitude: 11.96},
		{Name: "Unplaced Museum", Country: "Sweden", Locality: "Gothenburg", WikidataID: "Q2"},
		{Name: "Unplaced Elsewhere", Country: "Norway", Locality: "Oslo", WikidataID: "Q3"},
		// Nothing to geocode from, so it must not be offered up.
		{Name: "No Place At All", WikidataID: "Q4"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	pending, err := store.UnplacedMuseums(ctx, "Gothenburg", "", 10)
	if err != nil {
		t.Fatalf("unplaced: %v", err)
	}
	if len(pending) != 1 || pending[0].Name != "Unplaced Museum" {
		t.Fatalf("unplaced = %+v, want only the unplaced Gothenburg museum", pending)
	}

	all, err := store.UnplacedMuseums(ctx, "", "", 10)
	if err != nil {
		t.Fatalf("unplaced all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("unplaced overall = %d, want 2 — a record with no town and no country has nothing to geocode from", len(all))
	}

	if err := store.SetLocation(ctx, pending[0].ID, 57.7072, 11.967, false); err != nil {
		t.Fatalf("set location: %v", err)
	}

	// Now reachable by the query that could not see it before.
	page, err := store.Nearby(ctx, 57.7072, 11.967, 5, 10, 0)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	var found bool
	for _, hit := range page.Hits {
		if hit.Museum.Name == "Unplaced Museum" {
			found = true
		}
	}
	if !found {
		t.Error("the museum is still invisible to a radius query after being located")
	}

	if remaining, _ := store.UnplacedMuseums(ctx, "Gothenburg", "", 10); len(remaining) != 0 {
		t.Errorf("still %d unplaced in Gothenburg, want 0", len(remaining))
	}
}

// The unverified tail is names read off list pages that no source confirmed
// are museums — a list of museums in Maryland yielded "Williamsburg, Virginia".
// A caller that cannot tolerate them must be able to exclude them.
func TestNearbyVerified_FiltersTheUnverifiedTail(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Real Museum", Country: "Sweden", WikidataID: "Q1",
			Latitude: 57.7072, Longitude: 11.967, Verified: true},
		{Name: "Williamsburg, Virginia", Country: "Sweden", WikidataID: "Q2",
			Latitude: 57.7073, Longitude: 11.968, Verified: false},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	all, err := store.NearbyVerified(ctx, 57.7072, 11.967, 5, 10, 0, false)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(all.Hits) != 2 {
		t.Errorf("unfiltered = %d hits, want both", len(all.Hits))
	}

	only, err := store.NearbyVerified(ctx, 57.7072, 11.967, 5, 10, 0, true)
	if err != nil {
		t.Fatalf("nearby verified: %v", err)
	}
	if len(only.Hits) != 1 || only.Hits[0].Museum.Name != "Real Museum" {
		t.Errorf("verified-only = %+v, want just the confirmed museum", only.Hits)
	}
	if only.Total != 1 {
		t.Errorf("total = %d, want it to reflect the filter", only.Total)
	}
}

// Placing museums at their town centre must be one statement, not one per
// museum: doing it a row at a time crashed the database partway through 30,400
// round trips, each re-running an aggregate over the town's museums.
func TestPlaceAtTownCentres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Anchor One", Country: "Sweden", Locality: "Gothenburg Municipality",
			WikidataID: "Q1", Latitude: 57.70, Longitude: 11.96},
		{Name: "Anchor Two", Country: "Sweden", Locality: "Gothenburg Municipality",
			WikidataID: "Q2", Latitude: 57.72, Longitude: 11.98},
		// Unplaced, but in a town two museums are already placed in.
		{Name: "Lost Museum", Country: "Sweden", Locality: "Gothenburg Municipality", WikidataID: "Q3"},
		// Unplaced in a town nothing is placed in: nothing to infer from.
		{Name: "Nowhere Museum", Country: "Sweden", Locality: "Unknownville", WikidataID: "Q4"},
		// The same town under the plain name the sources also use. Matching it
		// to the administrative form is the only reason the second pass exists.
		{Name: "Plainly Named Museum", Country: "Sweden", Locality: "Gothenburg", WikidataID: "Q5"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	placed, discarded, err := store.PlaceAtTownCentres(ctx)
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if placed != 2 {
		t.Fatalf("placed = %d, want 2 — the museums whose town is known under either name", placed)
	}
	if discarded != 0 {
		t.Errorf("discarded = %d, want 0 — nothing was approximately placed before", discarded)
	}

	// Now reachable by a radius query, and flagged as approximate.
	page, err := store.Nearby(ctx, 57.71, 11.97, 10, 10, 0)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	var found bool
	for _, hit := range page.Hits {
		if hit.Museum.Name == "Lost Museum" {
			found = true
			if !hit.ApproximateLocation {
				t.Error("a town-centre position must be reported as approximate")
			}
		}
	}
	if !found {
		t.Error("the museum is still invisible to a radius query")
	}

	// Idempotent, but by recomputing rather than by leaving things alone: the
	// second run takes back the one approximate position it made and arrives at
	// the same one again. That is the property worth having, because a position
	// derived from a rule that later turns out to be wrong has to be reachable.
	again, discarded, err := store.PlaceAtTownCentres(ctx)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again != 2 || discarded != 2 {
		t.Errorf("second run placed %d and discarded %d, want 2 and 2", again, discarded)
	}
}

// Towns are matched within a country, by their full name before their leading
// word, and only when the town's own museums sit close together.
//
// Without all three, a centroid stops describing anywhere: grouping on the
// leading word alone put "Kingston" in five countries and "South Bend" with
// South Korea, and the resulting centroids were points in the open ocean. 3,677
// museums were placed that way, Korean ones off the coast of Spain.
func TestPlaceAtTownCentresRefusesGroupsThatDescribeNowhere(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		// Two towns sharing a leading word, one per country and far apart.
		{Name: "Ontario Anchor", Country: "Canada", Locality: "Port Hope",
			WikidataID: "Q10", Latitude: 43.95, Longitude: -78.29},
		{Name: "NSW Anchor", Country: "Australia", Locality: "Port Macquarie",
			WikidataID: "Q11", Latitude: -31.43, Longitude: 152.91},
		// Two towns sharing a leading word within one country, far apart.
		{Name: "Illinois Anchor", Country: "United States", Locality: "Springfield, Illinois",
			WikidataID: "Q12", Latitude: 39.80, Longitude: -89.64},
		{Name: "Massachusetts Anchor", Country: "United States", Locality: "Springfield, Massachusetts",
			WikidataID: "Q13", Latitude: 42.10, Longitude: -72.59},

		// Each of these must stay unplaced rather than be put in the sea.
		{Name: "Adrift Abroad", Country: "Canada", Locality: "Port Colborne", WikidataID: "Q14"},
		{Name: "Adrift At Home", Country: "United States", Locality: "Springfield, Missouri", WikidataID: "Q15"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, _, err := store.PlaceAtTownCentres(ctx); err != nil {
		t.Fatalf("place: %v", err)
	}

	for _, name := range []string{"Adrift Abroad", "Adrift At Home"} {
		museum, err := museumNamed(ctx, store, name)
		if err != nil {
			t.Fatalf("read back %q: %v", name, err)
		}
		if museum.Latitude != 0 || museum.Longitude != 0 {
			t.Errorf("%q was placed at %.4f,%.4f — a centroid of towns this far apart is not a place",
				name, museum.Latitude, museum.Longitude)
		}
	}
}

// museumNamed reads one museum back by name, for assertions about what was
// written rather than about what a query returns.
func museumNamed(ctx context.Context, store *Store, name string) (models.Museum, error) {
	page, err := store.Search(ctx, name, 10, 0)
	if err != nil {
		return models.Museum{}, err
	}
	for _, hit := range page.Hits {
		if hit.Museum.Name == name {
			return hit.Museum, nil
		}
	}
	return models.Museum{}, fmt.Errorf("no museum named %q", name)
}

// Museums publish recurring events as one row per occurrence. Eight listings of
// one exhibition's guided tours are not eight exhibitions.
func TestMergeDuplicateExhibitions(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	day := func(s string) *time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return &d
	}

	if _, err := store.SaveExhibitions(ctx, []exhibitions.Exhibition{
		{URL: "https://h.example/t/1", Title: "Introduction To The Exhibition",
			Museum: "Hasselblad Center", Latitude: 57.70, Longitude: 11.97, Start: day("2026-08-15"), End: day("2026-08-15")},
		{URL: "https://h.example/t/2", Title: "introduction to the exhibition ",
			Museum: "Hasselblad Center", Latitude: 57.70, Longitude: 11.97, Start: day("2026-09-26"), End: day("2026-09-26")},
		{URL: "https://h.example/t/3", Title: "Introduction To The Exhibition",
			Museum: "Hasselblad Center", Latitude: 57.70, Longitude: 11.97, Start: day("2026-08-22"), End: day("2026-08-22")},
		// A different exhibition at the same museum must survive untouched.
		{URL: "https://h.example/show", Title: "Women Behind the Camera",
			Museum: "Hasselblad Center", Latitude: 57.70, Longitude: 11.97, Start: day("2026-08-08"), End: day("2026-12-01")},
		// The same title at a different museum is a different event.
		{URL: "https://other.example/x", Title: "Introduction To The Exhibition",
			Museum: "Other Museum", Latitude: 57.70, Longitude: 11.97, Start: day("2026-08-15")},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	removed, err := store.MergeDuplicateExhibitions(ctx)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 of the 3 occurrences", removed)
	}

	hits, err := store.ExhibitionsNearby(ctx, 57.70, 11.97, 5, true, 50)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	byTitle := map[string]int{}
	for _, h := range hits {
		byTitle[strings.ToLower(strings.TrimSpace(h.Title))+"|"+h.Museum]++
	}
	if got := byTitle["introduction to the exhibition|Hasselblad Center"]; got != 1 {
		t.Errorf("occurrences kept = %d, want 1", got)
	}
	if got := byTitle["women behind the camera|Hasselblad Center"]; got != 1 {
		t.Error("a distinct exhibition at the same museum was merged away")
	}
	if got := byTitle["introduction to the exhibition|Other Museum"]; got != 1 {
		t.Error("the same title at another museum was merged away")
	}

	// The survivor must span the whole run, not just its own occurrence.
	for _, h := range hits {
		if h.Museum == "Hasselblad Center" && strings.EqualFold(strings.TrimSpace(h.Title), "Introduction To The Exhibition") {
			if h.Start == nil || h.Start.Format("2006-01-02") != "2026-08-15" {
				t.Errorf("start = %v, want the earliest occurrence", h.Start)
			}
			if h.End == nil || h.End.Format("2006-01-02") != "2026-09-26" {
				t.Errorf("end = %v, want the latest occurrence", h.End)
			}
		}
	}
}

// One museum recorded under two names is a duplicate; two museums with similar
// names are not. The second half is what makes this dangerous, so it is tested
// harder than the first.
func TestMergeNameVariants(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		// The reported duplicate: same words, 200 m apart.
		{Name: "Gothenburg Museum", Country: "Sweden", WikidataID: "Q1",
			Latitude: 57.7070, Longitude: 11.9670, Sitelinks: 12},
		{Name: "Museum of Gothenburg", Country: "Sweden", WikidataID: "Q2",
			Latitude: 57.7072, Longitude: 11.9673, Sitelinks: 3,
			Website: "https://example.org", AlsoKnownAs: []string{"Stadsmuseum"}},

		// Same city, same word "museum", different museums — must survive.
		{Name: "Maritime Museum", Country: "Sweden", WikidataID: "Q3",
			Latitude: 57.7071, Longitude: 11.9671, Sitelinks: 5},

		// The case that makes trigram similarity unusable: near-identical
		// names, genuinely different museums, 2 km apart.
		{Name: "Tate Modern", Country: "United Kingdom", WikidataID: "Q4",
			Latitude: 51.5076, Longitude: -0.0994, Sitelinks: 60},
		{Name: "Tate Britain", Country: "United Kingdom", WikidataID: "Q5",
			Latitude: 51.4911, Longitude: -0.1278, Sitelinks: 50},

		// Same words but far apart: two branches of one institution.
		{Name: "Louvre Museum", Country: "France", WikidataID: "Q6",
			Latitude: 48.8606, Longitude: 2.3376, Sitelinks: 167},
		{Name: "Museum Louvre", Country: "United Arab Emirates", WikidataID: "Q7",
			Latitude: 24.5339, Longitude: 54.3980, Sitelinks: 40},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	removed, err := store.MergeNameVariants(ctx)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want exactly the one duplicate", removed)
	}

	survivors := map[string]bool{}
	if err := store.EachMuseum(ctx, func(m models.Museum) { survivors[m.Name] = true }); err != nil {
		t.Fatalf("read back: %v", err)
	}

	for _, want := range []string{
		"Gothenburg Museum", "Maritime Museum",
		"Tate Modern", "Tate Britain", "Louvre Museum", "Museum Louvre",
	} {
		if !survivors[want] {
			t.Errorf("%q was merged away and should not have been", want)
		}
	}
	if survivors["Museum of Gothenburg"] {
		t.Error("the duplicate survived")
	}

	// The survivor must inherit what the merged record knew, or merging is
	// just deletion.
	page, err := store.Search(ctx, "gothenburg museum", 1, 0)
	if err != nil || len(page.Hits) == 0 {
		t.Fatalf("search: %v", err)
	}
	kept := page.Hits[0].Museum
	if kept.Website == "" {
		t.Error("the merged record's website was lost")
	}
	if !slices.Contains(kept.AlsoKnownAs, "Museum of Gothenburg") {
		t.Errorf("the merged name was not kept as an alias; have %v", kept.AlsoKnownAs)
	}

	// And it must still be findable by the name that disappeared.
	byOldName, err := store.Search(ctx, "museum of gothenburg", 1, 0)
	if err != nil || len(byOldName.Hits) == 0 {
		t.Fatal("the museum is no longer findable by its merged-away name")
	}
}

// storedTitles reads back every stored exhibition's title. Straight SQL rather
// than a query method: these rows carry no position, so every reader that takes
// a radius would report them all as missing.
func storedTitles(t *testing.T, ctx context.Context, store *Store) map[string]bool {
	t.Helper()
	rows, err := store.pool.Query(ctx, "SELECT title FROM exhibitions")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()

	found := make(map[string]bool)
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found[title] = true
	}
	return found
}
