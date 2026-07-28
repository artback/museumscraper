package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// placeTTL is how long a resolved place is trusted. Cities do not move, so this
// is about picking up corrections upstream rather than about staleness.
const placeTTL = 30 * 24 * time.Hour

// Place is a geocoded location with the extent a query around it should cover.
type Place struct {
	Query       string
	DisplayName string
	Latitude    float64
	Longitude   float64
	RadiusKm    float64
	// Found is false for a name the geocoder could not resolve. The failure is
	// cached as deliberately as a success.
	Found bool
}

// ErrPlaceUnknown reports a name no geocoder could resolve.
var ErrPlaceUnknown = errors.New("place not found")

// LookupPlace returns a cached place. The second result is false when the name
// has not been resolved before, or when the entry has aged out.
func (s *Store) LookupPlace(ctx context.Context, query string) (Place, bool, error) {
	const stmt = `
SELECT display_name, ST_Y(location::geometry), ST_X(location::geometry), radius_km, found
FROM places
WHERE query = $1 AND resolved_at > now() - $2::interval`

	place := Place{Query: query}
	err := s.pool.QueryRow(ctx, stmt, query, placeTTL.String()).Scan(
		&place.DisplayName, &place.Latitude, &place.Longitude, &place.RadiusKm, &place.Found)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Place{}, false, nil
	case err != nil:
		return Place{}, false, fmt.Errorf("lookup place %q: %w", query, err)
	}
	return place, true, nil
}

// SavePlace records a resolution, successful or not.
func (s *Store) SavePlace(ctx context.Context, place Place) error {
	const stmt = `
INSERT INTO places (query, display_name, location, radius_km, found, resolved_at)
VALUES ($1, $2, ST_SetSRID(ST_MakePoint($4::double precision, $3::double precision), 4326)::geography, $5, $6, now())
ON CONFLICT (query) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    location     = EXCLUDED.location,
    radius_km    = EXCLUDED.radius_km,
    found        = EXCLUDED.found,
    resolved_at  = now()`

	if _, err := s.pool.Exec(ctx, stmt, place.Query, place.DisplayName,
		place.Latitude, place.Longitude, place.RadiusKm, place.Found); err != nil {
		return fmt.Errorf("save place %q: %w", place.Query, err)
	}
	return nil
}
