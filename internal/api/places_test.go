package api

import (
	"context"
	"errors"
	"testing"

	"museum/internal/postgres"
	"museum/pkg/location"
)

// fakeCache is an in-memory stand-in for the places table.
type fakeCache struct {
	entries map[string]postgres.Place
	lookups int
	saves   int
}

func newFakeCache() *fakeCache {
	return &fakeCache{entries: make(map[string]postgres.Place)}
}

func (c *fakeCache) LookupPlace(_ context.Context, query string) (postgres.Place, bool, error) {
	c.lookups++
	place, ok := c.entries[query]
	return place, ok, nil
}

func (c *fakeCache) SavePlace(_ context.Context, place postgres.Place) error {
	c.saves++
	c.entries[place.Query] = place
	return nil
}

func TestPlaceResolver_ResolvesAndCaches(t *testing.T) {
	cache := newFakeCache()
	calls := 0
	geocode := func(context.Context, string) (*location.NominatimLocation, error) {
		calls++
		return &location.NominatimLocation{
			Lat: "48.8566", Lon: "2.3522",
			DisplayName: "Paris, Ile-de-France, France",
			BoundingBox: []string{"48.8156", "48.9022", "2.2242", "2.4699"},
		}, nil
	}

	resolver := NewPlaceResolver(cache, geocode)
	ctx := context.Background()

	first, err := resolver.Resolve(ctx, "Paris, France")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if first.Latitude != 48.8566 || first.Longitude != 2.3522 {
		t.Errorf("coordinates = %v, %v", first.Latitude, first.Longitude)
	}
	// The bounding box is about 9.6 km tall and 18 km wide at this latitude, so
	// the radius should cover the city rather than fall back to the 3 km default.
	if first.RadiusKm < 5 || first.RadiusKm > maxPlaceRadiusKm {
		t.Errorf("radius = %.1f km, want a city-sized radius from the bounding box", first.RadiusKm)
	}

	// A second call for the same place, spelled differently, must not reach the
	// geocoder: it allows one request per second, so repeated lookups on a
	// request path would make the rate limiter decide the API's latency.
	second, err := resolver.Resolve(ctx, "paris france")
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if calls != 1 {
		t.Errorf("geocoder called %d times, want 1 — the cache did not answer", calls)
	}
	if second.Latitude != first.Latitude {
		t.Errorf("cached answer differs: %v vs %v", second.Latitude, first.Latitude)
	}
}

// A name the geocoder cannot resolve is cached as firmly as one it can.
// Without this, a misspelling retried in a loop spends the whole rate budget.
func TestPlaceResolver_CachesFailures(t *testing.T) {
	cache := newFakeCache()
	calls := 0
	geocode := func(context.Context, string) (*location.NominatimLocation, error) {
		calls++
		return nil, location.ErrNoResults
	}

	resolver := NewPlaceResolver(cache, geocode)
	ctx := context.Background()

	for range 3 {
		_, err := resolver.Resolve(ctx, "Nowherecityville")
		if !errors.Is(err, postgres.ErrPlaceUnknown) {
			t.Fatalf("error = %v, want ErrPlaceUnknown", err)
		}
	}
	if calls != 1 {
		t.Errorf("geocoder called %d times for a known-bad name, want 1", calls)
	}
}

// A geocoder that is down is a server failure, not a missing place: the caller
// should retry, and the name must not be cached as unknown.
func TestPlaceResolver_DoesNotCacheTransportFailures(t *testing.T) {
	cache := newFakeCache()
	geocode := func(context.Context, string) (*location.NominatimLocation, error) {
		return nil, errors.New("connection refused")
	}

	resolver := NewPlaceResolver(cache, geocode)

	_, err := resolver.Resolve(context.Background(), "Paris")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, postgres.ErrPlaceUnknown) {
		t.Error("an unreachable geocoder must not be reported as an unknown place")
	}
	if cache.saves != 0 {
		t.Errorf("cached %d entries after a transport failure, want 0", cache.saves)
	}
}

// Without a bounding box there is nothing to size the search from, so the
// request falls back to the default radius rather than guessing.
func TestPlaceResolver_FallsBackWhenNoBoundingBox(t *testing.T) {
	cache := newFakeCache()
	geocode := func(context.Context, string) (*location.NominatimLocation, error) {
		return &location.NominatimLocation{Lat: "48.8566", Lon: "2.3522", DisplayName: "Paris"}, nil
	}

	place, err := NewPlaceResolver(cache, geocode).Resolve(context.Background(), "Paris")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if place.RadiusKm != defaultRadiusKm {
		t.Errorf("radius = %.1f, want the %d km default", place.RadiusKm, defaultRadiusKm)
	}
}

// A country-sized box must not turn into a country-sized query.
func TestPlaceResolver_ClampsHugeBoundingBox(t *testing.T) {
	cache := newFakeCache()
	geocode := func(context.Context, string) (*location.NominatimLocation, error) {
		return &location.NominatimLocation{
			Lat: "46.6", Lon: "2.3", DisplayName: "France",
			BoundingBox: []string{"41.3", "51.1", "-5.1", "9.6"},
		}, nil
	}

	place, err := NewPlaceResolver(cache, geocode).Resolve(context.Background(), "France")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if place.RadiusKm != maxPlaceRadiusKm {
		t.Errorf("radius = %.1f km, want it clamped to %d km", place.RadiusKm, maxPlaceRadiusKm)
	}
}
