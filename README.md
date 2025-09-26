# Museum Scraper and Enrichment Pipeline (MinIO + Kafka)

This project crawls Wikipedia starting from the category "Category:Lists_of_museums_by_country", recursively visiting its subcategories and list pages to extract museum names. Each discovered museum is streamed into an S3-compatible object store (MinIO by default) as an individual JSON file, organized by country. When objects are created, MinIO emits events to Kafka; an optional enricher service can consume these events and process the stored museum records.

- Source: Wikipedia API (public, unauthenticated)
- Storage: S3-compatible (tested with MinIO)
- Messaging: Apache Kafka (KRaft single-broker for local dev)
- Language: Go 1.24

## What it does
- Traverses Wikipedia category members recursively (categories and pages).
- Parses simple wikitext list items (e.g., bullet points) and extracts wiki links as museum names.
- Filters out blacklisted prefixes like "Tourism", "Culture", "History", "UNESCO" to avoid non-museum links.
- Attempts to infer the country from page titles (e.g., "List of museums in France" → France).
- Streams results and writes each museum as JSON to S3/MinIO under: `raw_data/{country}/{museum-name}.json`.
- Skips writing if an object with the same key already exists.
- Optionally consumes MinIO object-created events via Kafka and iterates stored museum objects for further enrichment.

## Example output object (JSON)
```
{
  "Country": "France",
  "Name": "Musée d'Orsay"
}
```
Stored at a key like: `raw_data/france/musée-d'orsay.json` (lowercased with spaces replaced by dashes).

## Repository layout
- `cmd/parser/main.go` — CLI to run the Wikipedia scraper and write museums to S3/MinIO.
- `cmd/enricher/main.go` — CLI Kafka consumer that listens to MinIO s3:ObjectCreated:* events and iterates stored museum JSON objects (for downstream enrichment).
- `internal/enrich/` — Generic, commented pipeline abstraction to run concurrent steps per stage with sequential stages.
  - `pipeline.go`, `stage.go`, tests in `internal/enrich/pipeline_test.go`.
- `internal/service/` — Services used by apps.
  - `museumiterator.go` — Consumes Kafka events, fetches referenced museum objects from S3, and yields typed items.
- `internal/storage/` — S3/MinIO client and storage helpers.
  - `s3.go` — Create bucket, put/get objects, stream channel to S3.
- `pkg/wikipedia/` — Wikipedia API client, models, extractor, and category processing.
- `pkg/kafkaclient/` — Lightweight Kafka consumer wrapper with explicit offset commits and an iterator interface.
- `pkg/location/` — Nominatim/OpenStreetMap geocoding client and models.
- `pkg/geo/` — Country extraction and helpers.
- `pkg/graceful/` — Context helper that cancels on SIGINT/SIGTERM for graceful shutdown.
- `models/` — Data models (Museum, Coordinates, etc.).
- `docker-compose.yml` — Local stack: Kafka (KRaft), Kafka UI, MinIO, and topic/notification helpers.
- `create_topic.sh`, `create_event.sh` — Helper scripts used by Compose components to ensure topic and MinIO notifications.
- `test_data/` — Example messages and samples for local testing.

## Prerequisites
- Go 1.24+
- Docker + Docker Compose (recommended for local MinIO and Kafka)
- Internet access (to call Wikipedia API)

## Quick start
1) Start services (MinIO, Kafka, Kafka UI)

Use the provided docker-compose to run the full local stack:

```
docker compose up -d
```

This exposes:
- MinIO S3 API at http://localhost:9000
- MinIO Console at http://localhost:9001 (username: `minioadmin`, password: `minioadmin` by default)
- Kafka UI at http://localhost:8080 (connected to the local broker)
- Kafka broker external listener at localhost:29092 (for host apps)

2) Create a `.env` file

Create a `.env` in the project root with the following variables:

```
# MinIO/S3 connection
MINIO_ENDPOINT=localhost:9000
MINIO_ACCESS_KEY=minioadmin
MINIO_SECRET_KEY=minioadmin
MINIO_USE_SSL=false

# Bucket to store museum JSON objects
MUSEUM_BUCKET_NAME=museums

# Kafka configuration
# Container-to-container bootstrap (used by dockerized tools/UI)
KAFKA_BROKER=kafka:9092
# Host-to-container bootstrap (mapped port for local Go apps)
KAFKA_BROKER_LOCAL=localhost:29092
# Topic used by MinIO notifications and the enricher
KAFKA_TOPIC=raw.museum.ingestion.events.v1
# Consumer group for the enricher
KAFKA_GROUP_ID=enricher-local
```

Notes:
- The compose stack ensures the Kafka topic exists (`kafka-init`) and configures MinIO bucket notifications to publish s3:ObjectCreated:* events to Kafka.
- The app will create the bucket if it doesn’t exist.

3) Run the scraper (producer of objects)

```
go run ./cmd/parser
```

You’ll see progress logs printed to the console. The process will traverse subcategories and list pages, extracting museums and writing objects into your MinIO bucket. Browse objects in the MinIO console at http://localhost:9001.

4) (Optional) Run the enricher (consumer of events)

The enricher listens on `${KAFKA_BROKER_LOCAL}` for `${KAFKA_TOPIC}` and processes each created S3 object. Ensure your `.env` has KAFKA_BROKER_LOCAL set to the mapped host port (`localhost:29092`).

```
go run ./cmd/enricher
```

Tip: Open Kafka UI at http://localhost:8080 to watch messages arriving on `${KAFKA_TOPIC}`.

## Configuration
- Wikipedia user-agent: set in `pkg/wikipedia/api.go` (default `Golang_Spider_Bot/3.0`). If you fork for production use, please set a descriptive user-agent.
- Museum extraction blacklist: configured in `cmd/parser/main.go` when creating `NewMuseumExtractor([]string{"Tourism", "Culture", "History", "UNESCO"})`.
- MinIO/S3 endpoint and credentials: read from environment variables loaded by `.env` via `github.com/joho/godotenv`.

Environment variables summary:
- `MINIO_ENDPOINT` (e.g., `localhost:9000` or `play.min.io:9000`)
- `MINIO_ACCESS_KEY`
- `MINIO_SECRET_KEY`
- `MINIO_USE_SSL` (`true` or `false`)
- `MUSEUM_BUCKET_NAME` (e.g., `museums`)
- `KAFKA_BROKER` (container-to-container, e.g., `kafka:9092` — used by dockerized tools/UI)
- `KAFKA_BROKER_LOCAL` (host-to-container, e.g., `localhost:29092` — used by local Go apps)
- `KAFKA_TOPIC` (default `raw.museum.ingestion.events.v1`)
- `KAFKA_GROUP_ID` (consumer group for the enricher)

## How it works (Architecture)
- `pkg/wikipedia.WikipediaClient` fetches category members and page contents via the Wikipedia API.
- `pkg/wikipedia.CategoryService` handles pagination for category members and extracts wikitext for pages.
- `pkg/wikipedia.MuseumExtractor` scans wikitext lines, collecting wiki-linked entries from list items while skipping blacklisted prefixes.
- `pkg/wikipedia.CategoryProcessor` recursively walks categories and streams museum records via a channel.
- `internal/storage.S3Service` connects to MinIO/S3 and writes JSON objects per museum, skipping writes if an object already exists.

### Event pipeline (MinIO → Kafka → Enricher)
- MinIO is configured (via docker-compose) with a Kafka notification target and a bucket event rule.
- When a museum JSON is created under `raw_data/...`, MinIO publishes an `s3:ObjectCreated:*` event to the Kafka topic `${KAFKA_TOPIC}`.
- The `cmd/enricher` service consumes those events from Kafka, reads the referenced object from MinIO via `internal/storage.S3Service`, and can enrich/process the museum.
- You can observe incoming events and payloads via Kafka UI at http://localhost:8080.

### Enrichment pipeline (internal/enrich)
A small, generic pipeline abstraction to coordinate concurrent steps per stage, with sequential stages.

- Stages run sequentially; steps inside a stage run in parallel for each item.
- Errors in steps are logged; processing of the item continues.

Example usage:

```
// Define steps
stepA := func(ctx context.Context, m *MyItem) error { /* mutate m */ return nil }
stepB := func(ctx context.Context, m *MyItem) error { /* mutate m */ return nil }

// Group steps into stages
stage1 := enrich.NewStage(stepA, stepB) // runs A and B concurrently for each item

// Build pipeline and process a channel
pipe := enrich.NewPipeline(stage1)
out := pipe.Process(ctx, in) // out yields items after all stages finish
```

See `internal/enrich/pipeline_test.go` for behavior and guarantees.

### Event consumer iterator (internal/service)
`internal/service.MuseumIterator` provides a channel of enriched items by:
- Consuming Kafka messages (MinIO events) via `pkg/kafkaclient` iterator.
- Deserializing MinIO event payloads.
- Fetching the referenced museum JSON from S3 via `internal/storage.S3Service`.
- Emitting typed items and committing offsets after successful processing.

## Development
- Build the app:

```
go build ./...
```

- Run scraper:

```
go run ./cmd/parser
```

- Run enricher:

```
go run ./cmd/enricher
```

- Lint/format:

```
go fmt ./...
```

## Testing
Tests exist across multiple packages, including:
- `internal/enrich` — pipeline and stage behavior
- `pkg/kafkaclient` — Kafka client and iterator
- `pkg/graceful` — context cancellation
- `pkg/location` — geocoding service helpers
- `pkg/wikipedia` — fetching and processing logic

Run all tests:

```
go test ./...
```

Note: Prefer mocking external HTTP/Kafka where possible.

## Troubleshooting
- Error: "Error loading .env file": Ensure a `.env` file exists in the project root with required variables.
- Error creating MinIO client: Verify `MINIO_ENDPOINT`, credentials, and `MINIO_USE_SSL`. For local docker-compose, use the values in the example `.env` above.
- No objects appear in the bucket: Confirm that `MUSEUM_BUCKET_NAME` is set and that MinIO is running. Check application logs for any Wikipedia API errors or rate limiting.
- Kafka broker not reachable: If running the apps on your host, ensure you use `KAFKA_BROKER_LOCAL=localhost:29092`. Inside containers, use `kafka:9092`.
- Topic missing: The compose stack includes a topic initializer; if disabled, create it manually: `kafka-topics.sh --bootstrap-server localhost:29092 --create --if-not-exists --topic raw.museum.ingestion.events.v1 --partitions 1 --replication-factor 1`.
- No events in Kafka UI: Verify MinIO bucket notifications are configured and that new objects are being written under `raw_data/...`.
- Wikipedia rate limits or blocks: Use a more descriptive user-agent and consider adding backoff/retry logic if you extend this project.

## Notes
- Country extraction is heuristic and based on page titles; results can contain anomalies. You can improve `pkg/geo.ExtractCountry` for better accuracy.
- Object keys are sanitized by lowercasing and replacing spaces with dashes; special characters are preserved and may affect downstream tooling.

## License
All rights reserved or as specified by the repository owner. Update this section if you choose a specific open-source license.
