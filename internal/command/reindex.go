package command

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"museum/internal/keys"
	"museum/internal/models"
	"museum/internal/storage"
	"museum/pkg/graceful"
)

// reindexCommand loads the stored catalogue into the database.
func reindexCommand() Command {
	return Command{
		Name:    "reindex",
		Summary: "Load the stored catalogue into the database and refresh its indexes",
		Usage:   "[-batch 2000]",
		Run:     runReindex,
	}
}

func runReindex(ctx context.Context, args []string) error {
	fs := newFlagSet("reindex", "[-batch 2000]", os.Stderr)
	batchSize := fs.Int("batch", 2000, "museums per database round trip")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("reindex", fs.Args()); err != nil {
		return err
	}
	if *batchSize < 1 {
		return errors.New("batch must be at least 1")
	}

	store, bucket, err := museumStore()
	if err != nil {
		return err
	}
	db, err := database(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	start := time.Now()
	log.Printf("Reading the catalogue from %s/%s ...", bucket, keys.RawPrefix)

	catalogue, err := readCatalogue(ctx, store, bucket)
	if err != nil {
		return err
	}
	log.Printf("Read %d museums in %s (%d enriched, %d have no coordinates)",
		catalogue.read, time.Since(start).Round(time.Second), catalogue.enriched, catalogue.unplaced)

	if len(catalogue.everything) == 0 {
		return errors.New("nothing to load")
	}

	// Every museum is loaded, including the ones with no position: they are
	// unreachable by radius query but findable by name, and dropping them here
	// would make a quarter of the catalogue invisible.
	var written int64
	for start := 0; start < len(catalogue.everything); start += *batchSize {
		if ctx.Err() != nil {
			return errors.New("interrupted; the load is incomplete, run again")
		}
		end := min(start+*batchSize, len(catalogue.everything))

		n, err := db.SaveMuseums(ctx, catalogue.everything[start:end])
		if err != nil {
			return err
		}
		written += n
	}

	removed, err := db.MergeDuplicates(ctx)
	if err != nil {
		return err
	}
	if removed > 0 {
		log.Printf("Merged %d duplicate records", removed)
	}

	counts, err := db.Counts(ctx)
	if err != nil {
		return err
	}
	log.Printf("Loaded %d museums in %s: %d in the database, %d with coordinates, %d countries",
		written, time.Since(start).Round(time.Second),
		counts.Museums, counts.WithCoordinates, counts.Countries)
	return nil
}

// catalogueRead is the outcome of assembling the canonical catalogue.
type catalogueRead struct {
	// museums are the ones that can be indexed, i.e. those with coordinates.
	museums []models.Museum
	// everything is every record read, including those with no position. The
	// audit needs these: a museum that cannot be placed is a finding, and
	// checking only the indexed ones would hide the problem being reported.
	everything []models.Museum

	read     int
	enriched int
	unplaced int
}

// readCatalogue assembles the records the index is built from.
//
// The enriched copy of a museum wins over the raw one. The enrichment pipeline
// adds a postal address, an official website and — where the geocoder can place
// a museum the sources could not — coordinates, and building the index from raw
// records alone threw all of that away.
func readCatalogue(ctx context.Context, store museumStorage, bucket string) (catalogueRead, error) {
	var (
		mu       sync.Mutex
		byKey    = map[string]models.Museum{}
		enriched = map[string]bool{}
		result   catalogueRead
	)

	err := store.EachObject(ctx, bucket, keys.RawPrefix+"/", func(key string, museum models.Museum) {
		mu.Lock()
		defer mu.Unlock()
		result.read++
		byKey[keys.Museum(museum)] = museum
	})
	if err != nil {
		return result, err
	}

	// Enriched records are keyed by the same country/name slug, so they line up
	// with the raw ones they supersede.
	err = eachEnriched(ctx, store, bucket, func(e models.EnrichedMuseum) {
		mu.Lock()
		defer mu.Unlock()

		merged := mergeEnriched(e)
		key := keys.Museum(merged)
		if _, ok := byKey[key]; !ok {
			result.read++
		}
		byKey[key] = merged
		enriched[key] = true
	})
	if err != nil {
		log.Printf("Could not read enriched records, indexing raw only: %v", err)
	}

	for key, museum := range byKey {
		if enriched[key] {
			result.enriched++
		}
		result.everything = append(result.everything, museum)
		if !museum.HasCoordinates() {
			result.unplaced++
			continue
		}
		result.museums = append(result.museums, museum)
	}
	return result, nil
}

// museumStorage is the reading surface reindex needs, so the assembly logic can
// be exercised without object storage.
type museumStorage interface {
	EachObject(ctx context.Context, bucket, prefix string, fn func(key string, m models.Museum)) error
}

// eachEnriched walks the enriched records. It opens its own client because the
// generic store is typed to one record shape, and enriched records are a
// different one.
func eachEnriched(ctx context.Context, _ museumStorage, bucket string, fn func(models.EnrichedMuseum)) error {
	store, err := storage.NewS3Service(keys.EnrichedMuseum)
	if err != nil {
		return err
	}
	return store.EachObject(ctx, bucket, keys.EnrichedPrefix+"/", func(_ string, e models.EnrichedMuseum) {
		fn(e)
	})
}

// mergeEnriched folds the enrichment results back onto the museum record.
//
// The enrichment pipeline flattens Nominatim's answer into an untyped map, so
// the fields worth promoting are lifted out by name. Only gaps are filled: a
// coordinate stated by Wikidata is better evidence than one inferred from a
// name search, so it is never overwritten.
func mergeEnriched(e models.EnrichedMuseum) models.Museum {
	museum := e.Museum

	if !museum.HasCoordinates() {
		if lat, ok := floatFrom(e.Data["lat"]); ok {
			if lon, ok := floatFrom(e.Data["lon"]); ok {
				museum.Latitude, museum.Longitude = lat, lon
			}
		}
	}
	if museum.Website == "" {
		if website, ok := e.Data["website"].(string); ok {
			museum.Website = website
		}
	}
	if museum.Locality == "" {
		if locality, ok := e.Data["locality"].(string); ok {
			museum.Locality = locality
		}
	}
	return museum
}

// floatFrom reads a number that has been through JSON, where Nominatim's
// coordinates arrive as strings and everything numeric arrives as float64.
func floatFrom(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
