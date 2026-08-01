# Museum Catalogue

A catalogue of the world's museums — around **180,000** of them — assembled from four public sources, geocoded, indexed for location queries, and served over HTTP together with the exhibitions currently on show.

```
GET /v1/museums?place=Kyoto                              →  ~4 ms
GET /v1/museums?lat=48.8566&lon=2.3522&radius_km=2       →  ~1 ms
GET /v1/exhibitions?place=London                         →  ~4 ms
```

- **Sources**: Wikidata Query Service, Wikipedia action API, OpenStreetMap Overpass, museum websites
- **Storage**: PostgreSQL 16 with PostGIS and pg_trgm; S3-compatible object storage (MinIO) for source records
- **Messaging**: Apache Kafka (KRaft, single broker for local dev)
- **Language**: Go 1.25

---

## How it fits together

Everything is one binary, `museum`, with six subcommands. They never call each other: each one reads and writes a single object-storage bucket and coordinates through key prefixes alone. That is why this is *not* a microservice architecture — there are no service-to-service calls to make it one.

```mermaid
flowchart TB
    subgraph ext["External sources"]
        direction LR
        WD["Wikidata<br/>SPARQL"]
        WP["Wikipedia<br/>action API"]
        OSM["OpenStreetMap<br/>Overpass"]
        NOM["Nominatim<br/>geocoder"]
        WEB["Museum<br/>websites"]
    end

    CRAWL["<b>museum crawl</b><br/><i>scheduled - weekly</i>"]
    ENRICH["<b>museum enrich</b><br/><i>long-running consumer</i>"]
    REFRESH["<b>museum refresh</b><br/><i>scheduled - daily</i>"]
    REINDEX["<b>museum reindex</b><br/><i>after crawl / enrich</i>"]
    VERIFY["<b>museum verify</b><br/><i>after every crawl</i>"]
    SERVE["<b>museum serve</b><br/><i>long-running service</i>"]

    subgraph bucket["Object storage - the durable record"]
        direction LR
        RAW[("raw_data/<br/>one object per museum")]
        ENR[("enriched_data/<br/>plus address, website")]
    end

    subgraph db["PostgreSQL + PostGIS + pg_trgm"]
        direction LR
        MUSEUMS[("museums<br/>geography, trigram")]
        EXHIB[("exhibitions<br/>geography, dates")]
    end

    KAFKA{{"Kafka<br/>ObjectCreated events"}}
    CLIENT(["HTTP client"])

    WD --> CRAWL
    WP --> CRAWL
    OSM --> CRAWL
    CRAWL -- "merge + dedupe" --> RAW
    CRAWL --> MUSEUMS

    RAW -. "bucket notification<br/>prefix raw_data/" .-> KAFKA
    KAFKA --> ENRICH
    NOM --> ENRICH
    ENRICH --> ENR

    RAW --> REINDEX
    ENR --> REINDEX
    REINDEX -- "enriched wins,<br/>raw as fallback" --> MUSEUMS

    MUSEUMS --> REFRESH
    WEB --> REFRESH
    REFRESH --> EXHIB

    MUSEUMS --> VERIFY
    MUSEUMS --> SERVE
    EXHIB --> SERVE
    SERVE --> CLIENT
```

**Reading the diagram:** solid arrows are direct reads and writes; the dotted arrow is MinIO's bucket notification, the only asynchronous hop. The enrichment loop is deliberately closed — `reindex` prefers the enriched copy of a museum over the raw one, which is what makes the geocoding reach the API.

**Two stores, two jobs.** Object storage keeps the durable record of what each source said, and is what MinIO's notifications fire from. Postgres is what answers queries. It replaced three hand-rolled indexes — a degree-cell geo grid, a sharded JSON text index, and the full scans the audit ran — with indexes the database maintains transactionally. That removed an entire class of bug: those indexes were derived data with no invalidation, silently stale whenever the command that rebuilt them had not run, and indistinguishable from correct results.

### Why one binary

The six subcommands have genuinely different lifecycles, and that is a good reason to run them as separate *processes*:

| Subcommand | Lifecycle | Typical cadence |
| --- | --- | --- |
| `serve` | long-running service | always up |
| `enrich` | long-running consumer | always up |
| `crawl` | batch, runs to completion | weekly |
| `refresh` | batch, runs to completion | daily |
| `reindex` | batch, runs to completion | after `crawl`, and after `enrich` catches up |
| `verify` | batch, runs to completion | after every crawl |
| `query` | interactive | on demand |

Batch jobs and tools run through one `jobs` compose service, because `docker compose run` *replaces* a service's command — giving each job its own service with a baked-in subcommand meant the subcommand was dropped the moment you passed a flag, and the binary printed its help instead of running.

```bash
docker compose run --rm jobs crawl
docker compose run --rm jobs refresh -place "Paris, France" -radius 3
docker compose run --rm jobs verify -samples 3
```

It is not a reason to build six *programs*. They share connection settings, storage access, models and logging; splitting them meant six copies of the same wiring plus two CLIs that duplicated the API. One binary, one image, six entry points.

---

## Quick start

**1. Bring up the stack**

```bash
docker compose up -d
```

That starts Kafka, MinIO, the API and the enricher, and runs two init containers that create the Kafka topic and attach MinIO's bucket notification. Check the wiring took:

```bash
docker logs minio-init      # must report the notification was created
curl localhost:8090/health
```

| Service | Address |
| --- | --- |
| API | http://localhost:8090 |
| MinIO console | http://localhost:9001 (`minioadmin` / `minioadmin`) |
| Kafka UI | http://localhost:8080 |

**2. Fill the catalogue.** Batch jobs sit behind the `jobs` compose profile, so they run on request rather than at startup:

```bash
docker compose run --rm jobs crawl      # ~75 min for all sources
docker compose run --rm jobs reindex    # ~20 s
docker compose run --rm jobs verify     # audit what was collected
```

**3. Add exhibitions**

```bash
docker compose run --rm jobs refresh -place "Paris, France" -radius 3
```

**4. Query**

```bash
curl 'localhost:8090/v1/museums?lat=48.8566&lon=2.3522&radius_km=2&limit=5'
curl 'localhost:8090/v1/exhibitions?lat=48.8566&lon=2.3522&radius_km=2'
```

### Running from source instead

```bash
go build -o museum ./cmd/museum
./museum help
```

`.env` in the working directory supplies the connection settings; the checked-in one points at the compose stack from the host.

> **Note on `localhost`.** The host endpoints in `.env` use `127.0.0.1`, not `localhost`. Docker publishes an IPv6 mapping that does not accept connections on macOS, and Go tries `::1` first. Inside the compose network the services address each other by name (`minio:9000`, `kafka:9092`), which compose sets for them.

---

## Commands

Run `museum <command> -h` for the full flag list.

### `museum crawl` — build the catalogue

```bash
museum crawl                                        # wikidata, category, lists
museum crawl -sources wikidata                       # fastest single source
museum crawl -sources wikidata,category,lists,osm    # maximum coverage
```

Sources run concurrently into a shared merger, then everything is written at once.

> **The `lists` source is currently ineffective.** All four sources run
> concurrently, and `wikidata`, `category` and `lists` all draw on the Wikipedia
> and Wikidata APIs at once. In the last full crawl `lists` was rate-limited to
> 14 candidates — "Lists of museums in the United States" and "Lists of museums
> in England by county" were both skipped after four 429s. Running the
> Wikipedia-backed sources sequentially, or sharing one rate limiter between
> them, would fix it. `category` and `osm` were unaffected.

> **Run the sources together in one invocation.** Merging happens *within* a run. Two runs of different sources produce two independent record sets, and the second skips keys that already exist — so the same museum can end up stored twice under different names (`raw_data/france/army-museum-paris.json` from the list crawl and `raw_data/france/musee-de-l-armee.json` from Wikidata).

Interrupting with Ctrl-C stops collecting but still stores what the sources returned: the persistence phase runs on its own context so a cancelled crawl does not discard an hour of work.

### `museum enrich` — geocode stored museums

Consumes MinIO `ObjectCreated` events from Kafka, geocodes each museum against Nominatim, fetches the full OpenStreetMap place record, and writes to `enriched_data/`.

Delivery is **at-least-once**: the Kafka offset advances only once a museum has been through every stage and written back. It previously advanced when the pipeline *received* an item, so an interrupt part-way through enrichment lost that museum permanently — the offset was past it, and the crawl does not re-emit an event for an object that already exists.

Nominatim allows one request per second and each museum costs two, so expect roughly 25 museums per minute. That is by design, not a bottleneck to tune.

### `museum reindex` — rebuild the geo index

```bash
museum reindex
```

```
Read 86052 museums in 6s (4206 enriched, 19567 have no coordinates)
Loaded 86052 museums in 9s: 84584 in the database, 65896 with coordinates, 282 countries
```

Reads `raw_data/` and `enriched_data/`, **preferring the enriched copy**, and upserts into Postgres. Run it after a crawl, and again once enrichment has caught up.

`crawl` loads the database itself, so this is only needed when the two have drifted: after enrichment adds coordinates, after records were written by some other route, or to repair a partial load. A failed database load during a crawl is logged rather than fatal — the records are already in object storage, which is the durable copy.

### `museum refresh` — scrape exhibitions

```bash
museum refresh -place "Paris, France" -radius 3
museum refresh -all -max-museums 2000
```

Scraping twenty museum websites takes about **16 seconds**, and politeness limits mean that cost cannot come down — so it is a background job, not part of a request. Run it daily; exhibition runs change over weeks.

A refresh replaces only the entries for the museums it just scraped, so a run scoped to one city does not wipe another city's results out of a shared cell.

### `museum serve` — the HTTP API

```bash
museum serve -addr :8090
```

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/search?q=…` | Museums by name — the only interface that reaches the 23% with no coordinates |
| `GET /v1/museums` | Museums near a point, or in a named place |
| `GET /v1/exhibitions` | What is on show near a point, or in a named place |
| `GET /health` | What the catalogue holds |
| `GET /livez` | The process is running |
| `GET /readyz` | The catalogue can be queried |

**Locating a query.** Every location endpoint takes either coordinates
(`lat`, `lon`, `radius_km`) or a place name (`place`). Coordinates win if both
are given.

```bash
curl 'localhost:8090/v1/exhibitions?place=London'
curl 'localhost:8090/v1/museums?place=Paris,%20France&limit=5'
```

A named place is geocoded once and remembered in the `places` table, so the
second request for a city is a local index hit rather than an upstream call —
the geocoder allows one request per second, which is unusable per-request. The
radius is taken from the extent the geocoder reports, so `place=Kyoto` searches
about 25 km and `place=Zurich` about 7 km; pass `radius_km` to override it. A
name that cannot be resolved answers 404, and that failure is cached too, so a
misspelling retried in a loop cannot exhaust the rate limit.

```json
{ "count": 5, "museums": [ ... ],
  "query": { "lat": 35.0116, "lon": 135.7681, "radius_km": 24.8, "limit": 5,
             "place": "Kyoto, Kyoto Prefecture, Japan" } }
```

The `place` field echoes what the name resolved to, so a caller can tell which
Springfield it was given.

**Browser clients.** Responses carry `Access-Control-Allow-Origin: *` and
preflight is answered, so a map application can call the API directly. The data
is public and read-only; there are no credentials involved.

**Paging.** Results carry `total` and `has_more` alongside `count`, and take an
`offset`. Without a total, a full page is indistinguishable from a complete
result set — London holds 617 museums within 50 km, and the API used to return
500 of them with nothing to say the other 117 existed.

```bash
curl 'localhost:8090/v1/museums?place=London&radius_km=50&limit=500'          # count 500, total 617, has_more true
curl 'localhost:8090/v1/museums?place=London&radius_km=50&limit=500&offset=500'  # count 117, has_more false
```

**Stable ids.** Every museum carries an `id` that survives re-crawls, and
`GET /v1/museums/{id}` fetches one by it. The id is what to deep link to and
dedupe by; `wikidata_id` cannot serve, since about 4% of the catalogue has
none. Either form works: `/v1/museums/119577` or `/v1/museums/Q19675`.

**Limits.** Every request carries a 10-second deadline, which cancels the
database query rather than letting it run on unattended. Errors are JSON at
every status, including 404 and 405.

One client may make 10 requests a second (bursting to 30) and hold 4 at once;
over either, the API answers `429` with `Retry-After`. The concurrency cap is
the one that matters, and it is not the same control as the rate: an attack
made of *slow* requests keeps the rate low while occupying every connection in
the pool. Measured with 60 concurrent expensive searches from one client — the
rate limit alone let all 60 through and an ordinary request waited 8.8 s;
with the concurrency cap, 56 are refused in milliseconds and a *different*
client is served in 0.11 s. `/livez` and `/readyz` are exempt, since
rate-limiting a liveness probe turns a busy minute into a restart.

`/health` reports counts, not just a status. An empty catalogue answers every query with nothing and no error, which is indistinguishable from "there are no museums here" unless the counts are visible:

```json
{ "status": "ok", "museums": 84584, "with_coordinates": 65896,
  "countries": 282, "exhibitions": 55, "last_updated": "2026-07-27T22:39:32Z" }
```

```bash
curl 'localhost:8090/v1/search?q=musee%20d%20orsay&limit=5'
```

Names are normalised for both indexing and querying, so a search reaches a record however either was written:

| Typed | Finds |
| --- | --- |
| `musee d orsay` | Musée d'Orsay |
| `kunstmuseum zurich` | Kunstmuseum Zürich |
| `muzeum lazienki` | Muzeum Łazienki |
| `strassenmuseum` | Straßenmuseum |
| `hal saflieni` | Ħal Saflieni |
| `kunstmus` | prefix match |

Accents are folded by Unicode decomposition; letters that decomposition leaves alone — Maltese `Ħ`, Polish `Ł`, Nordic `Ø` and `Æ`, German `ß` — are transliterated explicitly, because they have no combining mark to strip and would otherwise be unreachable from an English keyboard. Cyrillic and Greek are left as they are: transliterating them well needs language context, and a reader searching in those scripts types them.

Each result carries `locatable`, saying whether the museum also has coordinates. Around a quarter do not, and a caller plotting results on a map needs to know which.

| Parameter | Default | Notes |
| --- | --- | --- |
| `lat`, `lon` | required | Rejected outside ±90 / ±180 |
| `radius_km` | 3 | Capped at 50 — an unbounded radius would read the world |
| `limit` | 50 | Capped at 500 |
| `upcoming` | `false` | `/v1/exhibitions` only |

Responses echo the query back, so a client can tell whether its radius or limit was clamped.

Measured on the local stack with 81k museums indexed:

| Request | Time |
| --- | --- |
| `/v1/museums` (2 km radius) | **4 ms** |
| `/v1/search`, one word | **14 ms** |
| `/v1/search`, two words, fuzzy | **83 ms** |

Fuzzy matching is what costs: a two-word query pulls roughly 21,000 candidates out of the trigram indexes before scoring. That is the price of finding `guggenhiem`.

### Similarity search

Search tolerates near-misses, which is most of what people type. Before the database it did not, and half of a realistic set of twelve queries failed:

| Typed | Before | Now |
| --- | --- | --- |
| `louvre` | Louvre | Louvre |
| `louvr` | Louvre | Louvre |
| `musee dorsay` | *Air Defence Museum* | Musée d'Orsay |
| `rijkmuseum` | *nothing* | Rijksmuseum De Gevangenpoort |
| `van gough museum` | *Goughs Motor Museum* | Van Gogh Museum |
| `guggenhiem` | *nothing* | Deutsche Guggenheim |
| `metropolitain museum` | *Réunion des Musées Métropolitains* | Metropolitan Museum of Art |

**6/12 → 11/12.**

Three mechanisms combine. Exact and prefix matching on the normalised name answer a correctly typed query. Whole-string trigram similarity catches a near-miss on a short name — `rijkmuseum` shares nearly all its trigrams with `rijksmuseum`. Word similarity catches the rest: whole-string similarity compares against the *entire* name, so `guggenhiem` scored far too low against "Solomon R. Guggenheim Museum" to match at all, while `word_similarity` measures the query against the best matching run of words inside it.

Names are normalised identically going in and coming out — see `internal/search`, which folds accents and transliterates the letters decomposition leaves alone (`Ħ`, `Ł`, `Ø`, `Æ`, `ß`).

Three things about this query are load-bearing, each learned by getting it wrong:

- **Every clause in the `WHERE` is index-backed.** Matching query words with `position()` cannot use an index, and the query went from under two milliseconds to over five hundred. It is now a `BitmapOr` across four index scans.
- **Name, aliases and town live in one indexed column.** Matching them with separate `OR`'d predicates — in particular an `EXISTS` over the aliases array — gave the planner nothing to combine and forced a sequential scan of the whole table.
- **Scoring measures the name, not that combined column.** Scoring the concatenation matched almost anything: `van gough museum` reached "Vantaa City Museum".

Measured over 84,584 museums: **14 ms** for a single-word query, **83 ms** for a two-word fuzzy one, **4 ms** for a radius query.

Ranking combines four signals: how the name matches (exact, prefix, or
substring), trigram similarity blended 0.7/0.3 between whole-name and
best-extent matching, a bonus when the query names the museum's town, and
prominence from the museum's Wikidata sitelink count — how many Wikipedia
language editions cover it, which is the closest thing the sources offer to
"how well known is this".

The weights are not arbitrary. Measured on 29 realistic queries against the
live catalogue, the earliest formula scored 19/29; the current one scores 28/29.
Two mistakes accounted for most of the gap. Taking the *greater* of the two
similarity measures let the lenient one overrule the strict one, so
`kunstmuseum zurich` matched "museum zurich" inside "National Museum Zurich"
and beat "Kunsthaus Zürich". And the town bonus was a plain substring test at
weight 1.0, which both fired on letters inside unrelated words — 338 towns are
three characters, and "Sé" matched every query containing "mu-**se**-um" — and
outweighed the name entirely, so `metropolitan museum new york` returned the
New York State Museum.

The one remaining failure is a data problem rather than a scoring one, and is
worth knowing about because it affects more than search: `kunstmuseum zurich`
returns National Museum Zurich rather than the Kunsthaus, because Wikidata
records the Kunsthaus in "District 1" while the National Museum is recorded in
"Zurich" — so only the latter earns the town bonus. Localities arrive at
whatever granularity the source used ("4th arrondissement of Paris", "Gothenburg
Municipality"), and the crawler does not follow the administrative chain up to
the city. Fetching P131's parents would fix this class of problem properly.

Acronyms are matched exactly against the alternative names rather than
fuzzily: `search_text` holds the aliases, but whole-string similarity cannot
find "moma" inside a sixty-character concatenation, so the Museum of Modern Art
was not even a candidate for the query `moma` while MOMA Tainan and MOMA
Machynlleth were.

An honest limit that remains: aliases are collected in English only. Five
languages cost 60% more response bytes and twelve made the query service time
out, which during a crawl loses a whole page of a country. English is where the
acronyms live, which is the case nothing else recovers — without it `moma`
matches a gallery in Wales while the Museum of Modern Art is unreachable.

### `museum verify` — audit the catalogue

```bash
museum verify                      # summary with examples
museum verify -check duplicate-record -json
museum verify -fail-on error       # exit non-zero, for a scheduled pipeline
```

```
21510 findings across 86052 museums and 55 exhibitions — 31 errors, 21479 warnings

no-coordinates                       20134  23.38%  [warning]
unknown-country                       1256   1.46%  [warning]
duplicate-record                        47   0.05%  [warning]
coordinates-far-from-country            41   0.05%  [warning]
country-contradicts-description         29   0.03%  [error]
suspicious-name                          2   0.00%  [error]
unusable-url                             1   0.00%  [warning]
```

The pipeline reproduces what its sources say, and sources contain errors that nothing upstream will catch. Wikidata has a museum with French coordinates, a French country and an English description reading *"museum in Isfahan Province, Iran"* — correct-looking, and wrong.

| Check | Severity | Catches |
| --- | --- | --- |
| `country-contradicts-description` | error | The country field disagrees with the country the description names |
| `suspicious-name` | error | Leaked markup: `[[…]]`, `{{…}}`, HTML tags or entities |
| `impossible-coordinates` | error | Latitude or longitude outside the valid range |
| `null-island` | error | Coordinates within 1 km of 0,0 — a failed parse, not a location |
| `exhibition-ends-before-it-starts` | error | Reversed date range |
| `exhibition-title-is-boilerplate` | error | A "Find out more" button label used as a title |
| `no-coordinates` | warning | Cannot be found by any location query |
| `unknown-country` | warning | Cannot be grouped or keyed reliably |
| `duplicate-record` | warning | Two records share a name and country |
| `coordinates-far-from-country` | warning | Position far outside the spread of that country's other museums |
| `unusable-url` | warning | A website or article link that is not usable http(s) |
| `exhibition-dates-implausible` | warning | A date more than ten years away |
| `exhibition-scrape-stale` | warning | Last refreshed over a month ago |

**A finding is a signal, not a verdict.** Several checks are heuristic and will flag correct records — a museum in an overseas territory *is* far from its country's centre. They earn their place by being cheap to review, and by making a crawler regression show up as a sudden jump in one count.

Two of these checks were themselves wrong when first run against the real catalogue, and the data corrected them:

- A minimum name length and a "must contain a letter" rule flagged **M+** (Hong Kong), **W5** (Belfast) and **70.8** (Liverpool), and nothing else. Both rules were removed; only evidence of leaked markup is diagnostic.
- `country-contradicts-description` reported 238 errors, of which 203 were noise: 181 were `Czechia` versus `Czech Republic` — the same country under two spellings — and 22 were `Atlanta, Georgia` read as the Caucasus. Fixing the first meant collapsing country aliases in `pkg/geo`, which also fixed a real bug: museums in "Czechia" never merged with museums in "Czech Republic". After both fixes, 29 findings remain, and they are genuine — Crimea, and sites on the Austrian/Swiss border.

### `museum query` — the same data from a shell

```bash
museum query search      musee d orsay
museum query museums     -place "Paris, France" -radius 2
museum query exhibitions -lat 48.8566 -lon 2.3522 -radius 2 -json
```

Reads the same index the API serves, so it answers "what would the API return" without running a server.

---

## Sources

No single catalogue is complete, and none is a superset of the others.

| Source | Flag | Scale | Finds |
| --- | --- | --- | --- |
| **Wikidata** | `wikidata` | 81,372 across 293 countries | Everything typed as a museum or subclass, with coordinates, websites and Wikipedia links |
| **Wikipedia categories** | `category` | tens of thousands | Museums with an English article that Wikidata has not typed as a museum |
| **Wikipedia lists** | `lists` | ~7,000 | Museums *named* in a "List of museums in X" article but with no article of their own |
| **OpenStreetMap** | `osm` | tens of thousands | Small local museums that never reached either wiki; mapped on the ground, so nearly all have coordinates |

The first three are on by default. OSM is opt-in — much slower (one Overpass query per country) and its records are thinner.

### How records are merged

Strongest evidence first:

1. **Wikidata ID** — exact and authoritative. Both Wikipedia sources expose it via `pageprops`, and OSM often carries a `wikidata` tag, so this catches most of the overlap.
2. **Normalised name + country** — punctuation, case and spacing ignored, so `Musée d'Orsay` and `Musee d Orsay` match. *Every* name a source supplied is tried, not just the primary one: OSM names a museum in the local language while Wikidata labels it in English, and without the alternatives the two records never meet. Surviving alternatives are kept as `also_known_as`.

A name alone is never enough — without a known country the record stays separate, because "City Museum" names dozens of unrelated institutions. Coordinates are deliberately *not* used for matching: museum campuses put genuinely distinct museums metres apart.

Later sources fill gaps without overwriting established facts, with one exception: Wikidata's `country` overrides one inferred by the category crawl, which derives it from an ancestor category and so gets satellites wrong (`Centre Pompidou Hanwha` sits under a French category but stands in South Korea).

---

## Backups and durability

Postgres, MinIO and Kafka each write to a named Docker volume, so the data
survives container restarts, `docker compose down`, and image upgrades.

It does **not** survive `docker compose down -v`, `docker volume rm`, or a
Docker Desktop reset — and on macOS every volume lives inside one VM disk
image, so a single corrupted file takes all of them together. The catalogue
costs roughly four hours of rate-limited crawling to rebuild.

Take a dump before anything risky, and on a schedule if the data matters:

```bash
docker compose --profile backup run --rm backup
```

It writes a compressed custom-format dump to `./backups` on the host, outside
Docker — about 17 MB for 181,000 museums and 9,000 exhibitions. Restore with:

```bash
docker compose exec -T postgres pg_restore -U museum -d museum --clean --if-exists < backups/museum-<stamp>.dump
```

Verify a backup by restoring it into a scratch database rather than trusting
that a non-empty file is a good one:

```bash
docker compose exec -T postgres psql -U museum -d postgres -c "CREATE DATABASE restore_check"
docker compose exec -T postgres pg_restore -U museum -d restore_check --no-owner < backups/museum-<stamp>.dump
docker compose exec -T postgres psql -U museum -d restore_check -c "SELECT count(*) FROM museums"
```

**The project name is pinned to `museum`** in `docker-compose.yml`. Without
that, Compose names volumes after whichever directory it is run from, so the
same repository checked out twice — or worked on in a git worktree — mounts
two different, empty databases and the data appears to have vanished. Do not
remove the `name:` key.

Object storage is not covered by the dump. It holds the raw crawl output and
is rebuildable by re-crawling, whereas Postgres also holds the enrichment,
geocoding and scraped exhibitions that took much longer to produce. Copy the
volume directly if you want it:

```bash
docker run --rm -v museum_minio_data:/from -v $(pwd)/backups:/to alpine tar czf /to/minio.tar.gz -C /from .
```

## Storage layout

**Object storage** holds the durable record of what each source said:

| Prefix | Written by | Contents |
| --- | --- | --- |
| `raw_data/{country}/{name}.json` | `crawl` | One object per museum, as the sources described it |
| `enriched_data/{country}/{name}.json` | `enrich` | The same museum plus the resolved postal address and geocoder response |

Keys are folded to lowercase ASCII — accents dropped, `ø`/`ł`/`ß` transliterated,
punctuation turned into dashes — so `Musée de l'Armée` is stored at
`raw_data/france/musee-de-l-armee.json`. A name written entirely in a script
ASCII cannot carry falls back to a short digest (`x-3f2a...`) rather than
colliding with every other such name.

**Postgres** holds what answers queries:

| Table | Loaded by | Indexes |
| --- | --- | --- |
| `museums` | `crawl`, `reindex` | GIST on `location`, GIN trigram on the name and on name+aliases+town, prefix index for typeahead, partial index on `postcode` |
| `places` | `serve` | Geocoded place names, so `?place=Paris` costs one upstream call ever |
| `exhibitions` | `refresh` | GIST on `location`, closing date |

A museum is identified by its Wikidata id where it has one, and otherwise by its name and country — the same rule the in-process merger uses, so the two cannot disagree about what counts as the same museum. Loads upsert on that identity, so a re-crawl updates rows in place rather than accumulating copies.

### Coordinates

Radius queries run through PostGIS: `ST_DWithin` against a GIST index on a `geography` column, ordered by true distance. Museums without coordinates have a null `location` and are excluded from those queries by definition — currently **18,700 of 84,584** (22%). They are reachable only by name, which is a large part of why search exists.

### The `verified` field

`true` means the museum has its own English Wikipedia article, so the record carries a URL and usually coordinates.

`false` means a list page named it but no English article exists yet. These are still real museums — roughly three quarters of the French list is in this state, because those articles exist only on the French Wikipedia — so they are kept with their name, country and locality. Filter on `verified` if you only want museums backed by an English article.

---

## Example records

`raw_data/albania/onufri-iconographic-museum.json`:

```json
{
  "name": "Onufri Iconographic Museum",
  "country": "Albania",
  "description": "Art museum in Berat, Albania",
  "wikipedia_url": "https://en.wikipedia.org/wiki/Onufri_Iconographic_Museum",
  "wikidata_id": "Q16349422",
  "source_page": "List of museums in Albania",
  "verified": true,
  "sources": ["wikidata", "wikipedia-list"]
}
```

`enrich` adds the postal address the geocoder resolved, kept as structured
fields rather than one display string so a caller can use the parts it needs:

```json
{
  "name": "Musée d'Orsay",
  "country": "France",
  "latitude": 48.8599,
  "longitude": 2.3266,
  "address": {
    "house_number": "1",
    "road": "Rue de la Légion d'Honneur",
    "city": "Paris",
    "postcode": "75007",
    "country": "France",
    "country_code": "fr"
  }
}
```

`GET /v1/exhibitions`:

```json
{
  "count": 1,
  "exhibitions": [
    {
      "title": "Frida: The Making of an Icon",
      "url": "https://www.tate.org.uk/whats-on/tate-modern/frida-kahlo",
      "museum": "Tate Modern",
      "distance_km": 1.42,
      "end": "2027-01-03T00:00:00Z",
      "running": true,
      "scraped_at": "2026-07-27T22:25:24Z"
    }
  ],
  "query": { "lat": 51.5074, "lon": -0.1278, "radius_km": 2, "limit": 50 }
}
```

---

## Exhibitions: what this can and cannot tell you

No open catalogue carries current exhibitions. Wikidata holds **411,771** exhibition items, but a query for those with an end date in the future returns **40** worldwide — they are a historical record, not a listings feed. OpenStreetMap has no notion of a temporary exhibition at all. The only place a museum reliably publishes its programme is its own website.

Almost none publish structured data either: of five major museums sampled, one emitted any JSON-LD, and only for `WebPage`. So extraction is structural:

1. Find the programme page — links the home page offers, then conventional paths (`/whats-on`, `/exhibitions`, `/ausstellungen`, `/expositions`, …).
2. Take links that go deeper into the site on an exhibition-shaped path. A site that files exhibitions under `/exhibition/` and talks under `/event/` is trusted to mean it — and the *path segments* decide, never the slug, or the Royal Academy's `/event/summer-exhibition-friday-lates-djs` reads as an exhibition.
3. Read the title from `aria-label`, then `title`, then an inner heading, then the flattened link text. Cards wrap an image, a type label, the venue and the dates in one link, so the flattened text is a poor title. Where the link is a "Find out more" button, the title comes from the URL slug.
4. Read dates from the link and its immediate card — never a larger ancestor, or sibling cards contribute each other's dates.
5. Drop anything already closed, and anything labelled a tour, workshop, concert or lecture.

Museums sharing a website are scraped once: institutions nest, and the Musée Charles X carries `louvre.fr` as its site.

**This is heuristic and will never be complete.** JS-rendered listings yield little (one Berlin museum gave 8 of its exhibitions); bot-blocked sites yield nothing (MoMA answers `403`). Every exhibition carries the `url` it came from — treat results as leads and link out to the museum's own page rather than presenting the scrape as authoritative. Attribution is the record whose website was read: Tate publishes four galleries on one domain, so all appear under whichever Tate record was scraped.

Scraping is polite: `robots.txt` honoured, one request per host per second, bodies capped, crawler identified.

---

## Configuration

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Postgres connection string, e.g. `postgres://museum:museum@127.0.0.1:55432/museum?sslmode=disable` |
| `MINIO_ENDPOINT` | S3 endpoint — `127.0.0.1:9000` from the host, `minio:9000` in compose |
| `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` | S3 credentials |
| `MINIO_USE_SSL` | `true` or `false` |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | Credentials for the MinIO container itself |
| `MUSEUM_BUCKET_NAME` | Bucket holding every prefix above |
| `KAFKA_BROKER` | Container-to-container bootstrap (`kafka:9092`) |
| `KAFKA_BROKER_LOCAL` | Bootstrap the app uses |
| `KAFKA_TOPIC` | Topic MinIO publishes to and `enrich` reads |
| `KAFKA_GROUP_ID` | Consumer group for `enrich` |
| `NOMINATIM_USER_AGENT` | Sent to Nominatim, which rejects generic agents |
| `WIKIDATA_USER_AGENT` | Sent to the Wikidata Query Service |
| `OVERPASS_USER_AGENT` | Sent to the Overpass API |
| `EXHIBITIONS_USER_AGENT` | Sent when reading museum websites |

Every external API is called with a descriptive User-Agent, a client-side rate limit and retry-with-backoff. All four enforce this differently and all four once broke the pipeline: Nominatim returns an HTML error page for Go's default agent, Wikipedia answers `429` to unthrottled crawling, the Wikidata endpoint truncates multi-megabyte responses mid-transfer, and Overpass answers `200` with an HTML error document when overloaded.

---

## Repository layout

```
cmd/museum/            the only binary; dispatches to subcommands
internal/
  command/             one file per subcommand
  api/                 HTTP handlers, query validation, middleware, place lookup
  collect/             cross-source deduplication and merging
  geoindex/            degree-cell grid, radius cover, haversine
  enrich/              generic pipeline: parallel steps, sequential stages
  service/             Kafka events to loaded storage objects
  storage/             S3/MinIO client
  keys/                key derivation, shared so writers cannot drift
  models/              Museum, EnrichedMuseum
  env/                 configuration loading
  postgres/            schema, queries, similarity search
  quality/             catalogue audit checks
  search/              text normalisation shared by writes and queries
pkg/
  wikidata/            SPARQL client and paged museum queries
  wikipedia/           API client, wikitext/table parsing, classification
  osm/                 Overpass client
  exhibitions/         museum-website scraper
  location/            Nominatim client
  geo/                 country recognition, ISO codes
  kafkaclient/         consumer with explicit offset commits
  graceful/            SIGINT/SIGTERM context
```

---

## Development

```bash
go build ./...
go test ./...              # includes tests that hit live APIs
go test -short ./...       # offline only
go test -race -short ./...
go vet ./... && gofmt -l .
```

Offline tests in `pkg/wikipedia/extractor_structure_test.go` and `pkg/exhibitions/extract_test.go` lock the extraction rules against fixtures taken from real pages; the `live_test.go` files check the same rules against the pages they were derived from.

---

## Troubleshooting

- **`environment variable MUSEUM_BUCKET_NAME is not set`** — no `.env` in the working directory, or the variable is not exported.
- **Connection reset on port 9000** — something else on the host owns it (a local Kubernetes cluster is a common culprit). Check with `lsof -nP -iTCP:9000 -sTCP:LISTEN` and remap the published port in `docker-compose.yml`.
- **No events in Kafka UI** — check `docker logs minio-init`; it must report that the bucket notification was created. Without it MinIO stores objects but publishes nothing, and `enrich` sits idle.
- **`/v1/exhibitions` returns nothing** — almost always because `museum refresh` has not run for that area. The endpoint serves precomputed data only; the crawl finds museums, and only `refresh` reads their websites for what is on show.

  The response says which it is. Check the `coverage` object rather than guessing:

  ```json
  { "count": 0, "exhibitions": [],
    "coverage": { "museums_in_area": 28, "museums_with_website": 22,
                  "note": "no exhibitions have been collected here yet: 22 museums have a website but none has been scraped. Run \"museum refresh\" for this area." } }
  ```

  A `null` `last_scraped` means nobody has looked; a timestamp with `count: 0` means the area was scraped and nothing is currently on. Fix the first with:

  ```bash
  docker compose --profile jobs run --rm jobs refresh -place "London" -radius 10 -max-museums 200
  ```

  That takes a couple of minutes for a city — 40 London museums yielded 90 exhibitions in 1m45s.
- **Everything returns nothing, and `/health` reports zero museums** — the database is empty. Run `museum reindex` to load it from object storage. The database tests create and drop their own schema, so they cannot cause this; an earlier version truncated whatever `TEST_DATABASE_URL` pointed at and wiped the loaded catalogue.
- **Museums missing from location queries** — they have no coordinates. Check the `reindex` output for the "no coordinates" count.
- **A Wikidata country reports far fewer museums than expected** — the paging subquery must stay `SELECT DISTINCT`, and the end-of-page check must count entities rather than emitted museums. Both have caused silent truncation.
- **`overpass returned text/html instead of JSON`** — the instance is overloaded; the client already falls back across mirrors.

---

## Notes

- Country extraction is heuristic, from page and category titles. `pkg/geo` recognises UN member states plus the naming variants Wikipedia uses interchangeably.
- Object keys are slugs: lowercased, non-alphanumeric runs collapsed to dashes, accents preserved. Two museums in one country whose names slugify identically collide, and the second is skipped — measured at 1 in 3,970 on German museums (0.03%).

## License

All rights reserved or as specified by the repository owner.
