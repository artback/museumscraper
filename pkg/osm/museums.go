package osm

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"

	"museum/internal/models"
	"museum/pkg/geo"
)

// Service streams museums out of OpenStreetMap.
type Service struct {
	client *Client
}

// NewService returns a Service backed by client.
func NewService(client *Client) *Service {
	return &Service{client: client}
}

// Museums streams the museums OpenStreetMap holds for every country this
// build recognises, one country at a time.
//
// Overpass cannot answer a single global "all museums" query within its
// execution budget, so the work is split by country area. The channel is
// closed when the walk finishes or ctx is cancelled; a country that fails is
// logged and skipped rather than aborting the run.
func (s *Service) Museums(ctx context.Context) <-chan models.Museum {
	out := make(chan models.Museum)

	go func() {
		defer close(out)

		countries := geo.Countries()
		log.Printf("osm: querying %d countries", len(countries))

		total, failed := 0, 0
		for _, country := range countries {
			if ctx.Err() != nil {
				return
			}

			code, ok := geo.ISOCode(country)
			if !ok {
				continue
			}

			museums, err := s.CountryMuseums(ctx, country, code)
			if err != nil {
				log.Printf("osm: skipping %s (%s): %v", country, code, err)
				failed++
				continue
			}

			for _, museum := range museums {
				select {
				case out <- museum:
					total++
				case <-ctx.Done():
					return
				}
			}
			if len(museums) > 0 {
				log.Printf("osm: %-28s %5d museums (running total %d)", country, len(museums), total)
			}
		}

		log.Printf("osm: finished, %d museums (%d countries failed)", total, failed)
	}()

	return out
}

// CountryMuseums returns every museum OpenStreetMap records inside a country.
//
// Nodes, ways and relations are all queried: a museum is a node when someone
// dropped a pin on it and a way or relation when its building outline is
// mapped. "out center" asks Overpass to compute a representative point for the
// latter two, which otherwise carry no coordinates of their own.
func (s *Service) CountryMuseums(ctx context.Context, country, isoCode string) ([]models.Museum, error) {
	overpassQL := fmt.Sprintf(`[out:json][timeout:%d];
area["ISO3166-1"="%s"]->.searchArea;
(
  node(area.searchArea)["tourism"="museum"];
  way(area.searchArea)["tourism"="museum"];
  relation(area.searchArea)["tourism"="museum"];
);
out center tags;`, serverTimeout, isoCode)

	elements, err := s.client.query(ctx, overpassQL)
	if err != nil {
		return nil, err
	}

	museums := make([]models.Museum, 0, len(elements))
	seen := make(map[string]struct{}, len(elements))

	for _, e := range elements {
		museum, ok := e.toMuseum(country)
		if !ok {
			continue
		}
		// The same museum is occasionally mapped as both a node and a building
		// outline; the name is enough to collapse those within one country.
		key := strings.ToLower(museum.Name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		museums = append(museums, museum)
	}
	return museums, nil
}

// toMuseum converts an OSM element, reporting false when it carries no name.
// An unnamed pin is not a usable catalogue entry.
func (e element) toMuseum(country string) (models.Museum, bool) {
	name := firstTag(e.Tags, "name:en", "int_name", "name")
	if strings.TrimSpace(name) == "" {
		return models.Museum{}, false
	}

	lat, lon := e.Lat, e.Lon
	if lat == 0 && lon == 0 {
		lat, lon = e.Center.Lat, e.Center.Lon
	}

	museum := models.Museum{
		Name:        name,
		AlsoKnownAs: otherNames(e.Tags, name),
		Country:     country,
		Locality:    firstTag(e.Tags, "addr:city", "addr:town", "addr:village", "addr:suburb"),
		Website:     firstTag(e.Tags, "website", "contact:website", "url"),
		Latitude:    lat,
		Longitude:   lon,
		// Wikidata and Wikipedia tags are what let the merger fold this record
		// into the one the wiki sources produced.
		WikidataID: strings.TrimSpace(e.Tags["wikidata"]),
		SourcePage: fmt.Sprintf("%s/%d", e.Type, e.ID),
		Sources:    []string{SourceName},
	}
	if article := wikipediaURL(e.Tags["wikipedia"]); article != "" {
		museum.WikipediaURL = article
		museum.Verified = true
	}
	if museum.Country == "" {
		museum.Country = "unknown"
	}
	return museum, true
}

// otherNames collects the name variants an element carries besides primary, so
// a record labelled in English can still be matched against one labelled in the
// local language.
func otherNames(tags map[string]string, primary string) []string {
	var names []string
	seen := map[string]struct{}{strings.ToLower(primary): {}}

	for key, value := range tags {
		if key != "name" && key != "int_name" && key != "alt_name" && !strings.HasPrefix(key, "name:") {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, dup := seen[strings.ToLower(value)]; dup {
			continue
		}
		seen[strings.ToLower(value)] = struct{}{}
		names = append(names, value)
	}
	slices.Sort(names)
	return names
}

// firstTag returns the first non-empty tag among keys.
func firstTag(tags map[string]string, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(tags[key]); v != "" {
			return v
		}
	}
	return ""
}

// wikipediaURL turns OSM's "lang:Article Title" wikipedia tag into a URL, and
// returns "" for anything that is not an English article — a link to another
// language's Wikipedia would not match what the wiki sources recorded.
func wikipediaURL(tag string) string {
	lang, title, found := strings.Cut(strings.TrimSpace(tag), ":")
	if !found || lang != "en" || title == "" {
		return ""
	}
	return "https://en.wikipedia.org/wiki/" + strings.ReplaceAll(title, " ", "_")
}
