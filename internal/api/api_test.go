package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"museum/internal/models"
	"museum/internal/postgres"
)

// fakeCatalogue stands in for the database so the handlers can be tested
// without one.
type fakeCatalogue struct {
	nearby      []postgres.Hit
	search      []postgres.Hit
	exhibitions []postgres.ExhibitionHit
	counts      postgres.Counts
	err         error

	// lastRadiusKm and lastLimit record what the handler passed down, so the
	// tests can check clamping actually reaches the query.
	lastRadiusKm float64
	lastLimit    int
	lastOffset   int
	lastUpcoming bool

	coverage postgres.Coverage
}

func (f *fakeCatalogue) Nearby(_ context.Context, _, _, radiusKm float64, limit, offset int) (postgres.Page, error) {
	f.lastRadiusKm, f.lastLimit, f.lastOffset = radiusKm, limit, offset
	return postgres.Page{Hits: f.nearby, Total: int64(len(f.nearby))}, f.err
}

func (f *fakeCatalogue) Search(_ context.Context, _ string, limit, offset int) (postgres.Page, error) {
	f.lastLimit, f.lastOffset = limit, offset
	return postgres.Page{Hits: f.search, Total: int64(len(f.search))}, f.err
}

func (f *fakeCatalogue) MuseumByID(_ context.Context, id string) (postgres.Hit, error) {
	if f.err != nil {
		return postgres.Hit{}, f.err
	}
	for _, hit := range f.nearby {
		if hit.Museum.WikidataID == id {
			return hit, nil
		}
	}
	return postgres.Hit{}, postgres.ErrNotFound
}

func (f *fakeCatalogue) ExhibitionCoverage(context.Context, float64, float64, float64) (postgres.Coverage, error) {
	return f.coverage, f.err
}

func (f *fakeCatalogue) ExhibitionsNearby(_ context.Context, _, _, radiusKm float64, upcoming bool, limit int) ([]postgres.ExhibitionHit, error) {
	f.lastRadiusKm, f.lastLimit, f.lastUpcoming = radiusKm, limit, upcoming
	return f.exhibitions, f.err
}

func (f *fakeCatalogue) Counts(context.Context) (postgres.Counts, error) { return f.counts, f.err }
func (f *fakeCatalogue) Ping(context.Context) error                      { return f.err }

func get(t *testing.T, c Catalogue, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	NewServer(c).Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestMuseums_ReturnsResults(t *testing.T) {
	c := &fakeCatalogue{nearby: []postgres.Hit{
		{Museum: models.Museum{Name: "Louvre Museum", Country: "France", Latitude: 48.86, Longitude: 2.33,
			Website: "https://louvre.fr"}, DistanceKm: 1.234},
	}}

	rec := get(t, c, "/v1/museums?lat=48.8566&lon=2.3522&radius_km=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var body museumResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 || body.Museums[0].Name != "Louvre Museum" {
		t.Fatalf("body = %+v", body)
	}
	if body.Museums[0].DistanceKm != 1.23 {
		t.Errorf("distance = %v, want it rounded to 1.23", body.Museums[0].DistanceKm)
	}
	if body.Query.RadiusKm != 2 {
		t.Errorf("query echo radius = %v, want 2", body.Query.RadiusKm)
	}
}

func TestMuseums_EmptyResultIsAnArray(t *testing.T) {
	rec := get(t, &fakeCatalogue{}, "/v1/museums?lat=0&lon=0")

	var body museumResponse
	json.Unmarshal(rec.Body.Bytes(), &body)
	// An empty result must serialise as [] rather than null, or clients break.
	if body.Museums == nil {
		t.Errorf("museums should be an empty array, got %s", rec.Body)
	}
}

func TestQueryValidation(t *testing.T) {
	c := &fakeCatalogue{}

	cases := []struct {
		name   string
		target string
		want   int
	}{
		{name: "missing lat", target: "/v1/museums?lon=2.35", want: http.StatusBadRequest},
		{name: "missing lon", target: "/v1/museums?lat=48.85", want: http.StatusBadRequest},
		{name: "lat out of range", target: "/v1/museums?lat=91&lon=2.35", want: http.StatusBadRequest},
		{name: "lon out of range", target: "/v1/museums?lat=48.85&lon=181", want: http.StatusBadRequest},
		{name: "non-numeric", target: "/v1/museums?lat=north&lon=2.35", want: http.StatusBadRequest},
		{name: "zero radius", target: "/v1/museums?lat=48.85&lon=2.35&radius_km=0", want: http.StatusBadRequest},
		// An unbounded radius would ask the database for the world.
		{name: "radius over the cap", target: "/v1/museums?lat=48.85&lon=2.35&radius_km=5000", want: http.StatusBadRequest},
		{name: "bad limit", target: "/v1/museums?lat=48.85&lon=2.35&limit=0", want: http.StatusBadRequest},
		{name: "search without q", target: "/v1/search", want: http.StatusBadRequest},
		{name: "valid", target: "/v1/museums?lat=48.85&lon=2.35", want: http.StatusOK},
		{name: "valid search", target: "/v1/search?q=louvre", want: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := get(t, c, tc.target); rec.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestLimitIsClampedBeforeReachingTheDatabase(t *testing.T) {
	c := &fakeCatalogue{}
	get(t, c, "/v1/museums?lat=48.85&lon=2.35&limit=99999")

	if c.lastLimit != maxLimit {
		t.Errorf("limit reached the query as %d, want it clamped to %d", c.lastLimit, maxLimit)
	}
}

func TestSearch_ReportsLocatability(t *testing.T) {
	c := &fakeCatalogue{search: []postgres.Hit{
		{Museum: models.Museum{Name: "Musée du cheminot", Country: "France"}, Score: 1.5},
		{Museum: models.Museum{Name: "Louvre", Latitude: 48.86, Longitude: 2.33}, Score: 2.5},
	}}

	rec := get(t, c, "/v1/search?q=musee")
	var body searchResponse
	json.Unmarshal(rec.Body.Bytes(), &body)

	if body.Count != 2 {
		t.Fatalf("count = %d", body.Count)
	}
	// A quarter of the catalogue has no position; a caller plotting a map has
	// to be able to tell which.
	if body.Museums[0].Locatable {
		t.Error("a record with no coordinates was reported as locatable")
	}
	if !body.Museums[1].Locatable {
		t.Error("a record with coordinates was reported as unlocatable")
	}
}

func TestExhibitions_UpcomingIsOptIn(t *testing.T) {
	c := &fakeCatalogue{}

	get(t, c, "/v1/exhibitions?lat=48.85&lon=2.35")
	if c.lastUpcoming {
		t.Error("upcoming exhibitions were included by default")
	}

	get(t, c, "/v1/exhibitions?lat=48.85&lon=2.35&upcoming=true")
	if !c.lastUpcoming {
		t.Error("upcoming=true was not passed through")
	}
}

func TestDatabaseFailureIsNotAnEmptyResult(t *testing.T) {
	c := &fakeCatalogue{err: errors.New("connection refused")}

	// A broken database must not be reported as "no museums here".
	if rec := get(t, c, "/v1/museums?lat=48.85&lon=2.35"); rec.Code != http.StatusBadGateway {
		t.Errorf("museums status = %d, want 502", rec.Code)
	}
	if rec := get(t, c, "/v1/search?q=louvre"); rec.Code != http.StatusBadGateway {
		t.Errorf("search status = %d, want 502", rec.Code)
	}
	if rec := get(t, c, "/health"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("health status = %d, want 503", rec.Code)
	}
}

func TestHealth_ReportsWhatTheCatalogueHolds(t *testing.T) {
	updated := time.Now()
	c := &fakeCatalogue{counts: postgres.Counts{
		Museums: 86052, WithCoordinates: 65337, Countries: 293, Exhibitions: 55,
		LastUpdated: &updated,
	}}

	rec := get(t, c, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)

	// An empty catalogue answers every query with nothing and no error, which
	// is indistinguishable from "there are no museums here" unless the counts
	// are reported.
	if body["status"] != "ok" || body["museums"].(float64) != 86052 {
		t.Errorf("body = %v", body)
	}
}
