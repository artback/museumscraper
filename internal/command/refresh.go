package command

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"museum/internal/harvest"
	"museum/internal/models"
	"museum/pkg/exhibitions"
	"museum/pkg/graceful"
	"museum/pkg/location"
)

const (
	// checkpointSize is how many exhibitions accumulate before being written.
	// Small enough that a crash costs seconds of scraping, large enough that
	// the writes stay batched.
	checkpointSize = 200

	// checkpointTimeout bounds one checkpoint's write.
	checkpointTimeout = 2 * time.Minute
)

// refreshCommand scrapes museum websites and indexes what is on show.
func refreshCommand() Command {
	return Command{
		Name:    "refresh",
		Summary: "Scrape museum websites for current exhibitions and index them",
		Usage:   "(-all | -place NAME | -lat N -lon N) [-radius 5]",
		Run:     runRefresh,
	}
}

func runRefresh(ctx context.Context, args []string) error {
	fs := newFlagSet("refresh", "(-all | -place NAME | -lat N -lon N) [-radius 5]", os.Stderr)
	var (
		place       = fs.String("place", "", "refresh museums around this place")
		lat         = fs.Float64("lat", 0, "latitude of the area to refresh")
		lon         = fs.Float64("lon", 0, "longitude of the area to refresh")
		radius      = fs.Float64("radius", 5, "radius in kilometres")
		all         = fs.Bool("all", false, "refresh every museum with a website, worldwide")
		maxMuseums  = fs.Int("max-museums", 500, "cap on museums to scrape (0 for no limit)")
		concurrency = fs.Int("concurrency", 8, "how many museum sites to read at once")

		// Off by default, and worth keeping that way. The heuristic scraper
		// reads thousands of sites for nothing; this reads the ones it could
		// not, and pays a model invocation of minutes the first time it meets
		// each of them.
		fallback    = fs.Bool("fallback", false, "for sites the scraper cannot read, use a generated extractor")
		maxCompiles = fs.Int("max-new-extractors", harvest.DefaultMaxCompiles,
			"how many new extractors one run may generate")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("refresh", fs.Args()); err != nil {
		return err
	}

	// Object storage is no longer consulted here: the catalogue in Postgres is
	// what says which museums have a website worth reading.
	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	start := time.Now()
	museums, err := selectMuseums(ctx, *all, *place, *lat, *lon, *radius, *maxMuseums)
	if err != nil {
		return err
	}
	if *maxMuseums > 0 && len(museums) > *maxMuseums {
		log.Printf("Limiting to %d of %d museums (raise -max-museums for more)", *maxMuseums, len(museums))
		museums = museums[:*maxMuseums]
	}
	if len(museums) == 0 {
		return errors.New("no museums with a website to refresh")
	}

	log.Printf("Scraping %d museum websites...", len(museums))
	db, err := database(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	// Exhibitions are written as they are scraped, in checkpoints, rather than
	// once at the end.
	//
	// The single write at the end made an hour's work all-or-nothing: a scrape
	// of 6,000 sites found 9,148 exhibitions and stored none, because the one
	// insert hit a mis-encoded title and the whole batch was refused. A crash
	// or a restart at minute 67 would have cost the same. Checkpointing bounds
	// the loss to whatever has been found since the last one.
	var (
		buffer  []exhibitions.Exhibition
		written int64
		found   int
		failed  int
	)

	flush := func() {
		if len(buffer) == 0 {
			return
		}
		// Each checkpoint gets a context that outlives cancellation: a Ctrl-C
		// during a scrape should still commit what has already been found.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkpointTimeout)
		defer cancel()

		stored, err := db.SaveExhibitions(writeCtx, buffer)
		if err != nil {
			// A failed checkpoint costs that checkpoint, not the run. The
			// scrape continues, and the log says how much was lost.
			log.Printf("Checkpoint failed, %d exhibitions lost: %v", len(buffer), err)
			failed += len(buffer)
		}
		written += stored
		buffer = buffer[:0]
	}

	scraper := exhibitions.NewScraper()
	if *fallback {
		generated, err := exhibitionFallback(ctx, *maxCompiles)
		if err != nil {
			// A misconfigured fallback must not cost the whole refresh. What
			// the heuristics can read is the great majority of the catalogue,
			// and it is still worth reading.
			log.Printf("Generated-extractor fallback unavailable, continuing without it: %v", err)
		} else {
			scraper.Fallback = generated
			log.Printf("Generated-extractor fallback enabled, up to %d new extractors this run", *maxCompiles)
		}
	}

	scraper.Stream(ctx, museums, *concurrency, func(batch []exhibitions.Exhibition) {
		found += len(batch)
		buffer = append(buffer, batch...)
		if len(buffer) >= checkpointSize {
			flush()
			log.Printf("  ... %d exhibitions stored so far (%s elapsed)",
				written, time.Since(start).Round(time.Second))
		}
	})
	flush()

	// Occurrences of one recurring event arrive from different listing pages
	// and different runs, so folding them together belongs here as well as in
	// the scraper.
	mergeCtx, cancelMerge := context.WithTimeout(context.WithoutCancel(ctx), checkpointTimeout)
	defer cancelMerge()

	if pruned, err := db.PruneNavigationListings(mergeCtx); err != nil {
		log.Printf("Could not prune navigation links: %v", err)
	} else if pruned > 0 {
		log.Printf("Removed %d listing navigation links stored as exhibitions", pruned)
	}

	if merged, err := db.MergeDuplicateExhibitions(mergeCtx); err != nil {
		log.Printf("Could not merge repeated listings: %v", err)
	} else if merged > 0 {
		log.Printf("Merged %d repeated listings of the same event", merged)
	}

	log.Printf("Refresh finished in %s: found %d exhibitions, stored %d, lost %d",
		time.Since(start).Round(time.Second), found, written, failed)
	return nil
}

// errNoArea means the caller gave no area to refresh.
var errNoArea = errors.New("pass -all, -place, or -lat and -lon")

// selectMuseums picks the museums to scrape, either everything in the
// catalogue or the ones near a point.
func selectMuseums(ctx context.Context, all bool, place string, lat, lon, radius float64, maxMuseums int) ([]models.Museum, error) {
	if all {
		return allMuseumsWithWebsites(ctx, maxMuseums)
	}

	if place != "" {
		loc, err := location.Geocode(ctx, place)
		if err != nil {
			return nil, err
		}
		var placeLat, placeLon float64
		if _, err := fmt.Sscanf(loc.Lat, "%f", &placeLat); err != nil {
			return nil, fmt.Errorf("bad latitude %q for %q: %w", loc.Lat, place, err)
		}
		if _, err := fmt.Sscanf(loc.Lon, "%f", &placeLon); err != nil {
			return nil, fmt.Errorf("bad longitude %q for %q: %w", loc.Lon, place, err)
		}
		lat, lon = placeLat, placeLon
		log.Printf("Refreshing around %s", loc.DisplayName)
	}
	if lat == 0 && lon == 0 {
		return nil, errNoArea
	}
	return nearbyMuseumsWithWebsites(ctx, lat, lon, radius)
}

// allMuseumsWithWebsites reads the whole catalogue, keeping the museums that
// have both a website to scrape and a position to index the results under.
func allMuseumsWithWebsites(ctx context.Context, limit int) ([]models.Museum, error) {
	db, err := database(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// Read from the serving store rather than by listing object storage.
	// Listing 96,000 objects to find the ones with a website was slow, and it
	// failed outright on a transient storage error mid-run. Postgres already
	// holds the merged, deduplicated catalogue with the websites indexed, and
	// it can order by prominence so a capped run scrapes the museums most
	// likely to publish listings rather than an arbitrary few thousand.
	return db.MuseumsWithWebsites(ctx, limit)
}

// nearbyMuseumsWithWebsites asks the database for the museums around a point
// that have a site to read.
func nearbyMuseumsWithWebsites(ctx context.Context, lat, lon, radius float64) ([]models.Museum, error) {
	db, err := database(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// The scraper reads at most a few hundred sites per run, so a generous cap
	// here is still far more than a refresh will use.
	page, err := db.Nearby(ctx, lat, lon, radius, 5000, 0)
	if err != nil {
		return nil, err
	}
	hits := page.Hits

	museums := make([]models.Museum, 0, len(hits))
	for _, hit := range hits {
		if strings.TrimSpace(hit.Museum.Website) != "" {
			museums = append(museums, hit.Museum)
		}
	}
	return museums, nil
}
