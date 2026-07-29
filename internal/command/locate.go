package command

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"museum/internal/models"
	"museum/internal/search"
	"museum/pkg/graceful"
	"museum/pkg/location"
)

// locateCommand geocodes museums the catalogue holds but cannot place.
func locateCommand() Command {
	return Command{
		Name:    "locate",
		Summary: "Geocode museums that have no coordinates",
		Usage:   "[-locality NAME] [-country NAME] [-limit N] [-dry-run]",
		Run:     runLocate,
	}
}

// runLocate resolves coordinates for stored museums that have none.
//
// A fifth of the catalogue has no position, and those records are invisible to
// every radius and place query — the crawl sources simply do not always carry
// coordinates. Enrichment already geocodes, but only for museums arriving
// through the event pipeline, so a record that was loaded without a position
// stayed that way with no means of repair.
func runLocate(ctx context.Context, args []string) error {
	fs := newFlagSet("locate", "[-locality NAME] [-country NAME] [-limit N] [-dry-run]", os.Stderr)
	var (
		locality = fs.String("locality", "", "only museums whose town matches this")
		country  = fs.String("country", "", "only museums in this country")
		limit    = fs.Int("limit", 200, "how many museums to attempt")
		dryRun   = fs.Bool("dry-run", false, "report what would be geocoded, without calling the geocoder")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("locate", fs.Args()); err != nil {
		return err
	}
	if *limit < 1 {
		return errors.New("limit must be a positive whole number")
	}

	db, err := database(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	pending, err := db.UnplacedMuseums(ctx, *locality, *country, *limit)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		log.Println("Nothing to locate: every matching museum already has coordinates")
		return nil
	}

	// The geocoder allows one request per second, so the run time is knowable
	// in advance and worth stating: a caller asking for thousands should know
	// it is an hours-long job before it starts rather than after.
	log.Printf("Locating %d museums (about %s at the geocoder's one request per second)",
		len(pending), (time.Duration(len(pending)) * 1100 * time.Millisecond).Round(time.Second))

	if *dryRun {
		for _, m := range pending {
			log.Printf("  would geocode %q (%s)", m.Name, placeOf(m.Locality, m.Country))
		}
		return nil
	}

	var located, approximated, unresolved int
	start := time.Now()

	for _, m := range pending {
		if ctx.Err() != nil {
			log.Printf("Interrupted: located %d of %d", located, len(pending))
			return nil
		}

		// The same query builder the enrichment pipeline uses, so a museum
		// geocoded here and one geocoded there are asked for identically.
		query := geocodeQuery(&models.Museum{Name: m.Name, Locality: m.Locality, Country: m.Country})

		var (
			lat, lon    float64
			approximate bool
		)

		found, err := location.Geocode(ctx, query)
		switch {
		case err == nil:
			if lat, lon, err = found.Coordinates(); err != nil {
				log.Printf("  %q: %v", m.Name, err)
				unresolved++
				continue
			}

		case errors.Is(err, location.ErrNoResults):
			// The geocoder does not know this museum by name — many are
			// historical, merged, or too small for OpenStreetMap. Falling back
			// to the centre of its town makes it findable, which is the whole
			// point; the record is marked approximate so a caller drawing pins
			// can tell a surveyed position from a town centre.
			//
			// The town centre comes from museums already placed there rather
			// than from another geocoder call: it costs nothing, and it is
			// derived from the same data the query will search.
			town, townErr := db.LocalityPlace(ctx, search.Normalize(m.Locality))
			if townErr != nil {
				unresolved++
				continue
			}
			lat, lon, approximate = town.Latitude, town.Longitude, true

		default:
			log.Printf("  %q: %v", m.Name, err)
			unresolved++
			continue
		}

		if err := db.SetLocation(ctx, m.ID, lat, lon, approximate); err != nil {
			return err
		}
		if approximate {
			approximated++
			continue
		}
		located++
	}

	log.Printf("Finished in %s: located %d exactly, %d at their town centre, %d still unresolved",
		time.Since(start).Round(time.Second), located, approximated, unresolved)
	return nil
}

// placeOf describes where a museum is said to be, for logging.
func placeOf(locality, country string) string {
	switch {
	case locality != "" && country != "":
		return locality + ", " + country
	case locality != "":
		return locality
	case country != "":
		return country
	default:
		return "no place recorded"
	}
}
