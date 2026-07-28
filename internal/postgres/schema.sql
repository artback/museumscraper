-- Schema for the museum catalogue.
--
-- Postgres replaces three hand-rolled indexes: the degree-cell geo grid, the
-- sharded text index, and the full scans the audit used. Those were rebuilt by
-- a separate command and could silently drift from the records they were
-- derived from — the failure that made "reindex" necessary in the first place.
-- Here the indexes are maintained by the database, transactionally.
--
-- Applied on every start; each statement is idempotent.

CREATE EXTENSION IF NOT EXISTS postgis;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE IF NOT EXISTS museums (
    id            bigserial PRIMARY KEY,

    -- Wikidata's identifier when a source supplied one. Not every museum has
    -- one, so it cannot be the primary key.
    wikidata_id   text,

    name          text NOT NULL,
    -- The name reduced to its comparable form: lowercase, accents folded,
    -- punctuation turned into separators. Trigram and prefix matching both run
    -- against this rather than the display name.
    normalized    text NOT NULL,

    country       text,
    locality      text,
    description   text,
    website       text,
    wikipedia_url text,
    page_id       integer,
    source_page   text,
    aliases       text[]      NOT NULL DEFAULT '{}',
    sources       text[]      NOT NULL DEFAULT '{}',
    verified      boolean     NOT NULL DEFAULT false,

    -- Null for the quarter of the catalogue no source could place. Those
    -- records are unreachable by radius query and findable only by name, which
    -- is why the column is nullable rather than defaulted to a point.
    location      geography(Point, 4326),

    updated_at    timestamptz NOT NULL DEFAULT now(),

    -- One column to upsert on. A museum is identified by its Wikidata id where
    -- it has one, and otherwise by its name and country — the same rule the
    -- in-process merger uses, so the two cannot disagree about what counts as
    -- the same museum.
    identity      text GENERATED ALWAYS AS (
                      coalesce(nullif(wikidata_id, ''), normalized || '|' || coalesce(country, ''))
                  ) STORED
);

CREATE UNIQUE INDEX IF NOT EXISTS museums_identity_idx ON museums (identity);

-- Radius queries. GIST over geography gives ST_DWithin an index to use, and
-- the same index orders results by distance.
CREATE INDEX IF NOT EXISTS museums_location_idx ON museums USING gist (location);

-- Similarity search. GIN over trigrams is what makes "rijkmuseum" reach
-- "Rijksmuseum" and "guggenhiem" reach "Guggenheim".
CREATE INDEX IF NOT EXISTS museums_normalized_trgm_idx ON museums USING gin (normalized gin_trgm_ops);

-- Prefix matching, for a name being typed. text_pattern_ops is required for
-- LIKE 'foo%' to use an index.
CREATE INDEX IF NOT EXISTS museums_normalized_prefix_idx ON museums (normalized text_pattern_ops);

-- One column holding everything a name query should match: the normalised
-- name, the alternative names, and the town.
--
-- Matching these with separate OR'd predicates — in particular an EXISTS over
-- the aliases array — gave the planner nothing it could combine, and every
-- search fell back to a sequential scan over the whole table. Folded into one
-- indexed column, the same query is answered from the trigram index.
-- Written by the application rather than generated: array_to_string is not
-- immutable, which a generated column requires, and the normalisation the
-- column needs already happens in Go.
ALTER TABLE museums ADD COLUMN IF NOT EXISTS search_text text NOT NULL DEFAULT '';

-- The town, normalised the same way as the name, so a query mentioning it can
-- be recognised with one cheap comparison per candidate row.
ALTER TABLE museums ADD COLUMN IF NOT EXISTS locality_normalized text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS museums_search_trgm_idx ON museums USING gin (search_text gin_trgm_ops);

-- How many Wikipedia language editions cover this museum. The closest thing
-- the sources offer to prominence, and the only way to rank the Louvre above
-- the Louvre-Lens when a query names neither more specifically.
ALTER TABLE museums ADD COLUMN IF NOT EXISTS sitelinks integer NOT NULL DEFAULT 0;

-- The search score takes ln(1 + sitelinks), and ln(0) is an error rather than
-- an infinity in Postgres. A single negative value would therefore fail every
-- search whose candidate set happened to include that row — not just that row.
-- Cheap to forbid outright.
DO $$
BEGIN
    ALTER TABLE museums ADD CONSTRAINT museums_sitelinks_non_negative CHECK (sitelinks >= 0);
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

-- The postal address the enrichment stage resolved, kept as columns rather
-- than as a blob: a street and a postcode are what a visitor-facing caller
-- actually needs, and burying them in JSON makes them unqueryable.
ALTER TABLE museums ADD COLUMN IF NOT EXISTS street   text NOT NULL DEFAULT '';
ALTER TABLE museums ADD COLUMN IF NOT EXISTS postcode text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS museums_postcode_idx ON museums (postcode) WHERE postcode <> '';

CREATE INDEX IF NOT EXISTS museums_country_idx ON museums (country);

CREATE TABLE IF NOT EXISTS exhibitions (
    -- The URL is the exhibition's identity: two listings pointing at the same
    -- page are the same exhibition, whichever museum's site they were read
    -- from.
    url          text PRIMARY KEY,

    title        text NOT NULL,
    museum       text,
    museum_wikidata_id text,

    starts_on    date,
    ends_on      date,

    location     geography(Point, 4326),
    source_page  text,
    scraped_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS exhibitions_location_idx ON exhibitions USING gist (location);

-- "What is on now" filters on the closing date, so it leads the index.
CREATE INDEX IF NOT EXISTS exhibitions_ends_idx ON exhibitions (ends_on);
