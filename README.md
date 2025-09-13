# Museum Scraper to S3 (MinIO)

This project crawls Wikipedia starting from the category "Category:Lists_of_museums_by_country", recursively visiting its subcategories and list pages to extract museum names. It streams each discovered museum into an S3-compatible object store (MinIO by default) as individual JSON files, organized by country.


- Source: Wikipedia API (public, unauthenticated)
- Storage: S3-compatible (tested with MinIO)
- Language: Go 1.24

## What it does
- Traverses Wikipedia category members recursively (categories and pages).
- Parses simple wikitext list items (e.g., bullet points) and extracts wiki links as museum names.
- Filters out blacklisted prefixes like "Tourism", "Culture", "History", "UNESCO" to avoid non-museum links.
- Attempts to infer the country from page titles (e.g., "List of museums in France" → France).
- Streams results and writes each museum as JSON to S3/MinIO under: `raw_data/{country}/{museum-name}.json`.
- Avoids re-writing an object if it already exists.

## Example output object (JSON)
```
{
  "Country": "France",
  "Name": "Musée d'Orsay"
}
```
Stored at a key like: `raw_data/france/musée-d'orsay.json` (lowercased with spaces replaced by dashes).

## Repository layout
- `cmd/parser/main.go` — Application entrypoint. Loads `.env`, kicks off Wikipedia processing and storage.
- `cmd/enricher/main.go` — Kafka consumer that listens to MinIO s3:ObjectCreated:* events and geocodes museum locations.
- `wikipedia/` — Wikipedia API client, models, extractor, and processing logic.
- `storage/` — S3/MinIO client and storage helpers.
- `kafkaclient/` — Lightweight Kafka consumer wrapper used by the enricher.
- `location/` — Nominatim (OpenStreetMap) geocoding client and models.
- `geo/` — Country extraction helpers.
- `models/` — Data models (Museum, Coordinates, etc.).
- `docker-compose.yml` — Local stack: Kafka (KRaft), Kafka UI, MinIO, and initialization helpers.

## Prerequisites
- Go 1.24+
- Docker + Docker Compose (recommended for local MinIO)
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
# Container-to-container bootstrap
KAFKA_BROKER=kafka:9092
# Host-to-container bootstrap (mapped port for local apps)
KAFKA_BROKER_LOCAL=localhost:29092
# Topic used by MinIO notifications and the enricher
KAFKA_TOPIC=raw.museum.ingestion.events.v1
# Consumer group for the enricher
KAFKA_GROUP_ID=enricher-local
```

Notes:
- The compose stack ensures the Kafka topic exists and configures MinIO bucket notifications to publish s3:ObjectCreated:* events to Kafka.
- The app will create the bucket if it doesn’t exist.

3) Run the scraper (producer of objects)

```
go run ./cmd/parser
```

You’ll see progress logs printed to the console. The process will traverse subcategories and list pages, extracting museums and writing objects into your MinIO bucket. You can browse objects in the MinIO console at http://localhost:9001.

4) (Optional) Run the enricher (consumer of events)

The enricher listens on `${KAFKA_BROKER_LOCAL}` for `${KAFKA_TOPIC}` and geocodes each museum from the newly created S3 object.

```
go run ./cmd/enricher
```

Tip: Open Kafka UI at http://localhost:8080 to watch messages arriving on `${KAFKA_TOPIC}`.

## Configuration
- Wikipedia user-agent: set in `wikipedia/api.go` (default `Golang_Spider_Bot/3.0`). If you fork for production use, please set a descriptive user-agent.
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
- `wikipedia.WikipediaClient` fetches category members and page contents via Wikipedia API.
- `wikipedia.CategoryService` handles pagination for category members and extracts wikitext for pages.
- `wikipedia.MuseumExtractor` scans wikitext lines, collecting wiki-linked entries from list items while skipping blacklisted prefixes.
- `wikipedia.CategoryProcessor` recursively walks categories and streams museum records via a channel.
- `storage.S3Service` connects to MinIO/S3 and writes JSON objects per museum, skipping writes if an object already exists.

### Event pipeline (MinIO → Kafka → Enricher)
- MinIO is configured (via docker-compose) with a Kafka notification target and a bucket event rule.
- When a museum JSON is created under `raw_data/...`, MinIO publishes an `s3:ObjectCreated:*` event to the Kafka topic `${KAFKA_TOPIC}`.
- The `cmd/enricher` service consumes those events from Kafka, reads the referenced object from MinIO, and geocodes the museum using the `location` package (Nominatim).
- You can observe incoming events and payloads via Kafka UI at http://localhost:8080.

## Development
- Build the app:

```
go build ./...
```

- Run with live logs:

```
go run ./cmd/parser
```

- Linting/formatting:

```
go fmt ./...
```

## Testing
There is a test file under `wikipedia/`. To run all tests:

```
go test ./...
```

Note: Some tests may rely on embedded or large strings and may not exercise external HTTP calls. If you add new tests that hit Wikipedia, prefer mocking the client.

## Troubleshooting
- Error: "Error loading .env file": Ensure a `.env` file exists in the project root with required variables.
- Error creating MinIO client: Verify `MINIO_ENDPOINT`, credentials, and `MINIO_USE_SSL`. For local docker-compose, use the values in the example `.env` above.
- No objects appear in the bucket: Confirm that `MUSEUM_BUCKET_NAME` is set and that MinIO is running. Check application logs for any Wikipedia API errors or rate limiting.
- Kafka broker not reachable: If running the apps on your host, ensure you use `KAFKA_BROKER_LOCAL=localhost:29092`. Inside containers, use `kafka:9092`.
- Topic missing: The compose stack includes a topic initializer; if disabled, create it manually: `kafka-topics.sh --bootstrap-server localhost:29092 --create --if-not-exists --topic raw.museum.ingestion.events.v1 --partitions 1 --replication-factor 1`.
- No events in Kafka UI: Verify MinIO bucket notifications are configured and that new objects are being written under `raw_data/...`. Recreate the bucket event rule if needed.
- Wikipedia rate limits or blocks: Use a more descriptive user-agent and consider adding backoff/retry logic if you extend this project.

## Notes
- The country extraction is heuristic and based on page titles; results can contain anomalies. You can improve `geo.ExtractCountry` for better accuracy.
- Object keys are sanitized by lowercasing and replacing spaces with dashes; special characters are preserved and may affect downstream tooling.


## License
All rights reserved or as specified by the repository owner. Update this section if you choose a specific open-source license.
