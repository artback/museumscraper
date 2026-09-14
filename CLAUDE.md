# Working on the museum catalogue

One Go binary, `museum`, with seven subcommands that never call each other. See
README.md for what each one does and why the architecture is shaped this way.
This file is only about running it.

## Tests

```bash
go test -short ./...        # offline only — use this by default
go test -race -short ./...  # before committing anything concurrent
go test ./...               # also hits Wikidata, Wikipedia, Overpass, Nominatim
```

Prefer `-short`. The full run makes real requests to rate-limited public
services, and Nominatim's usage policy is not a suggestion.

## Running locally

```bash
docker compose up -d                    # postgres+postgis, minio, kafka, api, enricher
docker compose run --rm jobs crawl      # batch jobs go through the "jobs" service
docker compose run --rm jobs reindex
docker compose run --rm jobs verify -samples 3
docker compose run --rm jobs harvest list
```

The API is then on <http://localhost:8090>, the map on `/map`.

Set `MUSEUM_CONTACT` before any run that leaves the machine. It is composed into
the User-Agent every source sees, and Wikimedia's and Nominatim's policies both
ask for a way to reach whoever is crawling them. Unset, the crawler logs that it
is unset and carries the project URL alone.

Batch jobs share one `jobs` service on purpose: `docker compose run` replaces a
service's command outright, so giving each job its own service with a baked-in
subcommand meant the subcommand vanished the moment you passed a flag.

## Running in production

It runs on the homelab Nomad cluster — a single Raspberry Pi, `100.116.81.88`,
reachable over Tailscale.

| | |
|---|---|
| Map | <http://100.116.81.88/map> |
| API | `http://100.116.81.88:8091` (fabio also routes `/map` and `/v1`) |
| Job spec | `packs/museum` in [artback/iac_jonathan](https://github.com/artback/iac_jonathan) |
| Deploy | push to `main` — see `.github/workflows/README.md` |

The job spec deliberately lives in the IaC repository alongside the sixteen
other services on the same Pi; this repository holds only the workflow that
builds the image and triggers a deploy.

To deploy by hand instead:

```bash
cd ~/Code/iac_jonathan
nomad-pack run packs/museum -f vars/museum-kalmar.pkrvars.hcl
```

## The extraction harness

`museum harvest` generates JavaScript extractors with a local model and runs
them in a sandbox. It needs an OpenAI-compatible endpoint:

```bash
EXTRACT_MODEL_ENDPOINT=http://localhost:11434/v1   # Ollama's compatible path
EXTRACT_MODEL=qwen2.5-coder:7b
HARVEST_BUCKET_NAME=museum-harvest                 # defaults to $MUSEUM_BUCKET_NAME-harvest
```

Generation is slow — minutes for one page on the Pi — and it is meant to be.
Nothing on the steady-state path touches the model.

Generated scripts get a standard library on the global `museum` (see
`internal/harvest/library.go`). `museum.dates` is the same
`exhibitions.ParseDateRange` the hand-written scraper uses, so **improving it
improves every extractor already in the store, without regenerating any of
them** — that is the intended place to put new parsing knowledge. Everything in
the library must stay a pure function: it is installed into the sandbox, and a
helper that fetched or wrote anything would undo the isolation the whole design
rests on.

## Six things that will bite you

**The Pi is arm64.** Any image the job references must be too. `postgis/postgis`
publishes amd64 only and simply will not run there; `imresamu/postgis` is the
multi-arch build of the same Dockerfile. A wrong-architecture image fails as an
opaque `exec format error` long after the build went green.

**Seeding must not raise bucket events.** The enricher does not check whether a
record has already been enriched — it geocodes every museum an event names, then
makes a second Nominatim call for the place details. Loading the existing
345,000 objects into a bucket with the notification attached queues roughly
690,000 requests against a service that allows one per second. Use
`seed_mode = true` for bulk loads; `packs/museum/scripts/migrate-data.sh`
refuses to run without it.

**Ports on the Pi are contested and not all discoverable.** 8090 is beszel, 9000
is mealie, 5432 is the shared postgres, 9100 is node-exporter. That last one has
no pack in the IaC repository, so grepping the job files does not find it —
taking it put node-exporter into a restart loop and blinded Prometheus. Check
the running cluster, not just the repo.

**The harness needs its own bucket.** It writes artifacts and run records
continuously, and the enricher geocodes from bucket notifications without
checking whether a record has already been enriched. Putting harvest state in
the bucket carrying the notification would queue Nominatim calls for objects
that are not museums at all — the same failure as seeding without
`seed_mode`, arrived at from a different direction. `HARVEST_BUCKET_NAME`
defaults to a separate bucket, and should stay one.

**A change to listing discovery does not reach swept sites for a quarter.** A
site whose listings were found once is re-read by replaying those pages, so the
home page — and with it every discovery heuristic — is skipped until
`sweep.RediscoverAfter` (90 days) has passed. Improving `candidateListingURLs`
or the home-page link scoring therefore shows up on new sites immediately and on
the existing catalogue slowly. To measure a change now, clear `discovered_at`:
`UPDATE site_scrapes SET discovered_at = NULL` puts every site back through full
discovery on its next read, at one extra request each.

**Tasks in a Nomad group share a network namespace.** A task reaching a sibling
through the host's published port hairpins back into its own namespace and never
connects. Use `localhost` and the container port for siblings; the host IP and
the static port only for other groups.
