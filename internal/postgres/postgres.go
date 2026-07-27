// Package postgres stores and queries the catalogue.
//
// It replaces three hand-rolled indexes — a degree-cell geo grid, a sharded
// text index, and the full scans the audit ran — with one system that maintains
// them itself. The grid and the shards were derived data with no invalidation:
// rebuilt by a separate command, silently stale whenever that command had not
// run, and impossible to tell apart from correct results.
//
// It also brings similarity matching, which the hand-rolled index could not do.
// Exact and prefix matching answered "louvre" and "louv" but not "rijkmuseum",
// "guggenhiem" or "van gough" — near-misses that are most of what people type.
package postgres

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"museum/internal/models"
	"museum/internal/search"
	"museum/pkg/exhibitions"
)

//go:embed schema.sql
var schema string

// Store is a connection pool with the catalogue's queries on it.
type Store struct {
	pool *pgxpool.Pool

	countsMu sync.RWMutex
	counts   Counts
	countsAt time.Time
}

// Open connects and applies the schema.
//
// The schema is idempotent and applied on every start rather than through a
// migration tool: there is one schema, it only grows, and a tool would be more
// machinery than the problem needs.
func Open(ctx context.Context, dsn string) (*Store, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	// A crawl writes from several goroutines and the API serves concurrently;
	// the default of one connection per CPU is fine, but a floor avoids
	// serialising on small machines.
	if config.MaxConns < 8 {
		config.MaxConns = 8
	}

	// Trigram thresholds are session settings, so they are applied as each
	// connection is made. Setting them per query was a bug waiting to happen:
	// a pool hands out whichever connection is free, so the SET and the SELECT
	// could land on different ones and the query would run at the default.
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `
SET pg_trgm.similarity_threshold = 0.3;
SET pg_trgm.word_similarity_threshold = 0.55;`)
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// SaveMuseums upserts a batch, returning how many rows were written.
//
// Upserting on identity means a re-crawl updates records in place instead of
// accumulating copies, and a museum that gains a Wikidata id later merges with
// the row that was keyed by name.
func (s *Store) SaveMuseums(ctx context.Context, museums []models.Museum) (int64, error) {
	const stmt = `
INSERT INTO museums (
    wikidata_id, name, normalized, search_text, locality_normalized, country, locality, description,
    website, wikipedia_url, page_id, source_page, aliases, sources, verified, sitelinks, location, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
    CASE WHEN $17::double precision IS NULL THEN NULL
         ELSE ST_SetSRID(ST_MakePoint($18::double precision, $17::double precision), 4326)::geography END,
    now()
)
ON CONFLICT (identity) DO UPDATE SET
    wikidata_id   = coalesce(nullif(EXCLUDED.wikidata_id, ''), museums.wikidata_id),
    name          = EXCLUDED.name,
    normalized    = EXCLUDED.normalized,
    search_text   = EXCLUDED.search_text,
    locality_normalized = EXCLUDED.locality_normalized,
    country       = coalesce(EXCLUDED.country, museums.country),
    locality      = coalesce(nullif(EXCLUDED.locality, ''), museums.locality),
    description   = coalesce(nullif(EXCLUDED.description, ''), museums.description),
    website       = coalesce(nullif(EXCLUDED.website, ''), museums.website),
    wikipedia_url = coalesce(nullif(EXCLUDED.wikipedia_url, ''), museums.wikipedia_url),
    page_id       = coalesce(nullif(EXCLUDED.page_id, 0), museums.page_id),
    source_page   = coalesce(nullif(EXCLUDED.source_page, ''), museums.source_page),
    aliases       = EXCLUDED.aliases,
    sources       = EXCLUDED.sources,
    verified      = museums.verified OR EXCLUDED.verified,
    sitelinks     = greatest(EXCLUDED.sitelinks, museums.sitelinks),
    -- A position already known is not replaced by a null one: enrichment can
    -- add coordinates, and a later crawl without them must not remove them.
    location      = coalesce(EXCLUDED.location, museums.location),
    updated_at    = now()`

	batch := &pgx.Batch{}
	for _, m := range museums {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}

		var lat, lon *float64
		if m.HasCoordinates() {
			lat, lon = &m.Latitude, &m.Longitude
		}

		batch.Queue(stmt,
			m.WikidataID, name, search.Normalize(name), searchText(m), search.Normalize(m.Locality),
			nullIfEmpty(m.Country),
			m.Locality, m.Description, m.Website, m.WikipediaURL, m.PageID,
			m.SourcePage, textArray(m.AlsoKnownAs), textArray(m.Sources), m.Verified,
			m.Sitelinks, lat, lon)
	}
	if batch.Len() == 0 {
		return 0, nil
	}

	results := s.pool.SendBatch(ctx, batch)
	defer results.Close()

	var written int64
	for range batch.Len() {
		tag, err := results.Exec()
		if err != nil {
			return written, fmt.Errorf("save museums: %w", err)
		}
		written += tag.RowsAffected()
	}
	return written, nil
}

// searchText is everything a name query should match, in one normalised string:
// the name, the alternative names, and the town.
//
// Matching those with separate OR'd predicates — in particular an EXISTS over
// the aliases array — gave the planner nothing it could combine, and every
// search fell back to a sequential scan of the whole table. One column with one
// trigram index is answered from the index.
func searchText(m models.Museum) string {
	parts := append([]string{m.Name}, m.AlsoKnownAs...)
	parts = append(parts, m.Locality)
	return search.Normalize(strings.Join(parts, " "))
}

// textArray makes a slice safe to send to a NOT NULL array column. A nil Go
// slice encodes as SQL NULL, not as an empty array, so a museum with no aliases
// would be rejected by the constraint rather than stored with none.
func textArray(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// nullIfEmpty turns the "unknown" placeholder and empty strings into SQL null,
// so a real value from another source can replace them.
func nullIfEmpty(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "unknown") {
		return nil
	}
	return &s
}

// Hit is a museum returned by a query, with why it matched.
type Hit struct {
	Museum models.Museum
	// DistanceKm is set by radius queries.
	DistanceKm float64
	// Score is set by search queries.
	Score float64
}

// Nearby returns the museums within radiusKm of a point, nearest first.
func (s *Store) Nearby(ctx context.Context, lat, lon, radiusKm float64, limit int) ([]Hit, error) {
	const stmt = `
SELECT name, coalesce(country,''), coalesce(locality,''), coalesce(description,''),
       coalesce(website,''), coalesce(wikipedia_url,''), coalesce(wikidata_id,''),
       aliases, sources, verified,
       ST_Y(location::geometry), ST_X(location::geometry),
       ST_Distance(location, $1::geography) / 1000.0 AS distance_km
FROM museums
WHERE location IS NOT NULL
  AND ST_DWithin(location, $1::geography, $2)
ORDER BY location <-> $1::geography
LIMIT $3`

	point := fmt.Sprintf("SRID=4326;POINT(%v %v)", lon, lat)

	rows, err := s.pool.Query(ctx, stmt, point, radiusKm*1000, limit)
	if err != nil {
		return nil, fmt.Errorf("nearby: %w", err)
	}
	defer rows.Close()

	return scanHits(rows, true)
}

// Search returns the museums whose name, aliases or locality match a query,
// best first.
//
// Several kinds of match are combined. An exact or prefix match on the
// normalised name is what a correctly typed query produces. Whole-string
// trigram similarity catches a near-miss on a short name: "rijkmuseum" shares
// almost all its trigrams with "rijksmuseum".
//
// Word similarity catches the rest. Whole-string similarity compares the query
// against the entire name, so "guggenhiem" scored far too low against "Solomon
// R. Guggenheim Museum" to match at all — the name is nearly three times as
// long. word_similarity measures the query against the best matching run of
// words inside the name, which is what a person means when they type one word
// of a museum's title.
//
// Every clause in the WHERE is index-backed. Scoring may scan the rows those
// clauses selected, but nothing may scan the table: an earlier version matched
// query words with position(), which no index can serve, and the query went
// from under two milliseconds to over five hundred.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	normalized := search.Normalize(query)
	if normalized == "" {
		return nil, nil
	}

	const stmt = `
WITH q AS (SELECT $1::text AS term)
SELECT name, coalesce(country,''), coalesce(locality,''), coalesce(description,''),
       coalesce(website,''), coalesce(wikipedia_url,''), coalesce(wikidata_id,''),
       aliases, sources, verified,
       ST_Y(location::geometry), ST_X(location::geometry),
       (
         CASE WHEN normalized = q.term THEN 3.0
              WHEN normalized LIKE q.term || '%' THEN 1.5
              WHEN position(q.term in normalized) > 0 THEN 0.75
              ELSE 0 END
         -- Similarity is measured against the name, not against search_text.
         -- Scoring the concatenation of name, aliases and town matched almost
         -- anything: "van gough museum" reached "Vantaa City Museum".
         + greatest(similarity(normalized, q.term), word_similarity(q.term, normalized))
         -- One cheap comparison, not a scan of the query's words: does the
         -- query mention this museum's town? It is what separates the
         -- Kunsthaus in Zürich from a Kunstmuseum on Fanø.
         + CASE WHEN locality_normalized <> ''
                 AND position(locality_normalized in q.term) > 0
                THEN 1.0 ELSE 0 END
         -- Prominence, on a log scale so it settles ties without overruling a
         -- better name match: the Louvre's 167 language editions score about
         -- 1.0, a museum with a single article about 0.14.
         + 0.2 * ln(1 + sitelinks)
         + CASE WHEN location IS NOT NULL THEN 0.01 ELSE 0 END
       ) AS score
FROM museums, q
WHERE normalized % q.term
   OR q.term <% normalized
   OR normalized LIKE q.term || '%'
   OR search_text % q.term
ORDER BY score DESC, length(normalized), name
LIMIT $2`

	rows, err := s.pool.Query(ctx, stmt, normalized, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	return scanHits(rows, false)
}

// scanHits reads a result set into museums. withDistance says whether the final
// column is a distance or a score.
func scanHits(rows pgx.Rows, withDistance bool) ([]Hit, error) {
	var hits []Hit

	for rows.Next() {
		var (
			hit      Hit
			lat, lon *float64
			last     float64
		)
		if err := rows.Scan(
			&hit.Museum.Name, &hit.Museum.Country, &hit.Museum.Locality,
			&hit.Museum.Description, &hit.Museum.Website, &hit.Museum.WikipediaURL,
			&hit.Museum.WikidataID, &hit.Museum.AlsoKnownAs, &hit.Museum.Sources,
			&hit.Museum.Verified, &lat, &lon, &last,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}

		if lat != nil && lon != nil {
			hit.Museum.Latitude, hit.Museum.Longitude = *lat, *lon
		}
		if withDistance {
			hit.DistanceKm = last
		} else {
			hit.Score = last
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// Counts summarises the catalogue.
type Counts struct {
	Museums         int64
	WithCoordinates int64
	WithWebsite     int64
	Exhibitions     int64
	Countries       int64
	LastUpdated     *time.Time
}

// Counts returns the summary, cached briefly.
//
// The counts are six aggregates over the whole table, about fifty milliseconds
// in total. That is far too expensive for a liveness probe polled every few
// seconds, and the numbers move only when a crawl or a refresh runs, so a short
// cache costs nothing in accuracy.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	s.countsMu.RLock()
	cached, at := s.counts, s.countsAt
	s.countsMu.RUnlock()

	if time.Since(at) < countsTTL {
		return cached, nil
	}

	counts, err := s.countNow(ctx)
	if err != nil {
		return counts, err
	}

	s.countsMu.Lock()
	s.counts, s.countsAt = counts, time.Now()
	s.countsMu.Unlock()

	return counts, nil
}

// countsTTL is how long a summary is reused.
const countsTTL = 30 * time.Second

// countNow runs the aggregates.
func (s *Store) countNow(ctx context.Context) (Counts, error) {
	const stmt = `
SELECT
    (SELECT count(*) FROM museums),
    (SELECT count(*) FROM museums WHERE location IS NOT NULL),
    (SELECT count(*) FROM museums WHERE coalesce(website,'') <> ''),
    (SELECT count(*) FROM exhibitions),
    (SELECT count(DISTINCT country) FROM museums WHERE country IS NOT NULL),
    (SELECT max(updated_at) FROM museums)`

	var counts Counts
	err := s.pool.QueryRow(ctx, stmt).Scan(
		&counts.Museums, &counts.WithCoordinates, &counts.WithWebsite,
		&counts.Exhibitions, &counts.Countries, &counts.LastUpdated)
	if err != nil {
		return counts, fmt.Errorf("counts: %w", err)
	}
	return counts, nil
}

// SaveExhibitions upserts scraped listings, keyed by URL.
func (s *Store) SaveExhibitions(ctx context.Context, found []exhibitions.Exhibition) (int64, error) {
	const stmt = `
INSERT INTO exhibitions (url, title, museum, museum_wikidata_id, starts_on, ends_on, location, source_page, scraped_at)
VALUES ($1, $2, $3, $4, $5, $6,
        CASE WHEN $7::double precision IS NULL THEN NULL
             ELSE ST_SetSRID(ST_MakePoint($8::double precision, $7::double precision), 4326)::geography END,
        $9, $10)
ON CONFLICT (url) DO UPDATE SET
    title      = EXCLUDED.title,
    museum     = EXCLUDED.museum,
    starts_on  = EXCLUDED.starts_on,
    ends_on    = EXCLUDED.ends_on,
    location   = coalesce(EXCLUDED.location, exhibitions.location),
    scraped_at = EXCLUDED.scraped_at`

	batch := &pgx.Batch{}
	for _, e := range found {
		if strings.TrimSpace(e.URL) == "" {
			continue
		}
		var lat, lon *float64
		if e.Latitude != 0 || e.Longitude != 0 {
			lat, lon = &e.Latitude, &e.Longitude
		}
		scraped := e.ScrapedAt
		if scraped.IsZero() {
			scraped = time.Now()
		}
		batch.Queue(stmt, e.URL, e.Title, e.Museum, e.MuseumWikidataID,
			e.Start, e.End, lat, lon, e.SourcePage, scraped)
	}
	if batch.Len() == 0 {
		return 0, nil
	}

	results := s.pool.SendBatch(ctx, batch)
	defer results.Close()

	var written int64
	for range batch.Len() {
		tag, err := results.Exec()
		if err != nil {
			return written, fmt.Errorf("save exhibitions: %w", err)
		}
		written += tag.RowsAffected()
	}
	return written, nil
}

// ExhibitionHit is an exhibition near a point.
type ExhibitionHit struct {
	exhibitions.Exhibition
	DistanceKm float64
}

// ExhibitionsNearby returns what is on show within radiusKm, soonest to close
// first — the order a visitor acts on.
//
// Currency is decided by the database against today's date rather than by the
// flags stored when the scrape ran, which may be weeks old.
func (s *Store) ExhibitionsNearby(ctx context.Context, lat, lon, radiusKm float64, includeUpcoming bool, limit int) ([]ExhibitionHit, error) {
	const stmt = `
SELECT url, title, coalesce(museum,''), coalesce(museum_wikidata_id,''),
       starts_on, ends_on, coalesce(source_page,''), scraped_at,
       ST_Y(location::geometry), ST_X(location::geometry),
       ST_Distance(location, $1::geography) / 1000.0 AS distance_km
FROM exhibitions
WHERE location IS NOT NULL
  AND ST_DWithin(location, $1::geography, $2)
  AND (ends_on IS NULL OR ends_on >= current_date)
  AND ($3 OR starts_on IS NULL OR starts_on <= current_date)
ORDER BY ends_on NULLS LAST, distance_km
LIMIT $4`

	point := fmt.Sprintf("SRID=4326;POINT(%v %v)", lon, lat)

	rows, err := s.pool.Query(ctx, stmt, point, radiusKm*1000, includeUpcoming, limit)
	if err != nil {
		return nil, fmt.Errorf("exhibitions nearby: %w", err)
	}
	defer rows.Close()

	var hits []ExhibitionHit
	now := time.Now()
	for rows.Next() {
		var (
			hit      ExhibitionHit
			lat, lon *float64
		)
		if err := rows.Scan(&hit.URL, &hit.Title, &hit.Museum, &hit.MuseumWikidataID,
			&hit.Start, &hit.End, &hit.SourcePage, &hit.ScrapedAt,
			&lat, &lon, &hit.DistanceKm); err != nil {
			return nil, fmt.Errorf("scan exhibition: %w", err)
		}
		if lat != nil && lon != nil {
			hit.Latitude, hit.Longitude = *lat, *lon
		}
		hit.Running = hit.Start == nil || !hit.Start.After(now)
		hit.Upcoming = hit.Start != nil && hit.Start.After(now)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// EachMuseum streams the whole catalogue, for the audit.
func (s *Store) EachMuseum(ctx context.Context, fn func(models.Museum)) error {
	const stmt = `
SELECT name, coalesce(country,''), coalesce(locality,''), coalesce(description,''),
       coalesce(website,''), coalesce(wikipedia_url,''), coalesce(wikidata_id,''),
       aliases, sources, verified,
       ST_Y(location::geometry), ST_X(location::geometry), 0::double precision
FROM museums`

	rows, err := s.pool.Query(ctx, stmt)
	if err != nil {
		return fmt.Errorf("scan catalogue: %w", err)
	}
	defer rows.Close()

	hits, err := scanHits(rows, false)
	if err != nil {
		return err
	}
	for _, hit := range hits {
		fn(hit.Museum)
	}
	return nil
}
