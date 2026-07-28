package wikidata

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"museum/internal/models"
)

const (
	// museumClass is Q33506, "museum". Queries walk P279* from it so every
	// subclass — art museum, maritime museum, house museum, science centre —
	// is included.
	museumClass = "wd:Q33506"

	// pageSize is how many museums are resolved per request. It trades two
	// limits off against each other: the query service caps execution at 60
	// seconds, and the optional joins multiply each museum into several result
	// rows, so a larger page produces a response big enough that the endpoint
	// starts truncating it mid-transfer.
	pageSize = 1000

	// labelLanguages is the fallback chain the label service walks to name each
	// museum. It is long on purpose: an entity with no label in any listed
	// language has no usable name and gets dropped, and a short chain silently
	// loses whole regions — most Japanese, Chinese, Korean and Arabic museum
	// items carry no English label at all.
	labelLanguages = "en,fr,de,es,it,nl,pt,sv,pl,ru,ja,zh,ko,ar,he,cs,hu,fi,no,da,uk,tr,ro,el,ca,id,fa,th,vi,sk,sl,bg,hr,sr,lt,lv,et,nb,gl,eu,af,ms,hi,bn,ta,is,sq,mk,be,ka,hy,az,kk,uz,ur,ne,si,my,km,lo,mn,sw"
)

// coordRe extracts longitude and latitude from the WKT literal Wikidata
// returns, e.g. "Point(2.3380277 48.8611473)". Longitude comes first.
var coordRe = regexp.MustCompile(`^Point\(\s*(-?[0-9.]+)\s+(-?[0-9.]+)\s*\)$`)

// Country is a country that has museums, with how many.
type Country struct {
	// ID is the Wikidata entity id, e.g. "Q142".
	ID string
	// Name is the English label, e.g. "France".
	Name string
	// Museums is how many museums are attributed to it.
	Museums int
}

// Service streams museums out of Wikidata.
type Service struct {
	client *Client
}

// NewService returns a Service backed by client.
func NewService(client *Client) *Service {
	return &Service{client: client}
}

// Countries lists every country with at least one museum, largest first.
//
// The counts and the labels are fetched separately on purpose. Running the
// label service inside the GROUP BY makes the query aggregate and label 80,000
// rows in one go, which exceeds the query service's 60-second execution cap;
// split in two, both halves return in seconds.
func (s *Service) Countries(ctx context.Context) ([]Country, error) {
	counts := fmt.Sprintf(`
SELECT ?country (COUNT(DISTINCT ?item) AS ?n) WHERE {
  ?item wdt:P31/wdt:P279* %s ; wdt:P17 ?country .
}
GROUP BY ?country
ORDER BY DESC(?n)`, museumClass)

	rows, err := s.client.query(ctx, counts)
	if err != nil {
		return nil, fmt.Errorf("count museums per country: %w", err)
	}

	countries := make([]Country, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		id := entityID(row["country"])
		if id == "" {
			continue
		}
		count, _ := strconv.Atoi(row["n"])
		countries = append(countries, Country{ID: id, Museums: count})
		ids = append(ids, id)
	}

	labels, err := s.countryLabels(ctx, ids)
	if err != nil {
		// Labels are only used for the country field and for logging; the crawl
		// is still correct without them.
		log.Printf("wikidata: cannot resolve country labels: %v", err)
	}
	for i := range countries {
		countries[i].Name = labels[countries[i].ID]
	}
	return countries, nil
}

// countryLabels resolves entity ids to English labels in one request.
func (s *Service) countryLabels(ctx context.Context, ids []string) (map[string]string, error) {
	labels := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return labels, nil
	}

	var values strings.Builder
	for _, id := range ids {
		values.WriteString("wd:")
		values.WriteString(id)
		values.WriteByte(' ')
	}

	sparql := fmt.Sprintf(`
SELECT ?country ?countryLabel WHERE {
  VALUES ?country { %s }
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en". }
}`, values.String())

	rows, err := s.client.query(ctx, sparql)
	if err != nil {
		return labels, err
	}
	for _, row := range rows {
		id := entityID(row["country"])
		if label := row["countryLabel"]; label != "" && label != id {
			labels[id] = label
		}
	}
	return labels, nil
}

// Museums streams every museum Wikidata knows about, country by country, and
// finishes with the museums that have no country recorded so none are lost.
//
// The returned channel is closed when the walk completes or ctx is cancelled.
// Failures for one country are logged and skipped rather than aborting the run.
func (s *Service) Museums(ctx context.Context) <-chan models.Museum {
	out := make(chan models.Museum)

	go func() {
		defer close(out)

		countries, err := s.Countries(ctx)
		if err != nil {
			log.Printf("wikidata: cannot list countries: %v", err)
			return
		}
		log.Printf("wikidata: %d countries with museums", len(countries))

		total := 0
		for _, country := range countries {
			if ctx.Err() != nil {
				return
			}
			n, err := s.streamCountry(ctx, country, out)
			if err != nil {
				log.Printf("wikidata: skipping %s: %v", country.Name, err)
				continue
			}
			total += n
			log.Printf("wikidata: %-28s %5d museums (running total %d)", country.Name, n, total)
		}

		n, err := s.streamStateless(ctx, out)
		if err != nil {
			log.Printf("wikidata: skipping country-less museums: %v", err)
		} else {
			total += n
			log.Printf("wikidata: %-28s %5d museums (running total %d)", "(no country recorded)", n, total)
		}

		log.Printf("wikidata: finished, %d museums", total)
	}()

	return out
}

// CountryMuseums returns every museum Wikidata attributes to one country. It is
// the scoped form of Museums, useful for testing and for partial refreshes.
func (s *Service) CountryMuseums(ctx context.Context, country Country) ([]models.Museum, error) {
	collected := make(chan models.Museum, 16)
	var (
		museums []models.Museum
		done    = make(chan struct{})
	)
	go func() {
		defer close(done)
		for museum := range collected {
			museums = append(museums, museum)
		}
	}()

	_, err := s.streamCountry(ctx, country, collected)
	close(collected)
	<-done

	return museums, err
}

// streamCountry pages through one country's museums.
func (s *Service) streamCountry(ctx context.Context, country Country, out chan<- models.Museum) (int, error) {
	selector := fmt.Sprintf("?item wdt:P31/wdt:P279* %s ; wdt:P17 wd:%s .", museumClass, country.ID)
	return s.streamPaged(ctx, selector, country.Name, out)
}

// streamStateless pages through museums with no P17 country statement. They are
// a small but real slice of the data and would otherwise be invisible.
func (s *Service) streamStateless(ctx context.Context, out chan<- models.Museum) (int, error) {
	selector := fmt.Sprintf(
		"?item wdt:P31/wdt:P279* %s . FILTER NOT EXISTS { ?item wdt:P17 ?anyCountry }", museumClass)
	return s.streamPaged(ctx, selector, "", out)
}

// streamPaged runs the museum query in pages until a page comes back short.
//
// The inner subquery selects and pages the museums on their own; the OPTIONAL
// clauses are joined outside it. Two details in that subquery are load-bearing:
//
//   - Paging happens inside it, because the optional joins multiply rows — a
//     museum with three locality statements produces three rows — so a LIMIT on
//     the joined result would slice through the middle of an entity.
//   - The selection is DISTINCT, because "wdt:P31/wdt:P279*" reaches an entity
//     once per path through the subclass hierarchy. Without it a LIMIT of 1000
//     returns 1000 rows covering only ~940 distinct museums, the page looks
//     short, and paging stops after the first page: Germany yielded 936 of its
//     8,515 museums.
func (s *Service) streamPaged(ctx context.Context, selector, countryName string, out chan<- models.Museum) (int, error) {
	emitted := 0

	for offset := 0; ; offset += pageSize {
		if ctx.Err() != nil {
			return emitted, ctx.Err()
		}

		// Aliases are English-only, and deliberately so. skos:altLabel is
		// multi-valued, so each language added multiplies the rows a page
		// returns: English costs nothing measurable, five languages cost 60%
		// more bytes, and twelve made the service time out at 504 — which
		// during a crawl loses a whole page of a country. English is also where
		// the acronyms live ("MoMA", "the Met"), and an acronym is the case
		// nothing else in the pipeline can recover: "moma" matched a gallery in
		// Wales while the Museum of Modern Art was unreachable, because no
		// spelling of the query resembles the stored name.
		//
		// The gap this leaves is a museum whose English label is a translation
		// and whose native name is only an alias. The label service's fallback
		// chain covers most of those already by naming the museum natively when
		// no English label exists.
		sparql := fmt.Sprintf(`
SELECT ?item ?itemLabel ?desc ?coord ?website ?article ?localityLabel ?countryLabel ?sitelinks ?alt WHERE {
  { SELECT DISTINCT ?item WHERE { %s } ORDER BY ?item LIMIT %d OFFSET %d }
  OPTIONAL { ?item wdt:P625 ?coord }
  OPTIONAL { ?item wdt:P856 ?website }
  OPTIONAL { ?item wdt:P131 ?locality }
  OPTIONAL { ?item wdt:P17 ?country }
  OPTIONAL { ?article schema:about ?item ; schema:isPartOf <https://en.wikipedia.org/> }
  OPTIONAL { ?item schema:description ?desc FILTER(LANG(?desc) = "en") }
  OPTIONAL { ?item wikibase:sitelinks ?sitelinks }
  OPTIONAL { ?item skos:altLabel ?alt FILTER(LANG(?alt) = "en") }
  SERVICE wikibase:label { bd:serviceParam wikibase:language "%s". }
}`, selector, pageSize, offset, labelLanguages)

		rows, err := s.client.query(ctx, sparql)
		if err != nil {
			return emitted, err
		}

		museums, entities := museumsFromRows(rows, countryName)
		for _, m := range museums {
			select {
			case out <- m:
				emitted++
			case <-ctx.Done():
				return emitted, ctx.Err()
			}
		}

		// The end of the data is decided by how many entities the page
		// contained, never by how many survived filtering. Entities with no
		// label in any requested language are dropped, so a full page can yield
		// fewer museums than requested — treating that as the end silently
		// truncated large countries to their first page.
		if entities < pageSize {
			return emitted, nil
		}
	}
}

// museumsFromRows collapses the multiple rows an entity can produce into one
// museum each, preserving the order the query returned them in. It also reports
// how many distinct entities the rows covered, which is what callers must use
// to decide whether a page was full — the museum count is lower whenever an
// entity is dropped for having no usable name.
func museumsFromRows(rows []binding, fallbackCountry string) (museums []models.Museum, entities int) {
	var (
		order []string
		byID  = make(map[string]*models.Museum, len(rows))
	)

	for _, row := range rows {
		id := entityID(row["item"])
		if id == "" {
			continue
		}

		museum, seen := byID[id]
		if !seen {
			museum = &models.Museum{
				WikidataID: id,
				Name:       row["itemLabel"],
				Sources:    []string{SourceName},
			}
			byID[id] = museum
			order = append(order, id)
		}

		// Later rows only fill gaps: any single statement is enough, and the
		// first one the query returned is as good as any.
		fillString(&museum.Name, row["itemLabel"])
		fillString(&museum.Description, row["desc"])
		fillString(&museum.Website, row["website"])
		fillString(&museum.WikipediaURL, row["article"])
		fillString(&museum.Locality, row["localityLabel"])
		fillString(&museum.Country, row["countryLabel"])

		if !museum.HasCoordinates() {
			if lat, lon, ok := parsePoint(row["coord"]); ok {
				museum.Latitude, museum.Longitude = lat, lon
			}
		}
		// Aliases accumulate: altLabel is multi-valued, so a museum's alternative
		// names arrive spread across several rows rather than in one.
		if alt := strings.TrimSpace(row["alt"]); alt != "" &&
			alt != museum.Name && !slices.Contains(museum.AlsoKnownAs, alt) {
			museum.AlsoKnownAs = append(museum.AlsoKnownAs, alt)
		}
		if museum.Sitelinks == 0 {
			// Clamped: the search score takes ln(1 + sitelinks), which errors at
			// zero, so a negative value from a malformed response would break
			// every search that scored against that row.
			if n, err := strconv.Atoi(row["sitelinks"]); err == nil && n > 0 {
				museum.Sitelinks = n
			}
		}
	}

	museums = make([]models.Museum, 0, len(order))
	for _, id := range order {
		m := byID[id]
		fillString(&m.Country, fallbackCountry)
		if m.Country == "" {
			m.Country = "unknown"
		}
		// An entity label falls back to the bare Q-id when no label exists in
		// any requested language; such a record has no usable name.
		if m.Name == "" || m.Name == m.WikidataID {
			continue
		}
		m.Verified = m.WikipediaURL != ""
		museums = append(museums, *m)
	}
	return museums, len(order)
}

// SourceName identifies records that came from Wikidata.
const SourceName = "wikidata"

// fillString assigns value to target when target is still empty.
func fillString(target *string, value string) {
	if *target == "" {
		*target = strings.TrimSpace(value)
	}
}

// entityID turns a Wikidata entity URI into its bare id ("Q142").
func entityID(uri string) string {
	if idx := strings.LastIndex(uri, "/"); idx != -1 {
		return uri[idx+1:]
	}
	return uri
}

// parsePoint reads the "Point(lon lat)" literal Wikidata uses for coordinates.
func parsePoint(literal string) (lat, lon float64, ok bool) {
	m := coordRe.FindStringSubmatch(strings.TrimSpace(literal))
	if m == nil {
		return 0, 0, false
	}
	lon, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, 0, false
	}
	lat, err = strconv.ParseFloat(m[2], 64)
	if err != nil {
		return 0, 0, false
	}
	return lat, lon, true
}
