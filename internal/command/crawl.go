package command

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"museum/internal/collect"
	"museum/internal/models"
	"museum/internal/postgres"
	"museum/pkg/graceful"
	"museum/pkg/osm"
	"museum/pkg/wikidata"
	"museum/pkg/wikipedia"
)

// rootListCategory is the top of the category tree holding the list articles.
const rootListCategory = "Category:Lists_of_museums_by_country"

// writeTimeout bounds the persistence phase, which runs even after an
// interrupt so a cancelled crawl still stores what it collected.
const writeTimeout = 30 * time.Minute

// crawlCommand builds the catalogue from the configured sources.
func crawlCommand() Command {
	return Command{
		Name:    "crawl",
		Summary: "Build the catalogue from Wikidata, Wikipedia and OpenStreetMap",
		Usage:   "[-sources wikidata,category,lists,osm]",
		Run:     runCrawl,
	}
}

func runCrawl(ctx context.Context, args []string) error {
	fs := newFlagSet("crawl", "[-sources wikidata,category,lists,osm] [-languages en,es,…]", os.Stderr)
	sources := fs.String("sources", "wikidata,category,lists",
		"comma-separated sources: wikidata, category, lists, osm")
	// English only by default. Every extra edition is a full category walk and
	// roughly doubles the crawl's Wikipedia traffic, so widening coverage is a
	// decision to make deliberately rather than something a routine crawl does
	// by accident. "all" is the shorthand for every edition known.
	languages := fs.String("languages", wikipedia.DefaultLanguage,
		"comma-separated Wikipedia editions for the category source, or \"all\"")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("crawl", fs.Args()); err != nil {
		return err
	}

	enabled := parseSources(*sources)
	if len(enabled) == 0 {
		return fmt.Errorf("no valid sources selected in %q", *sources)
	}

	editions := parseLanguages(*languages)
	if len(editions) == 0 {
		return fmt.Errorf("no known Wikipedia editions selected in %q", *languages)
	}

	store, bucket, err := museumStore()
	if err != nil {
		return err
	}

	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	if err := store.EnsureBucket(ctx, bucket, ""); err != nil {
		return err
	}

	start := time.Now()
	log.Printf("Crawling sources: %s", strings.Join(enabled, ", "))
	if contains(enabled, "category") {
		log.Printf("Wikipedia editions: %s", strings.Join(editions, ", "))
	}

	// The database is opened for the whole crawl so records can be made durable
	// while they are still being collected. A crawl that cannot reach it still
	// runs — object storage is the durable copy and "museum reindex" loads it
	// afterwards — but it forfeits checkpointing.
	saver := newCheckpointer(ctx)
	defer saver.close()

	merger := collect.NewMerger()
	collectSources(ctx, enabled, editions, merger, saver.add)
	saver.flush()

	distinct, folded := merger.Stats()
	log.Printf("Collected %d distinct museums (%d records merged across sources) in %s",
		distinct, folded, time.Since(start))

	museums := merger.Museums()

	// Persisting runs on its own context. Ctrl-C during a crawl means "stop
	// collecting", not "throw away the last hour" — reusing the cancelled
	// context here would abandon everything the sources had already returned.
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancelWrite()

	stored := store.StoreFromChannel(writeCtx, bucket, stream(writeCtx, museums))

	// Written again at the end, and deliberately: the checkpoints hold each
	// record as its own source saw it, while this holds the merged form, with
	// aliases and sources unioned across sources and the best coordinates kept.
	// The upsert is idempotent, so this refines what is already stored rather
	// than duplicating it — and if it fails, the checkpointed records remain.
	loadIntoDatabase(writeCtx, museums)

	log.Printf("Finished in %s: stored %d museums in bucket %q", time.Since(start), stored, bucket)
	return nil
}

// How much work a crash may cost, bounded two ways.
//
// A count alone is not enough. OpenStreetMap is queried one country at a time
// and produced 219 museums in eleven minutes, so a batch of 500 would have left
// every one of them unwritten for the whole of that — precisely the exposure
// checkpointing exists to remove. Whichever limit is reached first triggers the
// write, so a fast source checkpoints on volume and a slow one on time.
const (
	checkpointBatch    = 500
	checkpointInterval = 30 * time.Second
)

// checkpointer makes collected museums durable while the crawl is still
// running.
//
// Without it a crawl held every record in memory for an hour and a half and
// wrote them only at the very end, so any failure before that point — a crash,
// a restart, a lost connection, the machine losing power — cost the entire run.
// The same shape lost 9,148 scraped exhibitions to one malformed title.
//
// Records are written as each source produces them, before merging. The upsert
// keys on identity and unions what it finds, so writing a museum early and
// again later, enriched, converges on the same row rather than duplicating it.
type checkpointer struct {
	db  *postgres.Store
	ctx context.Context

	mu      sync.Mutex
	pending []models.Museum
	written int64
	lost    int

	// stop ends the periodic flush; done reports that it has ended, so close
	// cannot return while a write is still in flight.
	stop chan struct{}
	done chan struct{}
}

// newCheckpointer connects for the duration of the crawl. A checkpointer with
// no database still accepts records and simply keeps none of them, so the
// caller needs no special case.
func newCheckpointer(ctx context.Context) *checkpointer {
	// Checkpoints run on a context that outlives cancellation, for the same
	// reason the final write does: interrupting a crawl means "stop
	// collecting", not "discard what was collected".
	writeCtx := context.WithoutCancel(ctx)

	db, err := database(writeCtx)
	if err != nil {
		log.Printf("Checkpointing disabled: %v (the crawl will still store to object storage)", err)
		return &checkpointer{ctx: writeCtx}
	}

	c := &checkpointer{
		db:   db,
		ctx:  writeCtx,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	go c.flushPeriodically()
	return c
}

// flushPeriodically writes whatever has accumulated, so a trickle of records
// from a slow source is never left exposed for longer than the interval.
func (c *checkpointer) flushPeriodically() {
	defer close(c.done)

	ticker := time.NewTicker(checkpointInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.save(c.take())
		case <-c.stop:
			return
		}
	}
}

// take removes everything pending and returns it.
func (c *checkpointer) take() []models.Museum {
	c.mu.Lock()
	defer c.mu.Unlock()

	batch := c.pending
	c.pending = nil
	return batch
}

// add takes one museum, writing a batch once enough have accumulated. It is
// safe to call from every source goroutine at once.
func (c *checkpointer) add(m models.Museum) {
	if c.db == nil {
		return
	}

	c.mu.Lock()
	c.pending = append(c.pending, m)
	full := len(c.pending) >= checkpointBatch
	var batch []models.Museum
	if full {
		batch, c.pending = c.pending, nil
	}
	c.mu.Unlock()

	if full {
		c.save(batch)
	}
}

// flush writes whatever has not yet reached a full batch.
func (c *checkpointer) flush() {
	if c.db == nil {
		return
	}

	c.save(c.take())

	if c.written > 0 {
		log.Printf("Checkpointed %d museums during collection", c.written)
	}
	if c.lost > 0 {
		log.Printf("%d museums could not be checkpointed (the final write will retry them)", c.lost)
	}
}

// save persists one batch. A failure costs that batch and nothing else: the
// crawl continues, and the write at the end covers what was missed.
func (c *checkpointer) save(batch []models.Museum) {
	if len(batch) == 0 {
		return
	}

	n, err := c.db.SaveMuseums(c.ctx, batch)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		log.Printf("Checkpoint failed for %d museums: %v", len(batch), err)
		c.lost += len(batch)
		return
	}
	c.written += n
}

// close ends the periodic flush and waits for it, so no write is still running
// when the pool is torn down.
func (c *checkpointer) close() {
	if c.db == nil {
		return
	}
	close(c.stop)
	<-c.done
	c.db.Close()
}

// loadIntoDatabase writes the crawl straight into the serving store, so a fresh
// crawl is queryable without a separate step.
//
// A failure here is logged rather than returned: the records are already in
// object storage, which is the durable copy, and "museum reindex" can load them
// afterwards. Losing a crawl because the database was briefly unreachable would
// be a poor trade.
func loadIntoDatabase(ctx context.Context, museums []models.Museum) {
	db, err := database(ctx)
	if err != nil {
		log.Printf("Skipping the database load: %v (run \"museum reindex\" once it is reachable)", err)
		return
	}
	defer db.Close()

	const batchSize = 2000

	var (
		written int64
		lost    int
	)
	for start := 0; start < len(museums); start += batchSize {
		end := min(start+batchSize, len(museums))

		n, err := db.SaveMuseums(ctx, museums[start:end])
		if err != nil {
			// One failing batch is not a reason to abandon the rest. This used
			// to return, so a single transient error part way through discarded
			// every museum after it — tens of thousands of records thrown away
			// because one batch of two thousand did not land.
			log.Printf("Batch of %d museums failed: %v (continuing)", end-start, err)
			lost += end - start
			continue
		}
		written += n
	}

	log.Printf("Loaded %d museums into the database", written)
	if lost > 0 {
		log.Printf("%d museums were not loaded; they remain in object storage — run \"museum reindex\"", lost)
	}

	// A crawl is where duplicates are created, so it is where they are cleaned
	// up. Records arriving with a Wikidata id for a museum already stored under
	// its name are promoted in place by the upsert; the ones whose id was
	// already claimed are folded together here.
	removed, err := db.MergeDuplicates(ctx)
	if err != nil {
		log.Printf("Duplicate merge failed: %v (records are intact; rerun \"museum reindex\")", err)
		return
	}
	if removed > 0 {
		log.Printf("Merged %d duplicate records", removed)
	}

	// The same museum under two names, which the identity key cannot catch
	// because the names differ. Run after the id-based merge so it works on
	// what that leaves behind.
	variants, err := db.MergeNameVariants(ctx)
	if err != nil {
		log.Printf("Name-variant merge failed: %v (records are intact)", err)
		return
	}
	if variants > 0 {
		log.Printf("Merged %d museums recorded under more than one name", variants)
	}

	// The same museum stored under its name in two languages, which neither of
	// the merges above can see: they compare names, and a translation shares no
	// words with its original. Run last, so it works on a catalogue the other
	// two have already tidied.
	aliases, err := db.MergeAliasVariants(ctx)
	if err != nil {
		log.Printf("Alias-variant merge failed: %v (records are intact)", err)
		return
	}
	if aliases > 0 {
		log.Printf("Merged %d museums recorded under a name another row already knew", aliases)
	}
}

// collectSources runs every enabled source concurrently and feeds the merger.
// Sources are independent, so one failing or being slow does not hold up the
// others; the merger is safe for concurrent use.
//
// Each museum is also handed to onMuseum as it arrives, so the caller can make
// it durable without waiting for every source to finish.
//
// The category source runs once per language edition. Those crawls are the only
// way to reach the museums English Wikipedia has no article for, which is most
// of them: 35,352 against 19,802.
func collectSources(ctx context.Context, enabled, languages []string, merger *collect.Merger, onMuseum func(models.Museum)) {
	var wg sync.WaitGroup

	// One Wikipedia client for every source that needs one, so they share both
	// the rate limiter and the connection pool.
	wiki := wikipedia.NewClient()

	run := func(label string, stream func() <-chan models.Museum) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for museum := range stream() {
				merger.Add(museum)
				onMuseum(museum)
			}
			log.Printf("source %q finished", label)
		}()
	}

	for _, name := range enabled {
		if name != "category" {
			run(name, func() <-chan models.Museum { return museumsFrom(ctx, name, wiki) })
			continue
		}
		for _, lang := range languages {
			root, ok := wikipedia.RootCategoryFor(lang)
			if !ok {
				log.Printf("no museum category root known for %q, skipping that edition", lang)
				continue
			}
			run("category:"+lang, func() <-chan models.Museum {
				svc := wikipedia.NewCategoryService(wikipedia.NewLanguageClient(lang))
				stream := wikipedia.NewCategoryCrawler(svc).Museums(ctx, root)
				if lang == wikipedia.DefaultLanguage {
					return stream
				}
				return englishNormalized(ctx, stream)
			})
		}
	}

	wg.Wait()
}

// normalizeBatch is how many records are held back to be normalised together.
// Each batch costs one Wikidata request, so the size trades round trips against
// how long a record waits before it can be checkpointed.
const normalizeBatch = 200

// englishNormalized restates records from a non-English edition in the terms the
// rest of the catalogue uses.
//
// A Spanish crawl finds real museums, but it describes them in Spanish and files
// them under "Categoría:Museos de Alemania", so the country arrives as
// "Alemania". Nothing downstream can work with that: the country is a grouping
// key, the duplicate merger compares it, and the geographic checks look it up in
// an English gazetteer. The same museum found by the English crawl and by the
// Spanish one would not even be recognised as one record.
//
// Every article carries a Wikidata id, and Wikidata states the country as an
// entity rather than a word, so resolving through it produces exactly the shape
// the English crawl produces — without translating anything, and without a
// second gazetteer per language.
//
// The museum's own name is not replaced unless Wikidata has an English label for
// it, and for a museum that label is almost always the real name rather than a
// translation. The edition's own title is kept as an alias either way, because
// it is the name the museum is known by locally and losing it would make the
// record unfindable to anyone searching in that language.
//
// A record with no Wikidata id, or a lookup that fails, passes through unchanged
// rather than being dropped. It is then a record in Spanish, which is worse than
// one in English and far better than none.
func englishNormalized(ctx context.Context, in <-chan models.Museum) <-chan models.Museum {
	out := make(chan models.Museum)

	go func() {
		defer close(out)

		svc := wikidata.NewService(wikidata.NewClient())
		batch := make([]models.Museum, 0, normalizeBatch)

		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			ids := make([]string, 0, len(batch))
			for _, m := range batch {
				if m.WikidataID != "" {
					ids = append(ids, m.WikidataID)
				}
			}
			details, err := svc.Details(ctx, ids)
			if err != nil {
				log.Printf("normalising to English failed for %d records: %v", len(batch), err)
			}
			for _, m := range batch {
				select {
				case out <- applyDetails(m, details[m.WikidataID]):
				case <-ctx.Done():
					return false
				}
			}
			batch = batch[:0]
			return true
		}

		for museum := range in {
			batch = append(batch, museum)
			if len(batch) >= normalizeBatch && !flush() {
				return
			}
		}
		flush()
	}()

	return out
}

// applyDetails restates one record in English, keeping whatever the edition
// supplied where Wikidata has nothing better.
func applyDetails(m models.Museum, d wikidata.Details) models.Museum {
	if d.Label != "" && d.Label != m.Name {
		// The local name becomes an alias rather than being discarded: it is
		// how the museum is known where it stands, and the merger and the
		// search index both use aliases.
		if !slices.Contains(m.AlsoKnownAs, m.Name) {
			m.AlsoKnownAs = append(m.AlsoKnownAs, m.Name)
		}
		m.Name = d.Label
	}
	// Country is replaced, not gap-filled: the value already there was inferred
	// from a category title in another language, and that is the thing being
	// corrected.
	if d.Country != "" {
		m.Country = d.Country
	}
	if d.Description != "" {
		m.Description = d.Description
	}
	if m.Locality == "" {
		m.Locality = d.Locality
	}
	if len(m.Classes) == 0 {
		m.Classes = d.Classes
	}
	if d.Sitelinks > m.Sitelinks {
		m.Sitelinks = d.Sitelinks
	}
	return m
}

// museumsFrom starts the named source and returns its stream.
//
// The two Wikipedia-backed sources share one client. They used to build their
// own, and although each spaced its requests correctly, together they ran at
// twice the rate the API tolerates: both were throttled, and the lists crawl
// was cut down to 14 candidates with the United States and England skipped
// outright. The client's limiter is process-wide now, so a shared client is
// belt and braces rather than the fix itself — but it also shares connections,
// which is what a single logical crawler should do.
func museumsFrom(ctx context.Context, name string, wiki *wikipedia.Client) <-chan models.Museum {
	switch name {
	case "wikidata":
		return wikidata.NewService(wikidata.NewClient()).Museums(ctx)

	case "category":
		svc := wikipedia.NewCategoryService(wiki)
		return wikipedia.NewCategoryCrawler(svc).Museums(ctx, wikipedia.RootMuseumCategory)

	case "osm":
		return osm.NewService(osm.NewClient()).Museums(ctx)

	case "lists":
		svc := wikipedia.NewCategoryService(wiki)
		processor := wikipedia.NewCategoryProcessor(svc, wikipedia.NewMuseumExtractor(nil))
		return processor.ProcessCategoryAsync(ctx, rootListCategory)

	default:
		closed := make(chan models.Museum)
		close(closed)
		return closed
	}
}

// parseLanguages validates and de-duplicates the -languages flag, keeping only
// editions the category crawl knows a root category for.
func parseLanguages(raw string) []string {
	if strings.EqualFold(strings.TrimSpace(raw), "all") {
		return wikipedia.Languages()
	}

	var editions []string
	for _, lang := range strings.Split(raw, ",") {
		lang = strings.ToLower(strings.TrimSpace(lang))
		switch {
		case lang == "":
		case contains(editions, lang):
		default:
			if _, known := wikipedia.RootCategoryFor(lang); !known {
				log.Printf("ignoring unknown Wikipedia edition %q", lang)
				continue
			}
			editions = append(editions, lang)
		}
	}
	return editions
}

// parseSources validates and de-duplicates the -sources flag.
func parseSources(raw string) []string {
	known := map[string]bool{"wikidata": true, "category": true, "lists": true, "osm": true}

	var enabled []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		switch {
		case name == "":
		case !known[name]:
			log.Printf("ignoring unknown source %q", name)
		case contains(enabled, name):
		default:
			enabled = append(enabled, name)
		}
	}
	return enabled
}

func contains(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

// stream turns the merged slice back into a channel for the storage layer,
// stopping early if the run is cancelled.
func stream(ctx context.Context, museums []models.Museum) <-chan models.Museum {
	out := make(chan models.Museum)

	go func() {
		defer close(out)
		for _, museum := range museums {
			select {
			case out <- museum:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out
}
