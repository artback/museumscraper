package command

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"museum/internal/collect"
	"museum/internal/models"
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
	fs := newFlagSet("crawl", "[-sources wikidata,category,lists,osm]", os.Stderr)
	sources := fs.String("sources", "wikidata,category,lists",
		"comma-separated sources: wikidata, category, lists, osm")
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

	merger := collect.NewMerger()
	collectSources(ctx, enabled, merger)

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
	loadIntoDatabase(writeCtx, museums)

	log.Printf("Finished in %s: stored %d museums in bucket %q", time.Since(start), stored, bucket)
	return nil
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
	var written int64
	for start := 0; start < len(museums); start += batchSize {
		end := min(start+batchSize, len(museums))
		n, err := db.SaveMuseums(ctx, museums[start:end])
		if err != nil {
			log.Printf("Database load stopped after %d museums: %v", written, err)
			return
		}
		written += n
	}
	log.Printf("Loaded %d museums into the database", written)
}

// collectSources runs every enabled source concurrently and feeds the merger.
// Sources are independent, so one failing or being slow does not hold up the
// others; the merger is safe for concurrent use.
func collectSources(ctx context.Context, enabled []string, merger *collect.Merger) {
	var wg sync.WaitGroup

	for _, name := range enabled {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()

			for museum := range museumsFrom(ctx, name) {
				merger.Add(museum)
			}
			log.Printf("source %q finished", name)
		}(name)
	}

	wg.Wait()
}

// museumsFrom starts the named source and returns its stream.
func museumsFrom(ctx context.Context, name string) <-chan models.Museum {
	switch name {
	case "wikidata":
		return wikidata.NewService(wikidata.NewClient()).Museums(ctx)

	case "category":
		svc := wikipedia.NewCategoryService(wikipedia.NewClient())
		return wikipedia.NewCategoryCrawler(svc).Museums(ctx, wikipedia.RootMuseumCategory)

	case "osm":
		return osm.NewService(osm.NewClient()).Museums(ctx)

	case "lists":
		svc := wikipedia.NewCategoryService(wikipedia.NewClient())
		processor := wikipedia.NewCategoryProcessor(svc, wikipedia.NewMuseumExtractor(nil))
		return processor.ProcessCategoryAsync(ctx, rootListCategory)

	default:
		closed := make(chan models.Museum)
		close(closed)
		return closed
	}
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
