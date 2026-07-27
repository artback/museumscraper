package command

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"museum/internal/keys"
	"museum/internal/models"
	"museum/internal/storage"
	"museum/pkg/exhibitions"
	"museum/pkg/graceful"
	"museum/pkg/location"
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
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("refresh", fs.Args()); err != nil {
		return err
	}

	store, bucket, err := museumStore()
	if err != nil {
		return err
	}

	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	start := time.Now()
	museums, err := selectMuseums(ctx, store, bucket, *all, *place, *lat, *lon, *radius)
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
	found := exhibitions.NewScraper().ForMuseums(ctx, museums, *concurrency)
	log.Printf("Found %d exhibitions in %s", len(found), time.Since(start).Round(time.Second))

	// Writing runs on its own context so an interrupted refresh still stores
	// what it managed to scrape, rather than discarding minutes of polite,
	// rate-limited crawling.
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancelWrite()

	db, err := database(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	written, err := db.SaveExhibitions(writeCtx, found)
	if err != nil {
		return err
	}
	log.Printf("Refresh finished in %s: %d exhibitions stored",
		time.Since(start).Round(time.Second), written)
	return nil
}

// errNoArea means the caller gave no area to refresh.
var errNoArea = errors.New("pass -all, -place, or -lat and -lon")

// selectMuseums picks the museums to scrape, either everything in the
// catalogue or the ones near a point.
func selectMuseums(ctx context.Context, store *storage.S3Service[models.Museum], bucketName string, all bool, place string, lat, lon, radius float64) ([]models.Museum, error) {
	if all {
		return allMuseumsWithWebsites(ctx, store, bucketName)
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
func allMuseumsWithWebsites(ctx context.Context, store *storage.S3Service[models.Museum], bucketName string) ([]models.Museum, error) {
	var (
		mu      sync.Mutex
		museums []models.Museum
	)

	err := store.EachObject(ctx, bucketName, keys.RawPrefix+"/", func(_ string, museum models.Museum) {
		if museum.Website == "" || !museum.HasCoordinates() {
			return
		}
		mu.Lock()
		museums = append(museums, museum)
		mu.Unlock()
	})
	return museums, err
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
	hits, err := db.Nearby(ctx, lat, lon, radius, 5000)
	if err != nil {
		return nil, err
	}

	museums := make([]models.Museum, 0, len(hits))
	for _, hit := range hits {
		if strings.TrimSpace(hit.Museum.Website) != "" {
			museums = append(museums, hit.Museum)
		}
	}
	return museums, nil
}
