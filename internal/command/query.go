package command

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"museum/internal/postgres"
	"museum/pkg/location"
)

// queryCommand answers catalogue questions from the terminal.
//
// It runs the same database queries the API serves, so it answers "what would
// the API return" without a server — handy from a shell, a container exec, or a
// scheduled check.
func queryCommand() Command {
	return Command{
		Name:    "query",
		Summary: "Look up museums or exhibitions by location, or search by name",
		Usage:   "(museums|exhibitions|search) ...",
		Run:     runQuery,
	}
}

func runQuery(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("query needs a subject: museums, exhibitions or search")
	}
	subject, rest := args[0], args[1:]

	db, err := database(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	switch subject {
	case "search":
		return queryByName(ctx, db, rest)
	case "museums", "exhibitions":
		return queryByLocation(ctx, db, subject, rest)
	default:
		return fmt.Errorf("unknown subject %q, want museums, exhibitions or search", subject)
	}
}

// queryByName answers a name query — the only interface that reaches museums
// with no coordinates, and the only one tolerant of a misspelling.
func queryByName(ctx context.Context, db *postgres.Store, args []string) error {
	fs := newFlagSet("query search", "QUERY [-limit 20] [-json]", os.Stderr)
	var (
		limit  = fs.Int("limit", 20, "maximum results")
		asJSON = fs.Bool("json", false, "emit JSON instead of a table")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("query search needs something to search for")
	}

	hits, err := db.Search(ctx, query, *limit)
	if err != nil {
		return err
	}
	log.Printf("%q: %d matches", query, len(hits))

	if *asJSON {
		return emitJSON(hits)
	}
	if len(hits) == 0 {
		fmt.Println("Nothing found. Has \"museum reindex\" run since the last crawl?")
		return nil
	}
	for _, hit := range hits {
		where := hit.Museum.Locality
		if where == "" {
			where = hit.Museum.Country
		}
		// A record with no position cannot be shown on a map, which is worth
		// seeing at a glance.
		marker := " "
		if !hit.Museum.HasCoordinates() {
			marker = "?"
		}
		fmt.Printf("%s %5.2f  %-46s %-24s %s\n",
			marker, hit.Score, truncate(hit.Museum.Name, 46), truncate(where, 24), hit.Museum.Website)
	}
	return nil
}

// queryByLocation answers a radius query.
func queryByLocation(ctx context.Context, db *postgres.Store, subject string, args []string) error {
	fs := newFlagSet("query "+subject, "(-place NAME | -lat N -lon N) [-radius 3] [-json]", os.Stderr)
	var (
		place    = fs.String("place", "", "place to search around, geocoded via Nominatim")
		lat      = fs.Float64("lat", 0, "latitude of the search centre")
		lon      = fs.Float64("lon", 0, "longitude of the search centre")
		radius   = fs.Float64("radius", 3, "search radius in kilometres")
		limit    = fs.Int("limit", 50, "maximum results")
		asJSON   = fs.Bool("json", false, "emit JSON instead of a table")
		upcoming = fs.Bool("upcoming", false, "exhibitions: include ones that have not opened yet")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("query", fs.Args()); err != nil {
		return err
	}
	if *radius <= 0 {
		return fmt.Errorf("radius must be greater than zero")
	}

	centreLat, centreLon, label, err := resolveCentre(ctx, *place, *lat, *lon)
	if err != nil {
		return err
	}

	if subject == "museums" {
		hits, err := db.Nearby(ctx, centreLat, centreLon, *radius, *limit)
		if err != nil {
			return err
		}
		log.Printf("%s: %.1f km radius, %d matches", label, *radius, len(hits))

		if *asJSON {
			return emitJSON(hits)
		}
		if len(hits) == 0 {
			fmt.Println("No museums found in that radius.")
			return nil
		}
		for _, hit := range hits {
			link := hit.Museum.Website
			if link == "" {
				link = hit.Museum.WikipediaURL
			}
			fmt.Printf("%6.2f km  %-46s %-22s %s\n",
				hit.DistanceKm, truncate(hit.Museum.Name, 46), truncate(hit.Museum.Locality, 22), link)
		}
		return nil
	}

	hits, err := db.ExhibitionsNearby(ctx, centreLat, centreLon, *radius, *upcoming, *limit)
	if err != nil {
		return err
	}
	log.Printf("%s: %.1f km radius, %d on show", label, *radius, len(hits))

	if *asJSON {
		return emitJSON(hits)
	}
	if len(hits) == 0 {
		fmt.Println("Nothing on show found in that radius. Has \"museum refresh\" run for this area?")
		return nil
	}
	for _, hit := range hits {
		fmt.Printf("%-18s %-44s %-28s %s\n",
			formatRun(hit.Start, hit.End), truncate(hit.Title, 44), truncate(hit.Museum, 28), hit.URL)
	}
	return nil
}

// resolveCentre turns the flags into a search origin, geocoding a place name
// when one was given.
func resolveCentre(ctx context.Context, place string, lat, lon float64) (float64, float64, string, error) {
	if place != "" {
		loc, err := location.Geocode(ctx, place)
		if err != nil {
			return 0, 0, "", fmt.Errorf("cannot locate %q: %w", place, err)
		}
		var placeLat, placeLon float64
		if _, err := fmt.Sscanf(loc.Lat, "%f", &placeLat); err != nil {
			return 0, 0, "", fmt.Errorf("bad latitude %q for %q", loc.Lat, place)
		}
		if _, err := fmt.Sscanf(loc.Lon, "%f", &placeLon); err != nil {
			return 0, 0, "", fmt.Errorf("bad longitude %q for %q", loc.Lon, place)
		}
		return placeLat, placeLon, loc.DisplayName, nil
	}

	if lat == 0 && lon == 0 {
		return 0, 0, "", fmt.Errorf("pass -place, or -lat and -lon")
	}
	return lat, lon, fmt.Sprintf("%.4f, %.4f", lat, lon), nil
}

func emitJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// formatRun renders run dates compactly: a closing date is what a visitor
// needs, and an opening date matters only before it opens.
func formatRun(start, end *time.Time) string {
	now := time.Now()
	switch {
	case start != nil && start.After(now):
		return "from " + start.Format("2 Jan")
	case end != nil:
		return "until " + end.Format("2 Jan 2006")
	case start != nil:
		return "from " + start.Format("2 Jan 2006")
	default:
		return "dates unknown"
	}
}

// truncate shortens s to at most n runes, so multi-byte museum names are not
// cut mid-character.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n-1])) + "\u2026"
}
