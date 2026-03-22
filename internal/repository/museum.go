package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Museum represents a museum record in the database.
type Museum struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	Country      string            `json:"country"`
	City         *string           `json:"city,omitempty"`
	State        *string           `json:"state,omitempty"`
	Address      *string           `json:"address,omitempty"`
	Lat          *float64          `json:"lat,omitempty"`
	Lon          *float64          `json:"lon,omitempty"`
	OsmID        *int64            `json:"osm_id,omitempty"`
	OsmType      *string           `json:"osm_type,omitempty"`
	Category     *string           `json:"category,omitempty"`
	MuseumType   *string           `json:"museum_type,omitempty"`
	WikipediaURL *string           `json:"wikipedia_url,omitempty"`
	Website      *string           `json:"website,omitempty"`
	RawTags      map[string]string `json:"raw_tags,omitempty"`
	Distance     *float64          `json:"distance_meters,omitempty"`
	Exhibitions  []Exhibition      `json:"exhibitions,omitempty"`
}

// MuseumRepository provides CRUD and spatial query operations for museums.
type MuseumRepository struct {
	pool *pgxpool.Pool
}

// NewMuseumRepository creates a new MuseumRepository backed by the given connection pool.
func NewMuseumRepository(pool *pgxpool.Pool) *MuseumRepository {
	return &MuseumRepository{pool: pool}
}

// museumColumns lists the columns selected in standard museum queries.
const museumColumns = `id, name, country, city, state, address, lat, lon,
	osm_id, osm_type, category, museum_type, wikipedia_url, website, raw_tags`

// scanMuseumFromRow scans a single museum from a pgx.Row (QueryRow result).
func scanMuseumFromRow(row pgx.Row) (Museum, error) {
	var m Museum
	var rawTags []byte
	err := row.Scan(
		&m.ID, &m.Name, &m.Country, &m.City, &m.State, &m.Address,
		&m.Lat, &m.Lon, &m.OsmID, &m.OsmType, &m.Category,
		&m.MuseumType, &m.WikipediaURL, &m.Website, &rawTags,
	)
	if err != nil {
		return Museum{}, err
	}
	if rawTags != nil {
		if err := json.Unmarshal(rawTags, &m.RawTags); err != nil {
			return Museum{}, fmt.Errorf("unmarshalling raw_tags: %w", err)
		}
	}
	return m, nil
}

// scanMuseumRow is a pgx.RowToFunc for use with pgx.CollectRows.
func scanMuseumRow(row pgx.CollectableRow) (Museum, error) {
	var m Museum
	var rawTags []byte
	err := row.Scan(
		&m.ID, &m.Name, &m.Country, &m.City, &m.State, &m.Address,
		&m.Lat, &m.Lon, &m.OsmID, &m.OsmType, &m.Category,
		&m.MuseumType, &m.WikipediaURL, &m.Website, &rawTags,
	)
	if err != nil {
		return Museum{}, err
	}
	if rawTags != nil {
		if err := json.Unmarshal(rawTags, &m.RawTags); err != nil {
			return Museum{}, fmt.Errorf("unmarshalling raw_tags: %w", err)
		}
	}
	return m, nil
}

// museumWithDistanceRow scans a museum row that also includes a trailing distance column.
func museumWithDistanceRow(row pgx.CollectableRow) (Museum, error) {
	var m Museum
	var rawTags []byte
	err := row.Scan(
		&m.ID, &m.Name, &m.Country, &m.City, &m.State, &m.Address,
		&m.Lat, &m.Lon, &m.OsmID, &m.OsmType, &m.Category,
		&m.MuseumType, &m.WikipediaURL, &m.Website, &rawTags,
		&m.Distance,
	)
	if err != nil {
		return Museum{}, err
	}
	if rawTags != nil {
		if err := json.Unmarshal(rawTags, &m.RawTags); err != nil {
			return Museum{}, fmt.Errorf("unmarshalling raw_tags: %w", err)
		}
	}
	return m, nil
}

// marshalRawTags converts a map[string]string to JSON bytes for database storage.
func marshalRawTags(tags map[string]string) ([]byte, error) {
	if tags == nil {
		return nil, nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("marshalling raw_tags: %w", err)
	}
	return b, nil
}

// upsertQuery is the SQL used for single and batch museum upserts.
const upsertQuery = `
	INSERT INTO museums (name, country, city, state, address, lat, lon, location,
		osm_id, osm_type, category, museum_type, wikipedia_url, website, raw_tags)
	VALUES (
		$1, $2, $3, $4, $5, $6, $7,
		CASE WHEN $6::double precision IS NOT NULL AND $7::double precision IS NOT NULL
			THEN ST_SetSRID(ST_MakePoint($7, $6), 4326)::geography
			ELSE NULL
		END,
		$8, $9, $10, $11, $12, $13, $14
	)
	ON CONFLICT (name, country) DO UPDATE SET
		city         = COALESCE(EXCLUDED.city, museums.city),
		state        = COALESCE(EXCLUDED.state, museums.state),
		address      = COALESCE(EXCLUDED.address, museums.address),
		lat          = COALESCE(EXCLUDED.lat, museums.lat),
		lon          = COALESCE(EXCLUDED.lon, museums.lon),
		location     = CASE
			WHEN EXCLUDED.lat IS NOT NULL AND EXCLUDED.lon IS NOT NULL
			THEN ST_SetSRID(ST_MakePoint(EXCLUDED.lon, EXCLUDED.lat), 4326)::geography
			ELSE museums.location
		END,
		osm_id       = COALESCE(EXCLUDED.osm_id, museums.osm_id),
		osm_type     = COALESCE(EXCLUDED.osm_type, museums.osm_type),
		category     = COALESCE(EXCLUDED.category, museums.category),
		museum_type  = COALESCE(EXCLUDED.museum_type, museums.museum_type),
		wikipedia_url = COALESCE(EXCLUDED.wikipedia_url, museums.wikipedia_url),
		website      = COALESCE(EXCLUDED.website, museums.website),
		raw_tags     = COALESCE(EXCLUDED.raw_tags, museums.raw_tags),
		updated_at   = NOW()
	RETURNING id`

// UpsertMuseum inserts or updates a museum keyed on (name, country). When lat/lon
// are provided, the PostGIS location column is also set. Returns the museum ID.
func (r *MuseumRepository) UpsertMuseum(ctx context.Context, m Museum) (int64, error) {
	rawTagsJSON, err := marshalRawTags(m.RawTags)
	if err != nil {
		return 0, err
	}

	var id int64
	err = r.pool.QueryRow(ctx, upsertQuery,
		m.Name, m.Country, m.City, m.State, m.Address, m.Lat, m.Lon,
		m.OsmID, m.OsmType, m.Category, m.MuseumType, m.WikipediaURL, m.Website, rawTagsJSON,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upserting museum %q in %q: %w", m.Name, m.Country, err)
	}
	return id, nil
}

// batchSize controls how many rows are sent per pgx.Batch round-trip.
const batchSize = 100

// UpsertMuseumBatch inserts or updates museums in batches of 100 using pgx.Batch
// for performance. Returns a slice of IDs in the same order as the input.
func (r *MuseumRepository) UpsertMuseumBatch(ctx context.Context, museums []Museum) ([]int64, error) {
	ids := make([]int64, 0, len(museums))

	for start := 0; start < len(museums); start += batchSize {
		end := start + batchSize
		if end > len(museums) {
			end = len(museums)
		}
		chunk := museums[start:end]

		batch := &pgx.Batch{}
		for _, m := range chunk {
			rawTagsJSON, err := marshalRawTags(m.RawTags)
			if err != nil {
				return nil, fmt.Errorf("preparing batch for %q: %w", m.Name, err)
			}
			batch.Queue(upsertQuery,
				m.Name, m.Country, m.City, m.State, m.Address, m.Lat, m.Lon,
				m.OsmID, m.OsmType, m.Category, m.MuseumType, m.WikipediaURL, m.Website, rawTagsJSON,
			)
		}

		br := r.pool.SendBatch(ctx, batch)
		for i := range chunk {
			var id int64
			if err := br.QueryRow().Scan(&id); err != nil {
				br.Close()
				return nil, fmt.Errorf("batch upsert museum %q (index %d): %w", chunk[i].Name, start+i, err)
			}
			ids = append(ids, id)
		}
		if err := br.Close(); err != nil {
			return nil, fmt.Errorf("closing batch: %w", err)
		}
	}

	return ids, nil
}

// FindByCity returns all museums in the given city (case-insensitive ILIKE match).
func (r *MuseumRepository) FindByCity(ctx context.Context, city string) ([]Museum, error) {
	query := fmt.Sprintf(`SELECT %s FROM museums WHERE city ILIKE $1 ORDER BY name`, museumColumns)
	rows, err := r.pool.Query(ctx, query, city)
	if err != nil {
		return nil, fmt.Errorf("querying museums by city %q: %w", city, err)
	}
	museums, err := pgx.CollectRows(rows, scanMuseumRow)
	if err != nil {
		return nil, fmt.Errorf("collecting museums by city %q: %w", city, err)
	}
	return museums, nil
}

// FindNearby returns museums within radiusMeters of the given coordinates,
// ordered by distance. Each result includes Distance in meters.
func (r *MuseumRepository) FindNearby(ctx context.Context, lat, lon, radiusMeters float64, limit int) ([]Museum, error) {
	query := fmt.Sprintf(`
		SELECT %s,
			ST_Distance(location, ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography) AS distance_meters
		FROM museums
		WHERE location IS NOT NULL
		  AND ST_DWithin(location, ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography, $3)
		ORDER BY distance_meters
		LIMIT $4`, museumColumns)

	rows, err := r.pool.Query(ctx, query, lon, lat, radiusMeters, limit)
	if err != nil {
		return nil, fmt.Errorf("querying nearby museums (%.6f, %.6f, %.0fm): %w", lat, lon, radiusMeters, err)
	}
	results, err := pgx.CollectRows(rows, museumWithDistanceRow)
	if err != nil {
		return nil, fmt.Errorf("collecting nearby museums: %w", err)
	}
	return results, nil
}

// FindByCountry returns all museums in the given country.
func (r *MuseumRepository) FindByCountry(ctx context.Context, country string) ([]Museum, error) {
	query := fmt.Sprintf(`SELECT %s FROM museums WHERE country = $1 ORDER BY name`, museumColumns)
	rows, err := r.pool.Query(ctx, query, country)
	if err != nil {
		return nil, fmt.Errorf("querying museums by country %q: %w", country, err)
	}
	museums, err := pgx.CollectRows(rows, scanMuseumRow)
	if err != nil {
		return nil, fmt.Errorf("collecting museums by country %q: %w", country, err)
	}
	return museums, nil
}

// SearchByName performs a trigram-similarity search on museum names using the
// pg_trgm extension. Results are ordered by descending similarity.
func (r *MuseumRepository) SearchByName(ctx context.Context, query string, limit int) ([]Museum, error) {
	sql := fmt.Sprintf(`
		SELECT %s
		FROM museums
		WHERE name %% $1
		ORDER BY similarity(name, $1) DESC, name
		LIMIT $2`, museumColumns)

	rows, err := r.pool.Query(ctx, sql, query, limit)
	if err != nil {
		return nil, fmt.Errorf("searching museums by name %q: %w", query, err)
	}
	museums, err := pgx.CollectRows(rows, scanMuseumRow)
	if err != nil {
		return nil, fmt.Errorf("collecting museum name search results: %w", err)
	}
	return museums, nil
}

// List returns a paginated list of museums ordered by name, along with the total count.
func (r *MuseumRepository) List(ctx context.Context, limit, offset int) ([]Museum, int, error) {
	var total int
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM museums").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting museums: %w", err)
	}

	query := fmt.Sprintf(`SELECT %s FROM museums ORDER BY name LIMIT $1 OFFSET $2`, museumColumns)
	rows, err := r.pool.Query(ctx, query, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("listing museums: %w", err)
	}
	museums, err := pgx.CollectRows(rows, scanMuseumRow)
	if err != nil {
		return nil, 0, fmt.Errorf("collecting museums: %w", err)
	}
	return museums, total, nil
}

// GetByID retrieves a single museum by its primary key.
func (r *MuseumRepository) GetByID(ctx context.Context, id int64) (Museum, error) {
	query := fmt.Sprintf(`SELECT %s FROM museums WHERE id = $1`, museumColumns)
	m, err := scanMuseumFromRow(r.pool.QueryRow(ctx, query, id))
	if err != nil {
		return Museum{}, fmt.Errorf("getting museum by id %d: %w", id, err)
	}
	return m, nil
}
