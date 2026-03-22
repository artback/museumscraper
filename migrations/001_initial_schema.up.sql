CREATE EXTENSION IF NOT EXISTS postgis;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE IF NOT EXISTS museums (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    country TEXT NOT NULL,
    city TEXT,
    state TEXT,
    address TEXT,
    lat DOUBLE PRECISION,
    lon DOUBLE PRECISION,
    location GEOGRAPHY(POINT, 4326),
    osm_id BIGINT,
    osm_type TEXT,
    category TEXT,
    museum_type TEXT,
    wikipedia_url TEXT,
    website TEXT,
    raw_tags JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(name, country)
);

CREATE INDEX idx_museums_location ON museums USING GIST(location);
CREATE INDEX idx_museums_country ON museums(country);
CREATE INDEX idx_museums_city ON museums(city);
CREATE INDEX idx_museums_name_trgm ON museums USING GIN(name gin_trgm_ops);

CREATE TABLE IF NOT EXISTS exhibitions (
    id BIGSERIAL PRIMARY KEY,
    museum_id BIGINT NOT NULL REFERENCES museums(id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    description TEXT,
    start_date DATE,
    end_date DATE,
    is_permanent BOOLEAN DEFAULT FALSE,
    source_url TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_exhibitions_museum_id ON exhibitions(museum_id);
CREATE INDEX idx_exhibitions_dates ON exhibitions(start_date, end_date);
CREATE INDEX idx_exhibitions_active ON exhibitions(end_date) WHERE end_date IS NULL OR end_date >= CURRENT_DATE;
