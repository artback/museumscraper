// Package quality audits the catalogue for records that are wrong, unusable or
// suspicious.
//
// The pipeline reproduces what its sources say, and sources contain errors: a
// Wikidata museum can carry a French country, French coordinates and an English
// description reading "museum in Isfahan Province, Iran". Nothing upstream will
// catch that, and nothing downstream can tell it apart from a correct record.
// These checks make such records visible and countable.
//
// A finding is a signal, not a verdict. Several checks are heuristic and will
// flag correct records; they earn their place by being cheap to review and by
// making a regression in the crawlers show up as a sudden jump in one count.
package quality

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"museum/internal/models"
	"museum/pkg/exhibitions"
	"museum/pkg/geo"
)

// Severity says how much attention a finding deserves.
type Severity int

const (
	// Info is a fact worth counting rather than a fault.
	Info Severity = iota
	// Warning is probably wrong, or unusable for some queries.
	Warning
	// Error is definitely wrong.
	Error
)

func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// Check names identify what was found, and are stable so counts can be tracked
// between runs.
const (
	CheckNoCoordinates      = "no-coordinates"
	CheckNullIsland         = "null-island"
	CheckImpossibleCoords   = "impossible-coordinates"
	CheckCountryContradicts = "country-contradicts-description"
	CheckUnknownCountry     = "unknown-country"
	CheckGeographicOutlier  = "coordinates-far-from-country"
	CheckDuplicate          = "duplicate-record"
	CheckSuspiciousName     = "suspicious-name"
	CheckBadURL             = "unusable-url"
	CheckEndBeforeStart     = "exhibition-ends-before-it-starts"
	CheckImplausibleDates   = "exhibition-dates-implausible"
	CheckBoilerplateTitle   = "exhibition-title-is-boilerplate"
	CheckStaleScrape        = "exhibition-scrape-stale"
	CheckPermanentWithEnd   = "exhibition-permanent-but-ends"
)

// Finding is one problem with one record.
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"-"`
	Level    string   `json:"severity"`
	Subject  string   `json:"subject"`
	Detail   string   `json:"detail"`
	// Reference points at the record: a Wikidata id, or the URL an exhibition
	// was read from.
	Reference string `json:"reference,omitempty"`
}

// Report is the outcome of an audit.
type Report struct {
	Museums     int            `json:"museums_checked"`
	Exhibitions int            `json:"exhibitions_checked"`
	Counts      map[string]int `json:"counts"`
	Findings    []Finding      `json:"findings"`
}

// Errors returns how many findings are outright wrong records.
func (r Report) Errors() int { return r.countAtLeast(Error) }

// Warnings returns how many findings are probably wrong.
func (r Report) Warnings() int { return r.countAtLeast(Warning) - r.Errors() }

func (r Report) countAtLeast(level Severity) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity >= level {
			n++
		}
	}
	return n
}

// add records a finding.
func (r *Report) add(f Finding) {
	f.Level = f.Severity.String()
	if r.Counts == nil {
		r.Counts = map[string]int{}
	}
	r.Counts[f.Check]++
	r.Findings = append(r.Findings, f)
}

// CheckMuseums audits a catalogue.
func CheckMuseums(museums []models.Museum) Report {
	report := Report{Museums: len(museums), Counts: map[string]int{}}

	seen := make(map[string][]models.Museum, len(museums))
	byCountry := make(map[string][]models.Museum)

	for _, m := range museums {
		checkPosition(&report, m)
		checkCountry(&report, m)
		checkName(&report, m)
		checkURLs(&report, m)

		if key := identityKey(m); key != "" {
			seen[key] = append(seen[key], m)
		}
		if m.HasCoordinates() && geo.IsCountry(m.Country) {
			byCountry[m.Country] = append(byCountry[m.Country], m)
		}
	}

	checkDuplicates(&report, seen)
	checkOutliers(&report, byCountry)

	sortFindings(report.Findings)
	return report
}

// checkPosition reports coordinates that are missing, impossible, or the
// null-island artefact that a failed parse produces.
func checkPosition(report *Report, m models.Museum) {
	if !m.HasCoordinates() {
		report.add(Finding{
			Check: CheckNoCoordinates, Severity: Warning, Subject: m.Name,
			Detail:    "no coordinates, so it cannot be found by any location query",
			Reference: m.WikidataID,
		})
		return
	}

	if math.Abs(m.Latitude) > 90 || math.Abs(m.Longitude) > 180 {
		report.add(Finding{
			Check: CheckImpossibleCoords, Severity: Error, Subject: m.Name,
			Detail:    fmt.Sprintf("coordinates (%v, %v) are outside the valid range", m.Latitude, m.Longitude),
			Reference: m.WikidataID,
		})
		return
	}

	// A record within a kilometre of 0,0 is in the Gulf of Guinea. Museums are
	// not; a parse that lost its digits is.
	if distanceKm(m.Latitude, m.Longitude, 0, 0) < 1 {
		report.add(Finding{
			Check: CheckNullIsland, Severity: Error, Subject: m.Name,
			Detail:    "coordinates sit on null island, which means a failed parse rather than a location",
			Reference: m.WikidataID,
		})
	}
}

// countryInDescription matches the trailing country in the descriptions
// Wikidata writes, such as "museum in Isfahan Province, Iran".
func countryInDescription(description string) string {
	description = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(description), "."))
	if idx := strings.LastIndex(description, ","); idx != -1 {
		candidate := strings.TrimSpace(description[idx+1:])
		if canonical, ok := geo.Canonical(candidate); ok {
			return canonical
		}
	}
	// "museum in Iran" has no comma.
	if idx := strings.LastIndex(strings.ToLower(description), " in "); idx != -1 {
		candidate := strings.TrimSpace(description[idx+4:])
		if canonical, ok := geo.Canonical(candidate); ok {
			return canonical
		}
	}
	return ""
}

// checkCountry compares the country field against the one the description
// names. They disagree when a source is internally inconsistent, which no
// amount of care in this pipeline can prevent — only surface.
func checkCountry(report *Report, m models.Museum) {
	if !geo.IsCountry(m.Country) {
		if m.Country == "" || strings.EqualFold(m.Country, "unknown") {
			report.add(Finding{
				Check: CheckUnknownCountry, Severity: Warning, Subject: m.Name,
				Detail:    "country is unknown, so the record cannot be grouped or keyed reliably",
				Reference: m.WikidataID,
			})
		}
		return
	}

	stated, ok := geo.Canonical(m.Country)
	if !ok {
		return
	}
	described := countryInDescription(m.Description)
	if described == "" || described == stated {
		return
	}
	// Some names are a country and also a province or state of another. A
	// description reading "museum located in Atlanta, Georgia" on a record
	// filed under the United States is consistent, not contradictory.
	for _, parent := range geo.AmbiguousWithSubdivision[described] {
		if parent == stated {
			return
		}
	}
	{
		report.add(Finding{
			Check: CheckCountryContradicts, Severity: Error, Subject: m.Name,
			Detail: fmt.Sprintf("country is %q but the description says %q (%q)",
				stated, described, m.Description),
			Reference: m.WikidataID,
		})
	}
}

// markupArtefacts are the fragments that only appear in a name when a parser
// has leaked its source.
//
// Deliberately absent: a bare "|" and a bare "&". Both look like markup and
// neither is — "Kunstmuseum Winterthur | Beim Stadthaus" and
// "Museum Steinegg|Collepietra" are how those museums write their names, and
// flagging them produced nothing but noise.
var markupArtefacts = []string{"[[", "]]", "{{", "}}", "&lt;", "&gt;", "&amp;", "&quot;", "&#"}

// htmlTagRe matches an actual HTML tag rather than a stray angle bracket.
var htmlTagRe = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9]*(\s[^>]*)?/?>`)

// checkName reports names that no parser should have produced.
//
// Length and character class are not signals here, whatever intuition says. The
// catalogue contains "M+" (Hong Kong), "W5" (Belfast) and "70.8" (Liverpool);
// a minimum length and a "must contain a letter" rule flagged all of them and
// nothing else. Only evidence of leaked markup is diagnostic.
func checkName(report *Report, m models.Museum) {
	name := strings.TrimSpace(m.Name)

	if name == "" {
		report.add(Finding{
			Check: CheckSuspiciousName, Severity: Error, Subject: "(empty)",
			Detail: "record has no name", Reference: m.WikidataID,
		})
		return
	}

	for _, artefact := range markupArtefacts {
		if strings.Contains(name, artefact) {
			report.add(Finding{
				Check: CheckSuspiciousName, Severity: Error, Subject: name,
				Detail:    fmt.Sprintf("name contains %q, so an extractor is leaking raw source", artefact),
				Reference: m.WikidataID,
			})
			return
		}
	}
	if htmlTagRe.MatchString(name) {
		report.add(Finding{
			Check: CheckSuspiciousName, Severity: Error, Subject: name,
			Detail:    "name contains an HTML tag, so an extractor is leaking raw source",
			Reference: m.WikidataID,
		})
	}
}

// checkURLs reports links that cannot be followed.
func checkURLs(report *Report, m models.Museum) {
	for label, raw := range map[string]string{"website": m.Website, "wikipedia_url": m.WikipediaURL} {
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			report.add(Finding{
				Check: CheckBadURL, Severity: Warning, Subject: m.Name,
				Detail:    fmt.Sprintf("%s %q is not a usable http(s) URL", label, raw),
				Reference: m.WikidataID,
			})
		}
	}
}

// identityKey is the key two records would have to share to be the same museum.
func identityKey(m models.Museum) string {
	if !geo.IsCountry(m.Country) || strings.TrimSpace(m.Name) == "" {
		return ""
	}
	return normalize(m.Name) + "\x00" + normalize(m.Country)
}

// checkDuplicates reports museums stored more than once.
//
// The crawl merges within a run, so duplicates mean two runs wrote the same
// museum under different names — which is what happens when sources are
// crawled one at a time instead of together.
func checkDuplicates(report *Report, seen map[string][]models.Museum) {
	for _, group := range seen {
		if len(group) < 2 {
			continue
		}
		names := make([]string, 0, len(group))
		for _, m := range group {
			names = append(names, m.Name)
		}
		sort.Strings(names)

		report.add(Finding{
			Check: CheckDuplicate, Severity: Warning, Subject: group[0].Name,
			Detail:    fmt.Sprintf("%d records share an identity: %s", len(group), strings.Join(names, " / ")),
			Reference: group[0].WikidataID,
		})
	}
}

// minCountryMuseums is how many placed museums a country needs before its
// spread says anything. Below this, one outlier moves the median.
const minCountryMuseums = 30

// checkOutliers reports museums sitting implausibly far from the rest of their
// country's.
//
// There is no country-boundary data here, and pulling some in for this would be
// disproportionate. The museums themselves describe where their country is:
// taking the median position and flagging anything far beyond the spread of the
// other 99% catches a record filed under the wrong country without needing a
// map. Large countries have a large spread and are treated accordingly.
func checkOutliers(report *Report, byCountry map[string][]models.Museum) {
	for country, museums := range byCountry {
		if len(museums) < minCountryMuseums {
			continue
		}

		medianLat := median(field(museums, func(m models.Museum) float64 { return m.Latitude }))
		medianLon := median(field(museums, func(m models.Museum) float64 { return m.Longitude }))

		distances := make([]float64, len(museums))
		for i, m := range museums {
			distances[i] = distanceKm(medianLat, medianLon, m.Latitude, m.Longitude)
		}

		// The threshold is the 99th percentile of the country's own spread,
		// doubled and floored, so a genuinely large country does not report its
		// own extremities and a small one still has room for its islands.
		limit := math.Max(2*percentile(distances, 0.99), 500)

		for i, m := range museums {
			if distances[i] <= limit {
				continue
			}
			report.add(Finding{
				Check: CheckGeographicOutlier, Severity: Warning, Subject: m.Name,
				Detail: fmt.Sprintf("%.0f km from the centre of %s's museums, well beyond the %.0f km the rest span",
					distances[i], country, limit),
				Reference: m.WikidataID,
			})
		}
	}
}

// CheckExhibitions audits scraped listings.
func CheckExhibitions(found []exhibitions.Exhibition, now time.Time) Report {
	report := Report{Exhibitions: len(found), Counts: map[string]int{}}

	for _, e := range found {
		if e.Start != nil && e.End != nil && e.End.Before(*e.Start) {
			report.add(Finding{
				Check: CheckEndBeforeStart, Severity: Error, Subject: e.Title,
				Detail: fmt.Sprintf("runs from %s to %s",
					e.Start.Format("2006-01-02"), e.End.Format("2006-01-02")),
				Reference: e.URL,
			})
		}

		// An exhibition scheduled decades out is a misparsed date, not a plan.
		for label, when := range map[string]*time.Time{"start": e.Start, "end": e.End} {
			if when == nil {
				continue
			}
			if years := when.Sub(now).Hours() / 24 / 365; years > 10 || years < -10 {
				report.add(Finding{
					Check: CheckImplausibleDates, Severity: Warning, Subject: e.Title,
					Detail:    fmt.Sprintf("%s date %s is %.0f years away", label, when.Format("2006-01-02"), years),
					Reference: e.URL,
				})
			}
		}

		// A permanent display is one that does not close, so a closing date on
		// one means the two were read from different places and one of them is
		// wrong. Left in, it puts the exhibition in both answers at once: the
		// list of what is always on, and the list of what to catch before it
		// goes.
		if e.Permanent && e.End != nil {
			report.add(Finding{
				Check: CheckPermanentWithEnd, Severity: Error, Subject: e.Title,
				Detail:    fmt.Sprintf("marked permanent but closes on %s", e.End.Format("2006-01-02")),
				Reference: e.URL,
			})
		}

		if isBoilerplate(e.Title) {
			report.add(Finding{
				Check: CheckBoilerplateTitle, Severity: Error, Subject: e.Title,
				Detail:    "title is a button label, so the extractor failed to find the real one",
				Reference: e.URL,
			})
		}

		if !e.ScrapedAt.IsZero() && now.Sub(e.ScrapedAt) > 30*24*time.Hour {
			report.add(Finding{
				Check: CheckStaleScrape, Severity: Warning, Subject: e.Title,
				Detail:    fmt.Sprintf("last scraped %s ago; run refresh for this area", now.Sub(e.ScrapedAt).Round(24*time.Hour)),
				Reference: e.URL,
			})
		}
	}

	sortFindings(report.Findings)
	return report
}

// boilerplateTitles are the button labels that stand in for a title when a
// listing links out through a "read more" control. The scraper falls back to
// the URL slug when it sees one, so any that survive mean it missed a variant.
var boilerplateTitles = []string{
	"find out more", "read more", "learn more", "see more", "more info",
	"discover", "explore", "view", "details", "en savoir plus", "découvrir",
	"mehr erfahren", "meer info", "book now", "buy tickets",
}

func isBoilerplate(title string) bool {
	normalized := strings.ToLower(strings.Trim(strings.TrimSpace(title), ".>»→ "))
	for _, phrase := range boilerplateTitles {
		if normalized == phrase {
			return true
		}
	}
	return false
}

// sortFindings orders findings by severity, then by check, so the report reads
// worst-first and groups related problems.
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity > findings[j].Severity
		}
		if findings[i].Check != findings[j].Check {
			return findings[i].Check < findings[j].Check
		}
		return findings[i].Subject < findings[j].Subject
	})
}

// distanceKm returns the great-circle distance between two points.
//
// PostGIS computes distances for every query the API serves; this exists only
// for the outlier check, which compares a museum against the spread of its
// country's others in memory rather than per row in SQL.
func distanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusKm = 6371.0
	rad := math.Pi / 180

	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKm * math.Asin(math.Min(1, math.Sqrt(a)))
}

// normalize reduces a string to its lowercase letters and digits.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// field extracts one number from every museum.
func field(museums []models.Museum, get func(models.Museum) float64) []float64 {
	values := make([]float64, len(museums))
	for i, m := range museums {
		values[i] = get(m)
	}
	return values
}

// median returns the middle value, sorting a copy so the caller's slice keeps
// its order.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

// percentile returns the value below which the given fraction of values fall.
func percentile(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	idx := int(fraction * float64(len(sorted)-1))
	return sorted[max(0, min(idx, len(sorted)-1))]
}
