package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"

	"museum/internal/postgres"
	"museum/internal/search"
	"museum/pkg/location"
)

// Radius bounds derived from a geocoded place. A city's bounding box is a
// reasonable search extent; a country's is not, and neither is a doorway.
const (
	minPlaceRadiusKm = 1.0
	maxPlaceRadiusKm = maxRadiusKm
)

// PlaceCache is the persistence a resolver needs. It is an interface so the
// resolver can be tested without a database.
type PlaceCache interface {
	LookupPlace(ctx context.Context, query string) (postgres.Place, bool, error)
	SavePlace(ctx context.Context, place postgres.Place) error
	// LocalityPlace resolves a name against the towns already in the
	// catalogue, which the geocoder's exact matching cannot do.
	LocalityPlace(ctx context.Context, query string) (postgres.Place, error)
}

// Geocoder turns a place name into a location. Satisfied by pkg/location.
type Geocoder func(ctx context.Context, query string) (*location.NominatimLocation, error)

// PlaceResolver turns "Paris" into coordinates, remembering what it learns.
//
// This is what makes the catalogue answerable by the question people actually
// ask. Every endpoint took lat and lon, so "show me exhibitions in Paris"
// required the caller to already know where Paris is — the one thing a
// geocoder is for. Filtering on the stored locality is not a substitute:
// localities arrive as "4th arrondissement of Paris", so matching the name
// found 34 museums where a radius around the city centre finds 123.
type PlaceResolver struct {
	cache    PlaceCache
	geocode  Geocoder
	fallback float64
}

// NewPlaceResolver returns a resolver backed by cache and geocode.
func NewPlaceResolver(cache PlaceCache, geocode Geocoder) *PlaceResolver {
	return &PlaceResolver{cache: cache, geocode: geocode, fallback: defaultRadiusKm}
}

// Resolve returns the place a name refers to.
//
// It answers from the cache whenever it can, including for names that failed
// before: a misspelling retried in a loop would otherwise spend the geocoder's
// entire budget. It returns ErrPlaceUnknown for a name that cannot be resolved.
func (r *PlaceResolver) Resolve(ctx context.Context, name string) (postgres.Place, error) {
	key := search.Normalize(name)
	if key == "" {
		return postgres.Place{}, fmt.Errorf("%w: %q", postgres.ErrPlaceUnknown, name)
	}

	cached, ok, err := r.cache.LookupPlace(ctx, key)
	if err != nil {
		// A cache that cannot be read is not a reason to fail the request; it
		// only costs a geocoder call.
		log.Printf("api: place cache unavailable: %v", err)
	}
	if ok {
		if !cached.Found {
			return postgres.Place{}, fmt.Errorf("%w: %q", postgres.ErrPlaceUnknown, name)
		}
		return cached, nil
	}

	found, err := r.geocode(ctx, name)
	if errors.Is(err, location.ErrNoResults) {
		// The geocoder matches exactly, so a typo returns nothing at all —
		// while a museum search for a name spelled just as badly succeeds,
		// because that index matches trigrams. Falling back to the towns the
		// catalogue already holds closes that gap: "gothenborg" resolves to
		// Gothenburg from our own data.
		if fallback, localErr := r.cache.LocalityPlace(ctx, key); localErr == nil {
			fallback.Query = key
			r.remember(ctx, fallback)
			return fallback, nil
		}
		r.remember(ctx, postgres.Place{Query: key, DisplayName: name, Found: false})
		return postgres.Place{}, fmt.Errorf("%w: %q", postgres.ErrPlaceUnknown, name)
	}
	if err != nil {
		return postgres.Place{}, fmt.Errorf("geocode %q: %w", name, err)
	}

	lat, latErr := strconv.ParseFloat(found.Lat, 64)
	lon, lonErr := strconv.ParseFloat(found.Lon, 64)
	if latErr != nil || lonErr != nil {
		return postgres.Place{}, fmt.Errorf("geocode %q: unusable coordinates %q, %q", name, found.Lat, found.Lon)
	}

	place := postgres.Place{
		Query:       key,
		DisplayName: found.DisplayName,
		Latitude:    lat,
		Longitude:   lon,
		RadiusKm:    r.radiusFor(found, lat),
		Found:       true,
	}
	r.remember(ctx, place)
	return place, nil
}

// remember stores a resolution, treating a write failure as unimportant: the
// answer is already known, and the only cost is resolving it again later.
func (r *PlaceResolver) remember(ctx context.Context, place postgres.Place) {
	if err := r.cache.SavePlace(ctx, place); err != nil {
		log.Printf("api: cannot cache place %q: %v", place.Query, err)
	}
}

// radiusFor sizes a search from the geocoder's bounding box, so a query for a
// city covers the city and a query for a street does not.
//
// The box is "south north west east" in degrees. Latitude converts at a fixed
// 111 km per degree; longitude narrows towards the poles, which matters for the
// Nordic and Canadian cities the catalogue is full of. Half the larger side
// approximates the radius that covers the box.
func (r *PlaceResolver) radiusFor(found *location.NominatimLocation, lat float64) float64 {
	if len(found.BoundingBox) != 4 {
		return r.fallback
	}

	south, err1 := strconv.ParseFloat(found.BoundingBox[0], 64)
	north, err2 := strconv.ParseFloat(found.BoundingBox[1], 64)
	west, err3 := strconv.ParseFloat(found.BoundingBox[2], 64)
	east, err4 := strconv.ParseFloat(found.BoundingBox[3], 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return r.fallback
	}

	const kmPerDegree = 111.0
	heightKm := math.Abs(north-south) * kmPerDegree
	widthKm := math.Abs(east-west) * kmPerDegree * math.Cos(lat*math.Pi/180)

	radius := math.Max(heightKm, widthKm) / 2
	return math.Min(math.Max(radius, minPlaceRadiusKm), maxPlaceRadiusKm)
}
