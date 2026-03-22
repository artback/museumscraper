package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExhibitionRepository provides CRUD and query operations for exhibitions.
type ExhibitionRepository struct {
	pool *pgxpool.Pool
}

// NewExhibitionRepository creates a new ExhibitionRepository backed by the given connection pool.
func NewExhibitionRepository(pool *pgxpool.Pool) *ExhibitionRepository {
	return &ExhibitionRepository{pool: pool}
}

// exhibitionColumns lists the columns selected in standard exhibition queries.
const exhibitionColumns = `e.id, e.museum_id, e.title, e.description, e.start_date, e.end_date, e.is_permanent, e.source_url`

// scanExhibitionRow is a pgx.RowToFunc for use with pgx.CollectRows.
func scanExhibitionRow(row pgx.CollectableRow) (Exhibition, error) {
	var e Exhibition
	err := row.Scan(
		&e.ID, &e.MuseumID, &e.Title, &e.Description,
		&e.StartDate, &e.EndDate, &e.IsPermanent, &e.SourceURL,
	)
	if err != nil {
		return Exhibition{}, err
	}
	return e, nil
}

// ExhibitionWithMuseum pairs an exhibition with its museum name and optional distance.
type ExhibitionWithMuseum struct {
	Exhibition
	MuseumName string   `json:"museum_name"`
	Distance   *float64 `json:"distance_meters,omitempty"`
}

// scanExhibitionWithMuseumRow scans an exhibition joined with museum name.
func scanExhibitionWithMuseumRow(row pgx.CollectableRow) (ExhibitionWithMuseum, error) {
	var ewm ExhibitionWithMuseum
	err := row.Scan(
		&ewm.ID, &ewm.MuseumID, &ewm.Title, &ewm.Description,
		&ewm.StartDate, &ewm.EndDate, &ewm.IsPermanent, &ewm.SourceURL,
		&ewm.MuseumName,
	)
	if err != nil {
		return ExhibitionWithMuseum{}, err
	}
	return ewm, nil
}

// scanExhibitionWithMuseumDistanceRow scans an exhibition joined with museum name and distance.
func scanExhibitionWithMuseumDistanceRow(row pgx.CollectableRow) (ExhibitionWithMuseum, error) {
	var ewm ExhibitionWithMuseum
	err := row.Scan(
		&ewm.ID, &ewm.MuseumID, &ewm.Title, &ewm.Description,
		&ewm.StartDate, &ewm.EndDate, &ewm.IsPermanent, &ewm.SourceURL,
		&ewm.MuseumName, &ewm.Distance,
	)
	if err != nil {
		return ExhibitionWithMuseum{}, err
	}
	return ewm, nil
}

// activeExhibitionCondition is the WHERE clause fragment for active exhibitions.
const activeExhibitionCondition = `(e.is_permanent = true OR e.end_date IS NULL OR e.end_date >= CURRENT_DATE)`

// Create inserts a new exhibition and returns its ID.
func (r *ExhibitionRepository) Create(ctx context.Context, e Exhibition) (int64, error) {
	query := `
		INSERT INTO exhibitions (museum_id, title, description, start_date, end_date, is_permanent, source_url)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`

	var id int64
	err := r.pool.QueryRow(ctx, query,
		e.MuseumID, e.Title, e.Description, e.StartDate, e.EndDate, e.IsPermanent, e.SourceURL,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("creating exhibition %q for museum %d: %w", e.Title, e.MuseumID, err)
	}
	return id, nil
}

// FindActiveByMuseum returns current exhibitions for a given museum.
// An exhibition is active if it is permanent, has no end date, or its end date
// is today or in the future.
func (r *ExhibitionRepository) FindActiveByMuseum(ctx context.Context, museumID int64) ([]Exhibition, error) {
	query := fmt.Sprintf(
		`SELECT %s FROM exhibitions e WHERE e.museum_id = $1 AND %s ORDER BY e.start_date DESC NULLS LAST`,
		exhibitionColumns, activeExhibitionCondition,
	)

	rows, err := r.pool.Query(ctx, query, museumID)
	if err != nil {
		return nil, fmt.Errorf("querying active exhibitions for museum %d: %w", museumID, err)
	}
	exhibitions, err := pgx.CollectRows(rows, scanExhibitionRow)
	if err != nil {
		return nil, fmt.Errorf("collecting active exhibitions for museum %d: %w", museumID, err)
	}
	return exhibitions, nil
}

// FindActiveInCity returns all active exhibitions at museums in the given city
// (case-insensitive ILIKE match). Results are joined with the museums table to
// include the museum name.
func (r *ExhibitionRepository) FindActiveInCity(ctx context.Context, city string) ([]ExhibitionWithMuseum, error) {
	query := fmt.Sprintf(`
		SELECT %s, m.name AS museum_name
		FROM exhibitions e
		JOIN museums m ON m.id = e.museum_id
		WHERE m.city ILIKE $1 AND %s
		ORDER BY m.name, e.start_date DESC NULLS LAST`,
		exhibitionColumns, activeExhibitionCondition,
	)

	rows, err := r.pool.Query(ctx, query, city)
	if err != nil {
		return nil, fmt.Errorf("querying active exhibitions in city %q: %w", city, err)
	}
	results, err := pgx.CollectRows(rows, scanExhibitionWithMuseumRow)
	if err != nil {
		return nil, fmt.Errorf("collecting active exhibitions in city %q: %w", city, err)
	}
	return results, nil
}

// FindActiveNearby returns active exhibitions at museums within radiusMeters of
// the given coordinates. Results include the museum name and distance in meters.
func (r *ExhibitionRepository) FindActiveNearby(ctx context.Context, lat, lon, radiusMeters float64, limit int) ([]ExhibitionWithMuseum, error) {
	query := fmt.Sprintf(`
		SELECT %s, m.name AS museum_name,
			ST_Distance(m.location, ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography) AS distance_meters
		FROM exhibitions e
		JOIN museums m ON m.id = e.museum_id
		WHERE m.location IS NOT NULL
			AND ST_DWithin(m.location, ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography, $3)
			AND %s
		ORDER BY distance_meters, m.name, e.start_date DESC NULLS LAST
		LIMIT $4`,
		exhibitionColumns, activeExhibitionCondition,
	)

	rows, err := r.pool.Query(ctx, query, lon, lat, radiusMeters, limit)
	if err != nil {
		return nil, fmt.Errorf("querying active exhibitions nearby (%.6f, %.6f, %.0fm): %w", lat, lon, radiusMeters, err)
	}
	results, err := pgx.CollectRows(rows, scanExhibitionWithMuseumDistanceRow)
	if err != nil {
		return nil, fmt.Errorf("collecting active exhibitions nearby: %w", err)
	}
	return results, nil
}
