package command

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"museum/internal/models"
	"museum/internal/postgres"
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
		townOnly = fs.Bool("town-centres", false,
			"skip the geocoder and place every museum at the centre of its recorded town")
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

	start := time.Now()

	// Town-centre placement runs before the unplaced-museum check, because it
	// is also the repair for positions an earlier run got wrong. Those museums
	// are not unplaced — they are placed badly — so gating this on the count of
	// museums without coordinates would make the damage unfixable.
	if *townOnly {
		if *dryRun {
			log.Println("Would recompute every town-centre position (no geocoder calls)")
			return nil
		}
		placed, discarded, err := db.PlaceAtTownCentres(ctx)
		if err != nil {
			return err
		}
		// The second number is reported because it would otherwise be
		// invisible: this recomputes every approximate position rather than
		// only filling empty ones, so a museum an earlier run put in the wrong
		// place is either moved or left with none, and "placed N" alone would
		// not say that anything had been taken away.
		log.Printf("Placed %d museums at their town centres in %s, having discarded %d earlier approximate positions",
			placed, time.Since(start).Round(time.Second), discarded)
		return nil
	}

	pending, err := db.UnplacedMuseums(ctx, *locality, *country, *limit)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		log.Println("Nothing to locate: every matching museum already has coordinates")
		return nil
	}

	// The geocoder's advertised one request per second is a floor, not a
	// promise: a run of 114 museums took 79 minutes because the public instance
	// kept refusing and the backoff kept widening. Saying so up front matters,
	// because the honest estimate for tens of thousands is weeks rather than
	// hours, and -town-centres exists precisely to avoid that.
	log.Printf("Locating %d museums (at least %s, and in practice far longer if the geocoder throttles)",
		len(pending), (time.Duration(len(pending)) * 1100 * time.Millisecond).Round(time.Second))

	if *dryRun {
		for _, m := range pending {
			log.Printf("  would geocode %q (%s)", m.Name, placeOf(m.Locality, m.Country))
		}
		return nil
	}

	var located, approximated, unresolved int

	// Museums cluster heavily by town, so resolving each town once turns tens
	// of thousands of lookups into a few thousand.
	towns := make(map[string]postgres.Place)

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
			town, ok := townCentre(ctx, db, towns, m.Locality)
			if !ok {
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

// townCentre resolves a town to a position, remembering what it resolves.
//
// The centre comes from museums already placed in that town rather than from a
// geocoder: it costs nothing, needs no network, and is derived from the same
// data the query will search. A town nothing is placed in yet cannot be
// resolved, and the museum is left alone rather than guessed at.
func townCentre(ctx context.Context, db *postgres.Store, known map[string]postgres.Place, locality string) (postgres.Place, bool) {
	key := search.Normalize(locality)
	if key == "" {
		return postgres.Place{}, false
	}
	if cached, ok := known[key]; ok {
		return cached, cached.Found
	}

	town, err := db.LocalityPlace(ctx, key)
	if err != nil {
		// Cached as unresolvable too, so a town shared by hundreds of museums
		// is not looked up hundreds of times to fail each time.
		known[key] = postgres.Place{}
		return postgres.Place{}, false
	}
	town.Found = true
	known[key] = town
	return town, true
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
