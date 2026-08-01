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
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"museum/internal/models"
	"museum/internal/search"
	"museum/pkg/exhibitions"
)

//go:embed schema.sql
var schema string

// ErrNotFound reports a record that does not exist.
var ErrNotFound = errors.New("not found")

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
	if err := applySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	return &Store{pool: pool}, nil
}

// schemaLock is an arbitrary constant identifying the schema-application lock.
// Any process applying the schema takes the same one.
const schemaLock = 0x6D757365756D01 // "museum" and a version byte

// applySchema runs the schema with a lock held, so processes starting together
// do not race each other through it.
//
// They did. Two sweepers started at the same moment deadlocked on the ALTER
// TABLE and CREATE INDEX statements — Postgres detected it and failed one of
// them outright, so the process exited rather than starting. The schema is
// applied on every start by design, and this stack starts several processes at
// once, so the race was always there; it only became easy to hit once there
// was a service someone would sensibly run more than one of.
//
// An advisory lock rather than a migration table: the schema is idempotent and
// the only thing needed is that two of them are not interleaved.
func applySchema(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	defer tx.Rollback(ctx)

	// Held until the transaction ends, which is what serialises the appliers.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(schemaLock)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if _, err := tx.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// SaveMuseums upserts a batch, returning how many rows were written.
//
// Upserting on identity means a re-crawl updates records in place instead of
// accumulating copies. A museum that gains a Wikidata id needs one step first —
// see promoteWikidataIDs — because its identity changes when it does.
func (s *Store) SaveMuseums(ctx context.Context, museums []models.Museum) (int64, error) {
	if err := s.promoteWikidataIDs(ctx, museums); err != nil {
		return 0, err
	}

	const stmt = `
INSERT INTO museums (
    wikidata_id, name, normalized, search_text, locality_normalized, country, locality, description,
    website, wikipedia_url, page_id, source_page, aliases, aliases_normalized, sources, verified,
    sitelinks, street, postcode, location, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19,
    CASE WHEN $20::double precision IS NULL THEN NULL
         ELSE ST_SetSRID(ST_MakePoint($21::double precision, $20::double precision), 4326)::geography END,
    now()
)
ON CONFLICT (identity) DO UPDATE SET
    wikidata_id   = coalesce(nullif(EXCLUDED.wikidata_id, ''), museums.wikidata_id),
    name          = EXCLUDED.name,
    normalized    = EXCLUDED.normalized,
    -- Rebuilt from the union so an alias contributed by an earlier crawl stays
    -- searchable. Lowercasing is a weaker normaliser than the Go one, but it
    -- only ever adds terms — the incoming record's own names are already
    -- normalised properly in EXCLUDED.search_text.
    search_text   = EXCLUDED.search_text || ' ' ||
                    (SELECT coalesce(string_agg(DISTINCT lower(a), ' '), '')
                     FROM unnest(museums.aliases || EXCLUDED.aliases) a WHERE a <> ''),
    locality_normalized = EXCLUDED.locality_normalized,
    country       = coalesce(EXCLUDED.country, museums.country),
    locality      = coalesce(nullif(EXCLUDED.locality, ''), museums.locality),
    description   = coalesce(nullif(EXCLUDED.description, ''), museums.description),
    website       = coalesce(nullif(EXCLUDED.website, ''), museums.website),
    wikipedia_url = coalesce(nullif(EXCLUDED.wikipedia_url, ''), museums.wikipedia_url),
    page_id       = coalesce(nullif(EXCLUDED.page_id, 0), museums.page_id),
    source_page   = coalesce(nullif(EXCLUDED.source_page, ''), museums.source_page),
    -- Aliases and sources are unioned, never replaced. A source that knows
    -- fewer names for a museum must not erase the ones another source found:
    -- OpenStreetMap carries the local name ("Sjöfartsmuseet Akvariet") and
    -- Wikidata the English one and its acronyms, and a catalogue is only
    -- searchable in both languages if it keeps both. Replacing meant whichever
    -- crawl ran last silently won.
    aliases       = (SELECT coalesce(array_agg(DISTINCT a), '{}')
                     FROM unnest(museums.aliases || EXCLUDED.aliases) a WHERE a <> ''),
    aliases_normalized = (SELECT coalesce(array_agg(DISTINCT a), '{}')
                     FROM unnest(museums.aliases_normalized || EXCLUDED.aliases_normalized) a WHERE a <> ''),
    sources       = (SELECT coalesce(array_agg(DISTINCT s), '{}')
                     FROM unnest(museums.sources || EXCLUDED.sources) s WHERE s <> ''),
    verified      = museums.verified OR EXCLUDED.verified,
    sitelinks     = greatest(EXCLUDED.sitelinks, museums.sitelinks),
    street        = coalesce(nullif(EXCLUDED.street, ''), museums.street),
    postcode      = coalesce(nullif(EXCLUDED.postcode, ''), museums.postcode),
    -- A position already known is not replaced by a null one: enrichment can
    -- add coordinates, and a later crawl without them must not remove them.
    location      = coalesce(EXCLUDED.location, museums.location),
    updated_at    = now()`

	// The arguments are kept alongside the batch so a failed batch can be
	// replayed row by row.
	queries := make([][]any, 0, len(museums))
	batch := &pgx.Batch{}

	for _, m := range museums {
		name := validUTF8(strings.TrimSpace(m.Name))
		if name == "" {
			continue
		}

		var lat, lon *float64
		if m.HasCoordinates() {
			lat, lon = &m.Latitude, &m.Longitude
		}

		// Museum names come from Wikipedia wikitext, OpenStreetMap tags and
		// SPARQL results, none of which guarantee valid UTF-8. Postgres rejects
		// the byte sequence outright, and in a batch it rejects every row sent
		// alongside it.
		args := []any{
			validUTF8(m.WikidataID), name, search.Normalize(name), validUTF8(searchText(m)),
			search.Normalize(m.Locality),
			nullIfEmpty(validUTF8(m.Country)),
			validUTF8(m.Locality), validUTF8(m.Description), validUTF8(m.Website),
			validUTF8(m.WikipediaURL), m.PageID,
			validUTF8(m.SourcePage), validUTF8Each(textArray(m.AlsoKnownAs)),
			validUTF8Each(normalizedAliases(m.AlsoKnownAs)),
			validUTF8Each(textArray(m.Sources)), m.Verified,
			m.Sitelinks, validUTF8(m.Address.Street()), validUTF8(m.Address.Postcode), lat, lon,
		}

		queries = append(queries, args)
		batch.Queue(stmt, args...)
	}
	if batch.Len() == 0 {
		return 0, nil
	}

	written, err := s.execBatch(ctx, batch)
	if err == nil {
		return written, nil
	}

	// pgx runs a batch in an implicit transaction, so one rejected row rolls
	// back every row sent with it — 2,000 museums lost to one bad record. The
	// retry costs a round trip per row, but only on a batch that failed, and it
	// confines the damage to the row that caused it.
	log.Printf("postgres: museum batch failed (%v); retrying rows individually", err)

	written = 0
	var rejected int
	for _, queued := range queries {
		tag, rowErr := s.pool.Exec(ctx, stmt, queued...)
		if rowErr != nil {
			rejected++
			continue
		}
		written += tag.RowsAffected()
	}
	if rejected > 0 {
		log.Printf("postgres: %d of %d museums rejected", rejected, len(queries))
	}
	return written, nil
}

// promoteWikidataIDs gives an existing name-keyed row the Wikidata id an
// incoming record supplies for it.
//
// This exists because `identity` — the column the upsert conflicts on — is
// derived from wikidata_id, falling back to name and country. A museum first
// seen without a Wikidata id is stored under "name|country"; when a later crawl
// finds the same museum *with* one, its identity is the Q-id, which by
// construction cannot conflict with the stored row. The upsert therefore
// inserted a second copy and the merge arm of its DO UPDATE was unreachable.
// One Wikidata-only crawl produced 632 such pairs.
//
// Setting wikidata_id here recomputes the generated identity, so the upsert
// that follows recognises the row and updates it. Rows whose Wikidata id is
// already taken are left alone: those are true duplicates, and promoting them
// would collide on the unique index rather than merge. MergeDuplicates folds
// those together.
func (s *Store) promoteWikidataIDs(ctx context.Context, museums []models.Museum) error {
	normalized := make([]string, 0, len(museums))
	countries := make([]string, 0, len(museums))
	ids := make([]string, 0, len(museums))

	for _, m := range museums {
		id := strings.TrimSpace(m.WikidataID)
		name := strings.TrimSpace(m.Name)
		if id == "" || name == "" {
			continue
		}
		country := ""
		if c := nullIfEmpty(m.Country); c != nil {
			country = *c
		}
		normalized = append(normalized, search.Normalize(name))
		countries = append(countries, country)
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}

	// DISTINCT ON at both stages keeps this to one update per key: a batch can
	// carry two records with the same name and country, and a table can hold two
	// rows for them. Promoting more than one to the same Q-id would violate the
	// unique index — the very thing this is here to avoid.
	const stmt = `
WITH incoming AS (
    SELECT DISTINCT ON (normalized, country) normalized, country, wikidata_id
    FROM unnest($1::text[], $2::text[], $3::text[]) AS t(normalized, country, wikidata_id)
),
targets AS (
    SELECT DISTINCT ON (i.normalized, i.country) m.id, i.wikidata_id
    FROM museums m
    JOIN incoming i
      ON m.normalized = i.normalized
     AND coalesce(m.country, '') = i.country
    WHERE coalesce(m.wikidata_id, '') = ''
      AND NOT EXISTS (SELECT 1 FROM museums o WHERE o.wikidata_id = i.wikidata_id)
    ORDER BY i.normalized, i.country, m.id
)
UPDATE museums m SET wikidata_id = t.wikidata_id, updated_at = now()
FROM targets t WHERE m.id = t.id`

	if _, err := s.pool.Exec(ctx, stmt, normalized, countries, ids); err != nil {
		return fmt.Errorf("promote wikidata ids: %w", err)
	}
	return nil
}

// MergeDuplicates folds rows that describe the same museum into one, returning
// how many it removed.
//
// The pairs it finds are the ones promoteWikidataIDs cannot repair: the same
// name and country stored twice, once with a Wikidata id and once without,
// where the id is already claimed. The row carrying the Wikidata id is kept
// because it is the one every other source can be joined to; anything the
// discarded row knew that the keeper does not is copied over first, using the
// same field-by-field precedence as the upsert.
//
// One pass merges one duplicate per keeper, so it repeats until the catalogue
// is clean. Idempotent, and cheap once there is nothing left to do.
//
// Aliases gained by a merge are not folded into search_text, which only Go can
// build: the next crawl or reindex rewrites that column and picks them up.
func (s *Store) MergeDuplicates(ctx context.Context) (int64, error) {
	const stmt = `
WITH pairs AS (
    SELECT DISTINCT ON (keeper.id) keeper.id AS keep_id, dup.id AS drop_id
    FROM museums keeper
    JOIN museums dup
      ON dup.normalized = keeper.normalized
     AND coalesce(dup.country, '') = coalesce(keeper.country, '')
     AND dup.id <> keeper.id
    WHERE coalesce(keeper.wikidata_id, '') <> ''
      AND coalesce(dup.wikidata_id, '') = ''
    ORDER BY keeper.id, dup.id
),
merged AS (
    UPDATE museums k SET
        locality      = coalesce(nullif(k.locality, ''), d.locality),
        description   = coalesce(nullif(k.description, ''), d.description),
        website       = coalesce(nullif(k.website, ''), d.website),
        wikipedia_url = coalesce(nullif(k.wikipedia_url, ''), d.wikipedia_url),
        page_id       = coalesce(nullif(k.page_id, 0), d.page_id),
        source_page   = coalesce(nullif(k.source_page, ''), d.source_page),
        street        = coalesce(nullif(k.street, ''), d.street),
        postcode      = coalesce(nullif(k.postcode, ''), d.postcode),
        location      = coalesce(k.location, d.location),
        verified      = k.verified OR d.verified,
        sitelinks     = greatest(k.sitelinks, d.sitelinks),
        aliases       = (SELECT coalesce(array_agg(DISTINCT a), '{}') FROM unnest(k.aliases || d.aliases) a WHERE a <> ''),
        sources       = (SELECT coalesce(array_agg(DISTINCT s), '{}') FROM unnest(k.sources || d.sources) s WHERE s <> ''),
        updated_at    = now()
    FROM museums d JOIN pairs ON pairs.drop_id = d.id
    WHERE k.id = pairs.keep_id
)
DELETE FROM museums WHERE id IN (SELECT drop_id FROM pairs)`

	var removed int64
	for {
		tag, err := s.pool.Exec(ctx, stmt)
		if err != nil {
			return removed, fmt.Errorf("merge duplicates: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return removed, nil
		}
		removed += tag.RowsAffected()
	}
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

// normalizedAliases reduces each alternative name to its comparable form, for
// exact matching. Empty and duplicate results are dropped: an alias that
// normalises to the same string twice would only pad the index.
func normalizedAliases(aliases []string) []string {
	normalized := make([]string, 0, len(aliases))
	seen := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		reduced := search.Normalize(alias)
		if reduced == "" {
			continue
		}
		if _, dup := seen[reduced]; dup {
			continue
		}
		seen[reduced] = struct{}{}
		normalized = append(normalized, reduced)
	}
	return normalized
}

// validUTF8 replaces byte sequences Postgres would refuse.
//
// A text column will not accept invalid UTF-8, and there is no encoding to fall
// back on: the alternative to substituting the bad bytes is losing the record,
// and in a batch, losing every record alongside it. The replacement character
// is visible in the output, which is the point — it shows where the source was
// mis-encoded rather than hiding it.
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

// validUTF8Each cleans every element of an array bound for a text[] column. One
// bad alias rejects the whole row, and with it the whole batch.
func validUTF8Each(values []string) []string {
	for i, v := range values {
		values[i] = validUTF8(v)
	}
	return values
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
	// ApproximateLocation is true when the position is the museum's town
	// centre rather than the museum's own, because no geocoder could find it.
	ApproximateLocation bool

	// ID is the row's stable identifier. Without one there is nothing to deep
	// link to, nothing to page on, and nothing for a client to dedupe by:
	// wikidata_id is absent for about 4% of the catalogue, so it cannot serve.
	ID int64

	Museum models.Museum
	// DistanceKm is set by radius queries.
	DistanceKm float64
	// Score is set by search queries.
	Score float64
}

// Page is a slice of results together with the size of the whole set.
//
// Total is what makes truncation visible. Returning exactly `limit` rows with
// no total is indistinguishable from a result set that happens to be that
// size — London holds 614 museums within 50 km and the API returned 500 of
// them with nothing to say so, leaving 114 unreachable by any request.
type Page struct {
	Hits  []Hit
	Total int64
}

// Nearby returns the museums within radiusKm of a point, nearest first.
func (s *Store) Nearby(ctx context.Context, lat, lon, radiusKm float64, limit, offset int) (Page, error) {
	return s.NearbyVerified(ctx, lat, lon, radiusKm, limit, offset, false)
}

// NearbyVerified is Nearby, optionally restricted to museums backed by a
// Wikipedia article — which is what verified means across every source.
//
// It trades recall for precision rather than filtering error from truth. The
// unverified set holds both the noise (names read off list pages: "Williamsburg,
// Virginia", "Silverton (hotel and casino)") and a great many real museums too
// small to have an article. Gothenburg has 75 museums and 20 verified ones.
func (s *Store) NearbyVerified(ctx context.Context, lat, lon, radiusKm float64, limit, offset int, verifiedOnly bool) (Page, error) {
	// count(*) OVER () reports the size of the whole matching set from the same
	// scan that produces the page, so a total costs no second query.
	const stmt = `
SELECT id, name, coalesce(country,''), coalesce(locality,''), coalesce(description,''),
       coalesce(website,''), coalesce(wikipedia_url,''), coalesce(wikidata_id,''),
       aliases, sources, verified, street, postcode, location_approximate,
       ST_Y(location::geometry), ST_X(location::geometry),
       count(*) OVER () AS total,
       ST_Distance(location, $1::geography) / 1000.0 AS distance_km
FROM museums
WHERE location IS NOT NULL
  AND ST_DWithin(location, $1::geography, $2)
  AND (NOT $5::boolean OR verified)
ORDER BY location <-> $1::geography, id
LIMIT $3 OFFSET $4`

	point := fmt.Sprintf("SRID=4326;POINT(%v %v)", lon, lat)

	rows, err := s.pool.Query(ctx, stmt, point, radiusKm*1000, limit, offset, verifiedOnly)
	if err != nil {
		return Page{}, fmt.Errorf("nearby: %w", err)
	}
	defer rows.Close()

	return scanPage(rows, true)
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
func (s *Store) Search(ctx context.Context, query string, limit, offset int) (Page, error) {
	normalized := search.Normalize(query)
	if normalized == "" {
		return Page{}, nil
	}

	const stmt = `
WITH q AS (SELECT $1::text AS term)
SELECT id, name, coalesce(country,''), coalesce(locality,''), coalesce(description,''),
       coalesce(website,''), coalesce(wikipedia_url,''), coalesce(wikidata_id,''),
       aliases, sources, verified, street, postcode, location_approximate,
       ST_Y(location::geometry), ST_X(location::geometry),
       count(*) OVER () AS total,
       (
         CASE WHEN normalized = q.term THEN 3.0
              -- An exact alias match is nearly as strong as an exact name
              -- match, and stronger than a prefix on the name: someone typing
              -- "moma" means the museum that calls itself MoMA, not the first
              -- museum whose name happens to begin with those letters.
              WHEN aliases_normalized @> ARRAY[q.term] THEN 2.5
              WHEN normalized LIKE q.term || '%' THEN 1.5
              WHEN position(q.term in normalized) > 0 THEN 0.75
              ELSE 0 END
         -- Similarity is measured against the name, not against search_text.
         -- Scoring the concatenation of name, aliases and town matched almost
         -- anything: "van gough museum" reached "Vantaa City Museum".
         --
         -- The two measures are blended rather than maxed. word_similarity
         -- scores the best matching *extent* of the name, so it rewards a
         -- generic word the query happens to share: for "kunstmuseum zurich"
         -- it found "museum zurich" inside "National Museum Zurich" and scored
         -- it 0.632, beating "Kunsthaus Zürich" at 0.500 — even though whole-
         -- name similarity ranks Kunsthaus higher (0.500 against 0.400).
         -- Taking the greater of the two let the lenient measure overrule the
         -- strict one. Blending keeps word_similarity's ability to find a short
         -- query inside a long name without letting it dominate. The weighting
         -- is not sensitive: anything from 0.6/0.4 to 0.8/0.2 scores the same
         -- on the evaluation set.
         + 0.7 * similarity(normalized, q.term)
         + 0.3 * word_similarity(q.term, normalized)
         -- Does the query mention this museum's town? It is what separates the
         -- Kunsthaus in Zürich from a Kunstmuseum on Fanø.
         --
         -- Matched on word boundaries, because a plain substring test fires on
         -- letters inside unrelated words: 338 towns are three characters long
         -- and "Sé" promoted a museum in Funchal for every query containing
         -- "mu-se-um". Short names are skipped for the same reason. The regex
         -- is built from data, which is safe only because Normalize has already
         -- reduced it to letters, digits and spaces.
         --
         -- The weight is deliberately below the similarity range. At 1.0 it
         -- outweighed the name entirely, so "metropolitan museum new york"
         -- returned the New York State Museum: a weaker name match that
         -- happened to sit in the town the query named.
         + CASE WHEN length(locality_normalized) >= 4
                 AND q.term ~ ('\m' || locality_normalized || '\M')
                THEN 0.4 ELSE 0 END
         -- Prominence, on a log scale so it settles ties without overruling a
         -- better name match: the Louvre's 167 language editions score about
         -- 1.0, a museum with a single article about 0.14.
         + 0.2 * ln(1 + greatest(sitelinks, 0))
         + CASE WHEN location IS NOT NULL THEN 0.01 ELSE 0 END
       ) AS score
FROM museums, q
WHERE normalized % q.term
   OR q.term <% normalized
   OR normalized LIKE q.term || '%'
   OR search_text % q.term
   OR aliases_normalized @> ARRAY[q.term]
ORDER BY score DESC, length(normalized), name, id
LIMIT $2 OFFSET $3`

	rows, err := s.pool.Query(ctx, stmt, normalized, limit, offset)
	if err != nil {
		return Page{}, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	return scanPage(rows, false)
}

// MuseumByID returns one museum, so a result can be linked to and fetched
// again. It accepts the numeric id or a Wikidata "Q…" identifier.
func (s *Store) MuseumByID(ctx context.Context, id string) (Hit, error) {
	const stmt = `
SELECT id, name, coalesce(country,''), coalesce(locality,''), coalesce(description,''),
       coalesce(website,''), coalesce(wikipedia_url,''), coalesce(wikidata_id,''),
       aliases, sources, verified, street, postcode, location_approximate,
       ST_Y(location::geometry), ST_X(location::geometry),
       1::bigint AS total, 0::double precision
FROM museums
WHERE ($1::bigint IS NOT NULL AND id = $1::bigint) OR wikidata_id = $2
LIMIT 1`

	var numeric *int64
	if parsed, err := strconv.ParseInt(id, 10, 64); err == nil {
		numeric = &parsed
	}

	rows, err := s.pool.Query(ctx, stmt, numeric, id)
	if err != nil {
		return Hit{}, fmt.Errorf("museum %q: %w", id, err)
	}
	defer rows.Close()

	page, err := scanPage(rows, true)
	if err != nil {
		return Hit{}, err
	}
	if len(page.Hits) == 0 {
		return Hit{}, fmt.Errorf("museum %q: %w", id, ErrNotFound)
	}
	return page.Hits[0], nil
}

// scanHits reads a result set into museums. withDistance says whether the final
// column is a distance or a score.
func scanPage(rows pgx.Rows, withDistance bool) (Page, error) {
	var page Page

	for rows.Next() {
		var (
			hit      Hit
			lat, lon *float64
			total    int64
			last     float64
		)
		var street, postcode string
		if err := rows.Scan(
			&hit.ID,
			&hit.Museum.Name, &hit.Museum.Country, &hit.Museum.Locality,
			&hit.Museum.Description, &hit.Museum.Website, &hit.Museum.WikipediaURL,
			&hit.Museum.WikidataID, &hit.Museum.AlsoKnownAs, &hit.Museum.Sources,
			&hit.Museum.Verified, &street, &postcode, &hit.ApproximateLocation,
			&lat, &lon, &total, &last,
		); err != nil {
			return Page{}, fmt.Errorf("scan: %w", err)
		}

		hit.Museum.Address.Road, hit.Museum.Address.Postcode = street, postcode
		if lat != nil && lon != nil {
			hit.Museum.Latitude, hit.Museum.Longitude = *lat, *lon
		}
		if withDistance {
			hit.DistanceKm = last
		} else {
			hit.Score = last
		}
		page.Hits = append(page.Hits, hit)
		page.Total = total
	}
	return page, rows.Err()
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

// execBatch runs a prepared batch and reports how many rows it wrote.
func (s *Store) execBatch(ctx context.Context, batch *pgx.Batch) (int64, error) {
	results := s.pool.SendBatch(ctx, batch)
	defer results.Close()

	var written int64
	for range batch.Len() {
		tag, err := results.Exec()
		if err != nil {
			return 0, err
		}
		written += tag.RowsAffected()
	}
	return written, nil
}

// SaveExhibitions upserts scraped listings, keyed by URL.
func (s *Store) SaveExhibitions(ctx context.Context, found []exhibitions.Exhibition) (int64, error) {
	const stmt = `
INSERT INTO exhibitions (url, title, museum, museum_wikidata_id, starts_on, ends_on, location, source_page, scraped_at, permanent, site, first_seen_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6,
        CASE WHEN $7::double precision IS NULL THEN NULL
             ELSE ST_SetSRID(ST_MakePoint($8::double precision, $7::double precision), 4326)::geography END,
        $9, $10, $11, $12, $10, $10)
ON CONFLICT (url) DO UPDATE SET
    title      = EXCLUDED.title,
    museum     = EXCLUDED.museum,
    starts_on  = EXCLUDED.starts_on,
    ends_on    = EXCLUDED.ends_on,
    permanent  = EXCLUDED.permanent,
    site       = EXCLUDED.site,
    location   = coalesce(EXCLUDED.location, exhibitions.location),
    scraped_at = EXCLUDED.scraped_at,
    -- first_seen_at is never moved forward: it is what "new since Tuesday"
    -- means, and rewriting it on every sweep would make everything new every
    -- time.
    first_seen_at = least(exhibitions.first_seen_at, EXCLUDED.first_seen_at),
    last_seen_at  = greatest(exhibitions.last_seen_at, EXCLUDED.last_seen_at),
    -- Seeing it again undoes a retirement: a listing that came back was either
    -- never gone or has returned, and either way it is on show now.
    retired_at = NULL`

	// The arguments are kept alongside the batch so a failed batch can be
	// replayed row by row.
	queries := make([][]any, 0, len(found))
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
		// Text scraped from a museum's own website is not guaranteed to be
		// valid UTF-8: plenty of sites still serve Latin-1 without declaring
		// it, and Postgres rejects the byte sequence outright. Cleaning here is
		// the cheap half; decoding the declared charset properly, in the
		// scraper, is the other and preserves the characters rather than
		// replacing them.
		// The site is derived by the scraper's own rule rather than by parsing
		// the URL here, so the key a listing is stored under and the key its
		// sweep is scheduled under cannot disagree.
		site := exhibitions.SiteKey(e.SourcePage)
		if site == "" {
			site = exhibitions.SiteKey(e.URL)
		}

		args := []any{validUTF8(e.URL), validUTF8(e.Title), validUTF8(e.Museum),
			validUTF8(e.MuseumWikidataID),
			e.Start, e.End, lat, lon, validUTF8(e.SourcePage), scraped, e.Permanent, site}

		queries = append(queries, args)
		batch.Queue(stmt, args...)
	}
	if batch.Len() == 0 {
		return 0, nil
	}

	written, err := s.execBatch(ctx, batch)
	if err == nil {
		return written, nil
	}

	// pgx runs a batch in an implicit transaction, so one rejected row rolls
	// back every row sent with it. That turned a single mis-encoded title into
	// the loss of 9,148 exhibitions. Retrying the rows one at a time costs a
	// round trip each, but only on the rare batch that actually failed, and it
	// confines the damage to the row that caused it.
	log.Printf("postgres: exhibition batch failed (%v); retrying rows individually", err)

	written = 0
	var rejected int
	for _, queued := range queries {
		tag, rowErr := s.pool.Exec(ctx, stmt, queued...)
		if rowErr != nil {
			rejected++
			continue
		}
		written += tag.RowsAffected()
	}
	if rejected > 0 {
		log.Printf("postgres: %d of %d exhibitions rejected", rejected, len(queries))
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
       starts_on, ends_on, coalesce(source_page,''), scraped_at, permanent,
       ST_Y(location::geometry), ST_X(location::geometry),
       ST_Distance(location, $1::geography) / 1000.0 AS distance_km
FROM exhibitions
WHERE location IS NOT NULL
  AND retired_at IS NULL
  AND ST_DWithin(location, $1::geography, $2)
  AND (ends_on IS NULL OR ends_on >= current_date)
  AND ($3 OR starts_on IS NULL OR starts_on <= current_date)
-- Soonest to close leads, so a permanent display — which has no closing date
-- and will still be there next year — sorts behind everything a visitor could
-- miss, and gives way first when the limit is reached.
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
			&hit.Start, &hit.End, &hit.SourcePage, &hit.ScrapedAt, &hit.Permanent,
			&lat, &lon, &hit.DistanceKm); err != nil {
			return nil, fmt.Errorf("scan exhibition: %w", err)
		}
		if lat != nil && lon != nil {
			hit.Latitude, hit.Longitude = *lat, *lon
		}
		// A permanent display is on today whatever its dates say, and cannot be
		// upcoming: there is nothing for it to be waiting for.
		hit.Running = hit.Permanent || hit.Start == nil || !hit.Start.After(now)
		hit.Upcoming = !hit.Permanent && hit.Start != nil && hit.Start.After(now)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// EachMuseum streams the whole catalogue, for the audit.
//
// Genuinely streams: it calls fn as each row arrives. It used to gather every
// row into a slice first, so "streaming" 85,000 museums allocated 250 MB and
// the first callback fired only once the last row had been read.
func (s *Store) EachMuseum(ctx context.Context, fn func(models.Museum)) error {
	const stmt = `
SELECT name, coalesce(country,''), coalesce(locality,''), coalesce(description,''),
       coalesce(website,''), coalesce(wikipedia_url,''), coalesce(wikidata_id,''),
       aliases, sources, verified, street, postcode,
       ST_Y(location::geometry), ST_X(location::geometry)
FROM museums`

	rows, err := s.pool.Query(ctx, stmt)
	if err != nil {
		return fmt.Errorf("scan catalogue: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			museum           models.Museum
			lat, lon         *float64
			street, postcode string
		)
		if err := rows.Scan(
			&museum.Name, &museum.Country, &museum.Locality, &museum.Description,
			&museum.Website, &museum.WikipediaURL, &museum.WikidataID,
			&museum.AlsoKnownAs, &museum.Sources, &museum.Verified,
			&street, &postcode, &lat, &lon,
		); err != nil {
			return fmt.Errorf("scan catalogue: %w", err)
		}
		museum.Address.Road, museum.Address.Postcode = street, postcode
		if lat != nil && lon != nil {
			museum.Latitude, museum.Longitude = *lat, *lon
		}
		fn(museum)
	}
	return rows.Err()
}

// Coverage describes how well an area has been scraped for exhibitions.
//
// It exists because an empty result was ambiguous in the worst way: a request
// for exhibitions in London returned count 0, which reads as "nothing is on"
// when it actually meant "no museum here has ever been scraped". The catalogue
// knew 431 museums in that circle and had listings for none of them, and
// nothing in the response said so.
type Coverage struct {
	// MuseumsInArea is how many museums the catalogue knows there.
	MuseumsInArea int64
	// MuseumsWithSite is how many have a website that could be scraped.
	MuseumsWithSite int64
	// LastScraped is when the area was last refreshed, nil if it never was.
	LastScraped *time.Time
}

// ExhibitionCoverage reports what is known about an area.
func (s *Store) ExhibitionCoverage(ctx context.Context, lat, lon, radiusKm float64) (Coverage, error) {
	const stmt = `
SELECT count(*),
       count(*) FILTER (WHERE website IS NOT NULL AND website <> ''),
       -- Read from the attempt record, not from the results.
       --
       -- Taking this from max(scraped_at) over the exhibitions meant an area
       -- whose every museum had been read that morning and had published
       -- nothing reported "nobody has looked here yet" — the one answer
       -- guaranteed to be wrong, and the one that sends a caller off to run a
       -- refresh that will find nothing again.
       (SELECT max(ss.last_attempt_at)
          FROM site_scrapes ss
         WHERE ss.site IN (
             SELECT lower(regexp_replace(substring(m2.website from '^https?://([^/:]+)'), '^www\.', ''))
               FROM museums m2
              WHERE m2.location IS NOT NULL
                AND coalesce(m2.website,'') <> ''
                AND ST_DWithin(m2.location, $1::geography, $2)))
FROM museums
WHERE location IS NOT NULL AND ST_DWithin(location, $1::geography, $2)`

	point := fmt.Sprintf("SRID=4326;POINT(%v %v)", lon, lat)

	var coverage Coverage
	if err := s.pool.QueryRow(ctx, stmt, point, radiusKm*1000).Scan(
		&coverage.MuseumsInArea, &coverage.MuseumsWithSite, &coverage.LastScraped,
	); err != nil {
		return Coverage{}, fmt.Errorf("exhibition coverage: %w", err)
	}
	return coverage, nil
}

// Unplaced is a museum the catalogue holds but cannot locate.
type Unplaced struct {
	ID       int64
	Name     string
	Locality string
	Country  string
}

// UnplacedMuseums returns museums with no coordinates, optionally narrowed to a
// town or country.
//
// A fifth of the catalogue is in this state. Those records are findable by name
// and invisible to every radius and place query, which is the one thing a
// visitor-facing caller does most — a museum nobody can find on a map may as
// well not be in the catalogue.
//
// Only records with something to geocode from are returned: a bare name with no
// town and no country cannot be resolved to one place with any confidence.
func (s *Store) UnplacedMuseums(ctx context.Context, locality, country string, limit int) ([]Unplaced, error) {
	const stmt = `
SELECT id, name, coalesce(locality, ''), coalesce(country, '')
FROM museums
WHERE location IS NULL
  AND (coalesce(locality, '') <> '' OR coalesce(country, '') <> '')
  AND ($1 = '' OR locality ILIKE '%' || $1 || '%')
  AND ($2 = '' OR country ILIKE $2)
ORDER BY sitelinks DESC, id
LIMIT $3`

	rows, err := s.pool.Query(ctx, stmt, locality, country, limit)
	if err != nil {
		return nil, fmt.Errorf("unplaced museums: %w", err)
	}
	defer rows.Close()

	var found []Unplaced
	for rows.Next() {
		var u Unplaced
		if err := rows.Scan(&u.ID, &u.Name, &u.Locality, &u.Country); err != nil {
			return nil, fmt.Errorf("scan unplaced: %w", err)
		}
		found = append(found, u)
	}
	return found, rows.Err()
}

// SetLocation records coordinates for one museum. approximate marks a position
// taken from the museum's town rather than from the museum itself.
func (s *Store) SetLocation(ctx context.Context, id int64, lat, lon float64, approximate bool) error {
	const stmt = `
UPDATE museums
SET location = ST_SetSRID(ST_MakePoint($3::double precision, $2::double precision), 4326)::geography,
    location_approximate = $4,
    updated_at = now()
WHERE id = $1`

	if _, err := s.pool.Exec(ctx, stmt, id, lat, lon, approximate); err != nil {
		return fmt.Errorf("set location for %d: %w", id, err)
	}
	return nil
}

// MuseumsWithWebsites returns museums that have a site worth scraping for
// exhibitions, most prominent first.
//
// Ordering by sitelinks is what makes a capped run useful: the scraper reads a
// few thousand sites, and the museums most likely to publish a structured
// "what's on" page are the ones the world writes most about. A cap without an
// ordering would scrape an arbitrary few thousand of 71,728.
func (s *Store) MuseumsWithWebsites(ctx context.Context, limit int) ([]models.Museum, error) {
	const stmt = `
SELECT name, coalesce(country,''), coalesce(locality,''), coalesce(website,''),
       coalesce(wikidata_id,''), ST_Y(location::geometry), ST_X(location::geometry)
FROM museums
WHERE website IS NOT NULL AND website <> ''
  AND location IS NOT NULL
ORDER BY sitelinks DESC, id
LIMIT $1`

	rows, err := s.pool.Query(ctx, stmt, limit)
	if err != nil {
		return nil, fmt.Errorf("museums with websites: %w", err)
	}
	defer rows.Close()

	var museums []models.Museum
	for rows.Next() {
		var (
			m        models.Museum
			lat, lon *float64
		)
		if err := rows.Scan(&m.Name, &m.Country, &m.Locality, &m.Website,
			&m.WikidataID, &lat, &lon); err != nil {
			return nil, fmt.Errorf("scan museum with website: %w", err)
		}
		if lat != nil && lon != nil {
			m.Latitude, m.Longitude = *lat, *lon
		}
		museums = append(museums, m)
	}
	return museums, rows.Err()
}

// maxTownSpreadKm is how far a town's museums may lie from their own centre
// before that centre is refused as a position for the town's other museums.
//
// The number that matters is not the ideal town radius but the point beyond
// which a centroid stops describing anywhere. A group whose members are 50 km
// apart still puts a museum in roughly the right city; a group whose members
// are 19,000 km apart puts it in the ocean.
const maxTownSpreadKm = 50

// PlaceAtTownCentres gives every unplaced museum the position of the town it is
// recorded in. It reports how many it placed and how many earlier approximate
// positions it discarded.
//
// One statement per pass rather than one per museum. Doing this a row at a time
// meant 30,400 round trips, each re-running an ST_Collect aggregate over the
// town's museums, and the database crashed partway through and recovered from
// its log. Computing the centroids once and joining against them does the same
// work in seconds without asking the server for it thirty thousand times.
//
// Positions set here are marked approximate, because they are: the museum is
// really in that town, but it is not really at that point.
//
// Three things decide whether the centre is anywhere at all.
//
// Towns are grouped within a country. Without that, "Kingston" spanned five
// countries and "Cambridge" four, and their centroids were points in the
// Atlantic; 3,677 museums were placed that way, including Korean ones off the
// coast of Spain.
//
// Towns are matched by their recorded name first, and only then by one name
// being a whole-word extension of the other. That second pass exists to merge
// the administrative forms the sources carry — "Gothenburg" against "Gothenburg
// Municipality" — and the prefix is what limits it to that. Matching on the
// leading word alone instead merges "Port Hope" with "Port Colborne", which are
// different towns 300 km apart, and "South Bend" with South Korea.
//
// A group is refused when its own members are spread wider than a city. This is
// what catches what the first two miss — one country still holds three
// Springfields, so no centre can stand for "Springfield" and each is placed
// only from its own fully-qualified name.
//
// Centres are computed only from surveyed positions, never from approximate
// ones, so a run cannot feed on the output of the run before it.
func (s *Store) PlaceAtTownCentres(ctx context.Context) (placed, discarded int64, err error) {
	// Earlier approximate positions are cleared first. They are derived, not
	// sourced — this call recomputes every one it can — and leaving them is
	// precisely the bug: a museum placed in the sea by a bad grouping stays
	// there forever, because it is no longer unplaced.
	const clear = `
UPDATE museums
SET location = NULL, location_approximate = false, updated_at = now()
WHERE location_approximate`

	// Tier 1 keys on the town as recorded; tier 2 keys on its leading word to
	// find candidates cheaply and then keeps only those that are a whole-word
	// extension of the museum's own town. Both are scoped to a country and both
	// must be tight. The tiers run in order, so the looser one is consulted only
	// for a museum the exact name could not place.
	const centres = `
WITH surveyed AS (
    SELECT lower(country) AS country, locality_normalized AS town, location
    FROM museums
    WHERE location IS NOT NULL
      AND NOT location_approximate
      AND locality_normalized <> ''
      AND coalesce(country, '') <> ''
),
keyed AS (
    SELECT country, town AS key, town, location FROM surveyed WHERE $1 = 1
    UNION ALL
    SELECT country, split_part(town, ' ', 1), town, location FROM surveyed
    WHERE $1 = 2 AND length(split_part(town, ' ', 1)) >= 4
),
centre AS (
    SELECT country, key, ST_Centroid(ST_Collect(location::geometry))::geography AS point
    FROM keyed
    GROUP BY 1, 2
),
towns AS (
    SELECT c.country, c.key, c.point, array_agg(DISTINCT k.town) AS names
    FROM centre c
    JOIN keyed k ON k.country = c.country AND k.key = c.key
    GROUP BY 1, 2, 3
    HAVING max(ST_Distance(k.location, c.point)) <= $2::double precision * 1000
)
UPDATE museums m
SET location = t.point,
    location_approximate = true,
    updated_at = now()
FROM towns t
WHERE m.location IS NULL
  AND m.locality_normalized <> ''
  AND lower(m.country) = t.country
  AND t.key = CASE WHEN $1 = 1 THEN m.locality_normalized
                   ELSE split_part(m.locality_normalized, ' ', 1) END
  AND ($1 = 1 OR EXISTS (
      SELECT 1 FROM unnest(t.names) AS n
      -- One name extends the other, on a word boundary: "Gothenburg" and
      -- "Gothenburg Municipality" are the same town, "Port Hope" and "Port
      -- Colborne" are not. starts_with rather than LIKE, so a locality
      -- containing % or _ is compared literally.
      WHERE starts_with(n, m.locality_normalized || ' ')
         OR starts_with(m.locality_normalized, n || ' ')))`

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("place at town centres: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, clear)
	if err != nil {
		return 0, 0, fmt.Errorf("clear approximate positions: %w", err)
	}
	discarded = tag.RowsAffected()

	for tier := 1; tier <= 2; tier++ {
		tag, err := tx.Exec(ctx, centres, tier, maxTownSpreadKm)
		if err != nil {
			return 0, 0, fmt.Errorf("place at town centres (pass %d): %w", tier, err)
		}
		placed += tag.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("place at town centres: %w", err)
	}
	return placed, discarded, nil
}

// Point is a museum reduced to what a map needs to draw it.
type Point struct {
	ID  int64
	Lat float64
	Lon float64
}

// Points returns museum positions for drawing, most prominent first.
//
// The ordinary radius query caps at 500 hits and carries a dozen fields per
// museum, which is right for a list and wrong for a map: a world view wants
// tens of thousands of positions and nothing else. Ordering by prominence means
// a truncated view shows the museums worth seeing at that scale rather than an
// arbitrary subset, and zooming in narrows the box until everything local fits.
//
// An empty box means the whole world.
func (s *Store) Points(ctx context.Context, west, south, east, north float64, hasBox bool, limit int) ([]Point, error) {
	const stmt = `
SELECT id, ST_Y(location::geometry), ST_X(location::geometry)
FROM museums
WHERE location IS NOT NULL
  AND (NOT $1::boolean
       OR ST_Intersects(location::geometry, ST_MakeEnvelope($2, $3, $4, $5, 4326)))
ORDER BY sitelinks DESC, id
LIMIT $6`

	rows, err := s.pool.Query(ctx, stmt, hasBox, west, south, east, north, limit)
	if err != nil {
		return nil, fmt.Errorf("points: %w", err)
	}
	defer rows.Close()

	points := make([]Point, 0, min(limit, 4096))
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.ID, &p.Lat, &p.Lon); err != nil {
			return nil, fmt.Errorf("scan point: %w", err)
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// MergeDuplicateExhibitions folds repeated listings of one event into a single
// row spanning all of its dates, and reports how many rows it removed.
//
// Museums publish recurring events as one entry per occurrence, each with its
// own URL — Hasselblad Center listed one exhibition's guided tours eight times,
// Kalmar konstmuseum listed "Konstparken" four times. The scraper now merges
// them as it reads, but rows already stored were keyed on URL and need folding
// together after the fact.
//
// The surviving row keeps the earliest start and the latest end, so the entry
// covers the whole run rather than whichever occurrence happened to be kept.
func (s *Store) MergeDuplicateExhibitions(ctx context.Context) (int64, error) {
	const stmt = `
WITH grouped AS (
    SELECT museum,
           lower(btrim(title)) AS title_key,
           min(starts_on) AS first_start,
           max(ends_on)   AS last_end,
           -- The earliest occurrence is kept, so its URL is the one a visitor
           -- following the link arrives at first.
           (array_agg(url ORDER BY coalesce(starts_on, DATE '0001-01-01'), url))[1] AS keep_url
    FROM exhibitions
    WHERE museum IS NOT NULL AND museum <> ''
    GROUP BY museum, lower(btrim(title))
    HAVING count(*) > 1
),
widened AS (
    UPDATE exhibitions e
    SET starts_on = g.first_start,
        ends_on   = g.last_end
    FROM grouped g
    WHERE e.url = g.keep_url
    RETURNING e.url
)
DELETE FROM exhibitions e
USING grouped g
WHERE e.museum = g.museum
  AND lower(btrim(e.title)) = g.title_key
  AND e.url <> g.keep_url`

	tag, err := s.pool.Exec(ctx, stmt)
	if err != nil {
		return 0, fmt.Errorf("merge duplicate exhibitions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PruneNavigationListings removes stored rows that are a listing's own paging
// controls rather than anything on show, and reports how many it removed.
//
// The scraper rejects these as it reads now, but rows gathered before it could
// are still in the table: "Föregående Evenemang", "Nästa Evenemang", "Evenemang
// in Lista View" — a calendar plugin's previous, next and view-switch buttons,
// stored as though they were exhibitions.
//
// The test is the scraper's own, applied to the URL against the page it was
// found on, so the two cannot drift apart on what counts as navigation.
//
// Permanent displays are exempt. A museum with no programme is recorded from
// the page that describes it, so its URL and its source page are the same page
// — which is exactly the shape the navigation test rejects, and here means the
// opposite of a paging control.
func (s *Store) PruneNavigationListings(ctx context.Context) (int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT url, source_page FROM exhibitions
          WHERE source_page IS NOT NULL AND source_page <> '' AND NOT permanent`)
	if err != nil {
		return 0, fmt.Errorf("prune navigation: %w", err)
	}

	var doomed []string
	for rows.Next() {
		var link, source string
		if err := rows.Scan(&link, &source); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan listing: %w", err)
		}
		base, err := url.Parse(source)
		if err != nil {
			continue
		}
		if exhibitions.IsNavigationLink(link, base) {
			doomed = append(doomed, link)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("prune navigation: %w", err)
	}
	if len(doomed) == 0 {
		return 0, nil
	}

	tag, err := s.pool.Exec(ctx, `DELETE FROM exhibitions WHERE url = ANY($1)`, doomed)
	if err != nil {
		return 0, fmt.Errorf("prune navigation: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MuseumsWithWebsitesNear returns the museums around a point whose sites are
// worth reading for exhibitions, most prominent first.
func (s *Store) MuseumsWithWebsitesNear(ctx context.Context, lat, lon, radiusKm float64, limit int) ([]models.Museum, error) {
	const stmt = `
SELECT name, coalesce(country,''), coalesce(locality,''), coalesce(website,''),
       coalesce(wikidata_id,''), ST_Y(location::geometry), ST_X(location::geometry)
FROM museums
WHERE website IS NOT NULL AND website <> ''
  AND location IS NOT NULL
  AND ST_DWithin(location, $1::geography, $2)
ORDER BY sitelinks DESC, id
LIMIT $3`

	point := fmt.Sprintf("SRID=4326;POINT(%v %v)", lon, lat)

	rows, err := s.pool.Query(ctx, stmt, point, radiusKm*1000, limit)
	if err != nil {
		return nil, fmt.Errorf("museums with websites near: %w", err)
	}
	defer rows.Close()

	var museums []models.Museum
	for rows.Next() {
		var (
			m        models.Museum
			lat, lon *float64
		)
		if err := rows.Scan(&m.Name, &m.Country, &m.Locality, &m.Website,
			&m.WikidataID, &lat, &lon); err != nil {
			return nil, fmt.Errorf("scan museum: %w", err)
		}
		if lat != nil && lon != nil {
			m.Latitude, m.Longitude = *lat, *lon
		}
		museums = append(museums, m)
	}
	return museums, rows.Err()
}

// MergeNameVariants folds together museums that are the same place recorded
// under different names, and reports how many rows it removed.
//
// "Gothenburg Museum" and "Museum of Gothenburg" are one museum two hundred
// metres apart in the catalogue, because each source named it its own way and
// the upsert keys on the exact name. Trigram similarity is the wrong tool here:
// it would merge "Tate Modern" with "Tate Britain", which are two museums.
//
// The rule is that two records are the same museum when they sit almost on top
// of each other AND their names use the same significant words. Word order and
// filler differ between sources — "of", "the", "der" — and word length is a
// cheap, language-independent way to drop those. Tate Modern and Tate Britain
// survive it: their word sets differ, so no distance would merge them.
//
// The radius is deliberately small. Two genuinely different museums can share a
// building, and 150 m is close enough that a false merge needs both the same
// words and the same doorway.
func (s *Store) MergeNameVariants(ctx context.Context) (int64, error) {
	const stmt = `
WITH tokens AS (
    SELECT id, location, sitelinks, normalized,
           (SELECT array_agg(word ORDER BY word)
              FROM unnest(string_to_array(normalized, ' ')) AS word
             WHERE length(word) > 2) AS words
    FROM museums
    WHERE location IS NOT NULL AND normalized <> ''
),
candidates AS (
    -- At least two significant words: a single shared word is a coincidence,
    -- not an identity. "Museum" alone would merge a town's every museum.
    SELECT * FROM tokens WHERE words IS NOT NULL AND cardinality(words) >= 2
),
pairs AS (
    SELECT a.id AS keeper, b.id AS victim
    FROM candidates a
    JOIN candidates b
      ON a.words = b.words
     AND a.id <> b.id
     AND ST_DWithin(a.location, b.location, 150)
    -- The better-documented record survives, so the merge keeps the row more
    -- of the catalogue already points at.
    WHERE (a.sitelinks, -a.id) > (b.sitelinks, -b.id)
),
-- One keeper per victim: a cluster of three collapses onto a single row rather
-- than each pair merging separately and leaving fragments.
resolved AS (
    SELECT victim, min(keeper) AS keeper FROM pairs GROUP BY victim
),
final AS (
    SELECT r.victim, r.keeper FROM resolved r
    WHERE NOT EXISTS (SELECT 1 FROM resolved o WHERE o.victim = r.keeper)
),
merged AS (
    UPDATE museums m SET
        aliases = (SELECT coalesce(array_agg(DISTINCT a), '{}')
                     FROM unnest(m.aliases || v.aliases || ARRAY[v.name]) a WHERE a <> ''),
        aliases_normalized = (SELECT coalesce(array_agg(DISTINCT a), '{}')
                     FROM unnest(m.aliases_normalized || v.aliases_normalized || ARRAY[v.normalized]) a WHERE a <> ''),
        sources = (SELECT coalesce(array_agg(DISTINCT s), '{}')
                     FROM unnest(m.sources || v.sources) s WHERE s <> ''),
        search_text = m.search_text || ' ' || v.normalized,
        sitelinks = greatest(m.sitelinks, v.sitelinks),
        website = coalesce(nullif(m.website, ''), v.website),
        wikipedia_url = coalesce(nullif(m.wikipedia_url, ''), v.wikipedia_url),
        description = coalesce(nullif(m.description, ''), v.description),
        verified = m.verified OR v.verified,
        updated_at = now()
    FROM final f JOIN museums v ON v.id = f.victim
    WHERE m.id = f.keeper
    RETURNING m.id
)
DELETE FROM museums WHERE id IN (SELECT victim FROM final)`

	tag, err := s.pool.Exec(ctx, stmt)
	if err != nil {
		return 0, fmt.Errorf("merge name variants: %w", err)
	}
	return tag.RowsAffected(), nil
}
