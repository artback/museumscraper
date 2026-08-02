package wikidata

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

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

	// classNames caches class id to English label for the life of a crawl. An
	// id mapped to the empty string is one that has been asked about and has no
	// usable label, which is what stops it being asked about again.
	//
	// Guarded because Museums runs the walk on its own goroutine while the
	// caller reads the channel, and a caller may run several Services' walks at
	// once.
	mu         sync.Mutex
	classNames map[string]string
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
		attachClasses(museums, s.classesForPage(ctx, selector, offset))
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

// classesForPage resolves what kind of thing each museum on a page is: the
// labels of its P31 statements, e.g. "steamboat", "passenger ship" and "working
// life museum" for S/S Bohuslän.
//
// Three decisions here were each forced by measurement against the live
// endpoint, and each looks like a needless complication until the simpler form
// is tried.
//
// It is a second request rather than another OPTIONAL in the page query.
// P31 is multi-valued, so joining it in multiplies the rows a page returns:
// on Sweden's first page, 1,123 rows and 1.1 MB became 1,927 rows and 2.2 MB.
// That is the pressure the alias comment describes, landing on the one request
// that carries the names, coordinates and websites — and about a quarter of
// page requests already fail with a 502 or 504. Split, a class query that fails
// costs this page its classes and nothing else, because the museums are already
// in hand by the time it runs. The price is one extra request per page, roughly
// 1.2 seconds of rate limiting against a crawl measured in hours.
//
// The P31 pattern is OPTIONAL even though a museum with no P31 cannot be in the
// result set — the selector matched on it. As a required pattern the planner is
// free to start from every P31 statement in Wikidata and join the page in
// afterwards, and it does: five consecutive attempts returned 502 or 504, one
// of them after 70 seconds. OPTIONAL forces evaluation per bound ?item, and the
// same query then answers in about 8 seconds. It is a planner hint written as a
// semantic no-op.
//
// It returns bare class ids, leaving labels to classLabels. Asking the label
// service for ?classLabel inside this query is what made the split version fail
// 5 times out of 5; resolving the ids separately costs one small request per
// batch of previously unseen classes, and classes repeat so heavily across the
// catalogue that after the first few pages there are almost none left to ask
// about.
func (s *Service) classesForPage(ctx context.Context, selector string, offset int) map[string][]string {
	sparql := fmt.Sprintf(`
SELECT ?item ?class WHERE {
  { SELECT DISTINCT ?item WHERE { %s } ORDER BY ?item LIMIT %d OFFSET %d }
  OPTIONAL { ?item wdt:P31 ?class }
}`, selector, pageSize, offset)

	rows, err := s.client.query(ctx, sparql)
	if err != nil {
		// Not fatal, and deliberately so: the museums on this page are already
		// collected, and a record with no classes is the record the catalogue
		// held before this existed.
		log.Printf("wikidata: no classes for this page: %v", err)
		return nil
	}

	ids := classIDsFromRows(rows)
	labels := s.classLabels(ctx, ids)
	classes := make(map[string][]string, len(ids))
	for item, classIDs := range ids {
		for _, id := range classIDs {
			if label := labels[id]; label != "" {
				classes[item] = append(classes[item], label)
			}
		}
	}
	return classes
}

// Details is what Wikidata knows about one entity, in English.
type Details struct {
	// Label is the English label, empty when the entity has none. Wikidata
	// leaves it empty rather than falling back, which is what makes it usable
	// as "is there an English name for this?".
	Label       string
	Description string
	Country     string
	Locality    string
	Classes     []string
	Sitelinks   int
}

// detailsBatch is how many entities one lookup asks about. Small on purpose:
// VALUES with a few hundred entities is answered in about a second, and the
// point of batching here is to amortise the round trip, not to move the whole
// catalogue in one request.
const detailsBatch = 200

// Details resolves entities to their English description, country, town, class
// and label.
//
// This is what makes a non-English crawl usable. Walking Spanish Wikipedia
// finds museums English Wikipedia has never heard of, but it finds them
// described in Spanish and filed under "Categoría:Museos de Alemania" — so the
// country arrives as "Alemania", which no part of the catalogue can group,
// compare or key on. Every one of those articles carries a Wikidata id, and
// Wikidata states the country as an entity rather than a word, so resolving
// through it turns each record into the same shape the English crawl produces.
//
// Notably it does not translate anything. The English label is Wikidata's own,
// which for a museum is almost always its real name rather than a translation
// of it ("Museo Nacional de Antropología" is that in both editions), and where
// no English label exists the caller keeps the name the article gave it.
func (s *Service) Details(ctx context.Context, ids []string) (map[string]Details, error) {
	details := make(map[string]Details, len(ids))
	if len(ids) == 0 {
		return details, nil
	}

	classIDs := make(map[string][]string, len(ids))

	for start := 0; start < len(ids); start += detailsBatch {
		if ctx.Err() != nil {
			return details, ctx.Err()
		}
		batch := ids[start:min(start+detailsBatch, len(ids))]

		var values strings.Builder
		for _, id := range batch {
			values.WriteString("wd:")
			values.WriteString(id)
			values.WriteByte(' ')
		}

		// The class is fetched as a bare id and labelled by classLabels, for the
		// reason classesForPage documents: asking the label service for
		// ?classLabel alongside a P31 join is what made that query fail every
		// time it was tried.
		sparql := fmt.Sprintf(`
SELECT ?item ?itemLabel ?desc ?countryLabel ?localityLabel ?sitelinks ?class WHERE {
  VALUES ?item { %s }
  OPTIONAL { ?item schema:description ?desc FILTER(LANG(?desc) = "en") }
  OPTIONAL { ?item wdt:P17 ?country }
  OPTIONAL { ?item wdt:P131 ?locality }
  OPTIONAL { ?item wikibase:sitelinks ?sitelinks }
  OPTIONAL { ?item wdt:P31 ?class }
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en". }
}`, values.String())

		rows, err := s.client.query(ctx, sparql)
		if err != nil {
			// One failed batch costs those records their English fields and
			// nothing else; they keep the names their own edition gave them.
			log.Printf("wikidata: cannot normalise %d entities: %v", len(batch), err)
			continue
		}

		for _, row := range rows {
			id := entityID(row["item"])
			if id == "" {
				continue
			}
			d := details[id]
			// The label service echoes the bare id when it has no English label,
			// which is exactly the case the caller must be able to detect.
			if label := strings.TrimSpace(row["itemLabel"]); label != "" && label != id {
				fillString(&d.Label, label)
			}
			fillString(&d.Description, row["desc"])
			fillString(&d.Country, row["countryLabel"])
			fillString(&d.Locality, row["localityLabel"])
			if d.Sitelinks == 0 {
				if n, err := strconv.Atoi(row["sitelinks"]); err == nil && n > 0 {
					d.Sitelinks = n
				}
			}
			if class := entityID(row["class"]); class != "" && !slices.Contains(classIDs[id], class) {
				classIDs[id] = append(classIDs[id], class)
			}
			details[id] = d
		}
	}

	labels := s.classLabels(ctx, classIDs)
	for id, classes := range classIDs {
		d := details[id]
		for _, class := range classes {
			if label := labels[class]; label != "" {
				d.Classes = append(d.Classes, label)
			}
		}
		details[id] = d
	}
	return details, nil
}

// classIDsFromRows groups the class ids a page returned by the museum they
// belong to. A museum with no P31 contributes an item row with no class, which
// the OPTIONAL makes possible and which carries no information.
func classIDsFromRows(rows []binding) map[string][]string {
	ids := make(map[string][]string, pageSize)
	for _, row := range rows {
		item, class := entityID(row["item"]), entityID(row["class"])
		if item == "" || class == "" || slices.Contains(ids[item], class) {
			continue
		}
		ids[item] = append(ids[item], class)
	}
	return ids
}

// classLabels resolves class ids to English labels, caching across the whole
// crawl.
//
// The cache is what makes this affordable. Museums are classified from a small
// shared vocabulary — "art museum", "historic house museum", "local museum" —
// so the same few hundred ids recur across all 181,000 records. The first pages
// of a crawl pay for a lookup each; later pages almost always find every class
// already known and make no request at all.
//
// English only, unlike the museum names. A class is ontology vocabulary rather
// than a proper noun, so the long fallback chain buys nothing: the chain exists
// because most Japanese and Arabic museum *items* carry no English label, which
// is not true of the classes they are instances of. One whose English label is
// missing is dropped rather than shown to a reader as a bare Q-id.
func (s *Service) classLabels(ctx context.Context, byItem map[string][]string) map[string]string {
	s.mu.Lock()
	if s.classNames == nil {
		s.classNames = make(map[string]string)
	}
	var unknown []string
	for _, ids := range byItem {
		for _, id := range ids {
			if _, seen := s.classNames[id]; !seen {
				s.classNames[id] = ""
				unknown = append(unknown, id)
			}
		}
	}
	s.mu.Unlock()

	if len(unknown) > 0 {
		// Sorted so a crawl issues the same lookups in the same order whatever
		// order the map happened to iterate in, which is what makes a failure
		// reproducible.
		slices.Sort(unknown)
		s.resolveClassLabels(ctx, unknown)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	resolved := make(map[string]string, len(byItem))
	for _, ids := range byItem {
		for _, id := range ids {
			if label := s.classNames[id]; label != "" {
				resolved[id] = label
			}
		}
	}
	return resolved
}

// classLabelBatch is how many ids one lookup asks about. A VALUES clause of a
// few hundred entities is answered in well under a second; the whole vocabulary
// at once would be a needlessly large request on the first page of a crawl.
const classLabelBatch = 300

// resolveClassLabels fills the cache for ids it does not yet know.
func (s *Service) resolveClassLabels(ctx context.Context, ids []string) {
	for start := 0; start < len(ids); start += classLabelBatch {
		batch := ids[start:min(start+classLabelBatch, len(ids))]

		var values strings.Builder
		for _, id := range batch {
			values.WriteString("wd:")
			values.WriteString(id)
			values.WriteByte(' ')
		}

		sparql := fmt.Sprintf(`
SELECT ?class ?classLabel WHERE {
  VALUES ?class { %s }
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en". }
}`, values.String())

		rows, err := s.client.query(ctx, sparql)
		if err != nil {
			// The ids stay cached as empty, so a failed lookup is not retried
			// for every remaining page of the crawl. They are labels for a tag;
			// re-asking 181,000 times would cost far more than it recovers.
			log.Printf("wikidata: cannot resolve %d class labels: %v", len(batch), err)
			continue
		}

		s.mu.Lock()
		for _, row := range rows {
			id := entityID(row["class"])
			// The label service echoes the bare Q-id when it has no label.
			if label := strings.TrimSpace(row["classLabel"]); id != "" && label != "" && label != id {
				s.classNames[id] = label
			}
		}
		s.mu.Unlock()
	}
}

// attachClasses copies the resolved classes onto the museums they belong to.
func attachClasses(museums []models.Museum, classes map[string][]string) {
	if len(classes) == 0 {
		return
	}
	for i := range museums {
		if found := classes[museums[i].WikidataID]; len(found) > 0 {
			museums[i].Classes = found
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
