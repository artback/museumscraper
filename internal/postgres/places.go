package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// placeTTL is how long a resolved place is trusted. Cities do not move, so this
// is about picking up corrections upstream rather than about staleness.
const placeTTL = 30 * 24 * time.Hour

// Bounds for a place resolved from the catalogue's own towns rather than from
// the geocoder.
const (
	// minLocalityScore is how well a town name must match before it is trusted.
	//
	// Measured rather than guessed. Correct resolutions score well clear of it
	// ("gothenborg" 0.571, "amsterdaam" 0.750) while the near-misses this must
	// refuse fall below ("goteborg" onto Götene 0.333, "munchen" onto
	// Schwabmünchen 0.375). Those two are no loss: the geocoder resolves both
	// spellings itself, so the fallback never sees them.
	minLocalityScore = 0.45

	minLocalityRadiusKm = 2.0
	maxLocalityRadiusKm = 25.0
)

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

// LocalityPlace resolves a place name against the towns the catalogue already
// knows, and is the fallback for a name the geocoder cannot resolve.
//
// The geocoder is exact: "gothenborg" returns nothing, while a search for a
// museum spelled that badly finds it, because the museum index matches on
// trigrams. That asymmetry is the whole reason this exists — the catalogue
// holds tens of thousands of town names and can already match them the
// forgiving way.
//
// The town is matched with word_similarity so a country in the query does not
// spoil it: "gothenborg sweden" has to match the extent "gothenborg" rather
// than the whole string. The centre is the centroid of that town's museums and
// the radius covers them, so the answer is drawn from the same data the query
// will search.
func (s *Store) LocalityPlace(ctx context.Context, query string) (Place, error) {
	// Two details decide whether this is safe.
	//
	// Similarity is whole-string against the town's leading word, not
	// word_similarity against the whole name. word_similarity scores the best
	// matching *extent*, which favours short names: against "gothenborg" it
	// ranked Gotha in Germany (0.667) above Gothenburg (0.636), so the fallback
	// would have answered confidently with the wrong country. Whole-string
	// similarity penalises the length mismatch, which is exactly right for a
	// town name, and reverses the order (0.308 against 0.571). The leading word
	// is taken because the catalogue stores administrative forms —
	// "Gothenburg Municipality", "4th arrondissement of Paris".
	//
	// A country named in the query filters the candidates and is removed from
	// the term, so "gothenborg sweden" is matched as "gothenborg" within Sweden
	// rather than as a whole string that resembles neither.
	const stmt = `
WITH q AS (SELECT $1::text AS term),
named AS (
    SELECT lower(country) AS name
    FROM museums, q
    WHERE country IS NOT NULL AND country <> ''
      AND q.term ~ ('\m' || lower(country) || '\M')
    GROUP BY 1
    -- The longest match wins, so "guinea-bissau" is not read as "guinea".
    ORDER BY length(lower(country)) DESC
    LIMIT 1
),
residual AS (
    SELECT btrim(regexp_replace(q.term,
               coalesce((SELECT '\m' || name || '\M' FROM named), '$^'), ' ', 'g')) AS term,
           (SELECT name FROM named) AS country
    FROM q
),
best AS (
    SELECT m.locality,
           ST_Centroid(ST_Collect(m.location::geometry))::geography AS centre,
           count(*) AS museums,
           max(similarity(split_part(m.locality_normalized, ' ', 1), r.term)) AS score
    FROM museums m, residual r
    WHERE m.location IS NOT NULL
      AND m.locality_normalized <> ''
      AND r.term <> ''
      AND (r.country IS NULL OR lower(m.country) = r.country)
      AND split_part(m.locality_normalized, ' ', 1) % r.term
    GROUP BY m.locality
    -- Most museums breaks a tie between equally-named towns: a query naming a
    -- city means the city, not a hamlet that shares its spelling.
    ORDER BY score DESC, museums DESC
    LIMIT 1
)
SELECT b.locality, ST_Y(b.centre::geometry), ST_X(b.centre::geometry), b.score,
       coalesce(max(ST_Distance(m.location, b.centre)) / 1000.0, 0)
FROM best b
JOIN museums m ON m.locality = b.locality AND m.location IS NOT NULL
GROUP BY b.locality, b.centre, b.score`

	var (
		place  Place
		score  float64
		spread float64
	)
	err := s.pool.QueryRow(ctx, stmt, query).Scan(
		&place.DisplayName, &place.Latitude, &place.Longitude, &score, &spread)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Place{}, ErrPlaceUnknown
	case err != nil:
		return Place{}, fmt.Errorf("locality place %q: %w", query, err)
	}

	// A weak match is worse than no match: resolving nonsense to a real town
	// answers confidently with the wrong place, which is harder to notice than
	// a 404.
	if score < minLocalityScore {
		return Place{}, ErrPlaceUnknown
	}

	place.Query = query
	place.Found = true
	place.RadiusKm = math.Min(math.Max(spread, minLocalityRadiusKm), maxLocalityRadiusKm)
	return place, nil
}
