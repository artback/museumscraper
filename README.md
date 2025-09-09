# Museum Scraper to S3 (MinIO)

This project crawls Wikipedia starting from the category "Category:Lists_of_museums_by_country", recursively visiting its subcategories and list pages to extract museum names. It streams each discovered museum into an S3-compatible object store (MinIO by default) as individual JSON files, organized by country.

- Source: Wikipedia API (public, unauthenticated)
- Storage: S3-compatible (tested with MinIO)
- Language: Go 1.25

## What it does
- Traverses Wikipedia category members recursively (categories and pages).
- Parses simple wikitext list items (e.g., bullet points) and extracts wiki links as museum names.
- Filters out blacklisted prefixes like "Tourism", "Culture", "History", "UNESCO" to avoid non-museum links.
- Attempts to infer the country from page titles (e.g., "List of museums in France" → France).
- Streams results and writes each museum as JSON to S3/MinIO under: `museums/{country}/{museum-name}.json`.
- Avoids re-writing an object if it already exists.

## Example output object (JSON)
```
{
  "Country": "France",
  "Name": "Musée d'Orsay"
}
```
Stored at a key like: `museums/france/musée-d'orsay.json` (lowercased with spaces replaced by dashes).

## Repository layout
- `main.go` — Application entrypoint. Loads `.env`, kicks off Wikipedia processing and storage.
- `wikipedia/` — Wikipedia API client, models, extractor, and processing logic.
- `storage/` — S3/MinIO client and storage helpers.
- `geo/` — Country extraction helpers.
- `models/` — Data models (Museum, Coordinates, etc.).
- `docker-compose.yml` — Local MinIO service for development.

## Prerequisites
- Go 1.24+
- Docker + Docker Compose (recommended for local MinIO)
- Internet access (to call Wikipedia API)

## Quick start
1) Start MinIO locally

Use the provided docker-compose to run MinIO and its console:

```
docker compose up -d
```

This exposes:
- S3 API at http://localhost:9000
- Console at http://localhost:9001 (username: `minioadmin`, password: `minioadmin` by default)

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
```

Note: The app will create the bucket if it doesn’t exist.

3) Run the scraper

```
go run .
```

You’ll see progress logs printed to the console. The process will traverse subcategories and list pages, extracting museums and writing objects into your MinIO bucket. You can browse objects in the MinIO console at http://localhost:9001.

## Configuration
- Wikipedia user-agent: set in `wikipedia/api.go` (default `Golang_Spider_Bot/3.0`). If you fork for production use, please set a descriptive user-agent.
- Museum extraction blacklist: configured in `main.go` when creating `NewMuseumExtractor([]string{"Tourism", "Culture", "History", "UNESCO"})`.
- MinIO/S3 endpoint and credentials: read from environment variables loaded by `.env` via `github.com/joho/godotenv`.

Environment variables summary:
- `MINIO_ENDPOINT` (e.g., `localhost:9000` or `play.min.io:9000`)
- `MINIO_ACCESS_KEY`
- `MINIO_SECRET_KEY`
- `MINIO_USE_SSL` (`true` or `false`)
- `MUSEUM_BUCKET_NAME` (e.g., `museums`)

## How it works (Architecture)
- `wikipedia.WikipediaClient` fetches category members and page contents via Wikipedia API.
- `wikipedia.CategoryService` handles pagination for category members and extracts wikitext for pages.
- `wikipedia.MuseumExtractor` scans wikitext lines, collecting wiki-linked entries from list items while skipping blacklisted prefixes.
- `wikipedia.CategoryProcessor` recursively walks categories and streams museum records via a channel.
- `storage.S3Service` connects to MinIO/S3 and writes JSON objects per museum, skipping writes if an object already exists.

## Development
- Build the app:

```
go build ./...
```

- Run with live logs:

```
go run .
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
- Wikipedia rate limits or blocks: Use a more descriptive user-agent and consider adding backoff/retry logic if you extend this project.

## Notes
- The country extraction is heuristic and based on page titles; results can contain anomalies. You can improve `geo.ExtractCountry` for better accuracy.
- Object keys are sanitized by lowercasing and replacing spaces with dashes; special characters are preserved and may affect downstream tooling.

## License
All rights reserved or as specified by the repository owner. Update this section if you choose a specific open-source license.
