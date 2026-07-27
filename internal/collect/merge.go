// Package collect combines museum records arriving from several catalogues
// into one deduplicated set.
package collect

import (
	"html"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"museum/internal/models"
)

// Merger accumulates museums from multiple sources, folding records that
// describe the same museum into one.
//
// Matching runs in two passes, strongest evidence first:
//
//  1. Wikidata id — an exact, authoritative identity. Both the Wikidata source
//     and the Wikipedia sources supply it (Wikipedia exposes it in pageprops),
//     so this catches most of the overlap.
//  2. Normalised name plus country — for records where at least one side has no
//     Wikidata id. Names are compared case-insensitively with punctuation
//     ignored, so "Musée d'Orsay" and "Musee d Orsay" meet. Every name a source
//     supplied is tried, not just the primary one: OpenStreetMap names a museum
//     in the local language while Wikidata often labels it in English, and
//     without the alternatives the two records would never meet.
//
// Coordinates are deliberately not used for matching: museum campuses and
// museum-within-a-building cases put genuinely distinct museums within metres
// of each other, and a proximity rule merges them wrongly.
//
// A Merger is safe for concurrent use so several source goroutines can feed it.
type Merger struct {
	mu         sync.Mutex
	byWikidata map[string]int
	byName     map[string]int
	museums    []*models.Museum
	merged     int
}

// NewMerger returns an empty Merger.
func NewMerger() *Merger {
	return &Merger{
		byWikidata: make(map[string]int),
		byName:     make(map[string]int),
	}
}

// Add records a museum, merging it into an existing entry when it matches one.
//
// Names are cleaned here because this is the one point every source passes
// through, so a leaked entity or an invisible formatting character is fixed
// once rather than in each crawler.
func (m *Merger) Add(museum models.Museum) {
	museum.Name = CleanName(museum.Name)
	for i, alias := range museum.AlsoKnownAs {
		museum.AlsoKnownAs[i] = CleanName(alias)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	keys := nameKeys(museum)

	if idx, ok := m.lookup(museum, keys); ok {
		mergeInto(m.museums[idx], museum)
		m.index(idx, *m.museums[idx], keys)
		m.merged++
		return
	}

	stored := museum
	m.museums = append(m.museums, &stored)
	m.index(len(m.museums)-1, stored, keys)
}

// lookup finds an existing record for the museum.
func (m *Merger) lookup(museum models.Museum, keys []string) (int, bool) {
	if museum.WikidataID != "" {
		if idx, ok := m.byWikidata[museum.WikidataID]; ok {
			return idx, true
		}
	}
	for _, key := range keys {
		idx, ok := m.byName[key]
		if !ok {
			continue
		}
		// Never merge two records that carry different Wikidata ids: a name
		// match is weaker evidence than an explicit disagreement.
		existing := m.museums[idx]
		if existing.WikidataID == "" || museum.WikidataID == "" || existing.WikidataID == museum.WikidataID {
			return idx, true
		}
	}
	return 0, false
}

// index records the lookup keys pointing at a stored museum.
func (m *Merger) index(idx int, museum models.Museum, keys []string) {
	if museum.WikidataID != "" {
		m.byWikidata[museum.WikidataID] = idx
	}
	for _, key := range append(keys, nameKeys(museum)...) {
		if _, taken := m.byName[key]; !taken {
			m.byName[key] = idx
		}
	}
}

// Museums returns the merged set in the order museums were first seen.
func (m *Merger) Museums() []models.Museum {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]models.Museum, 0, len(m.museums))
	for _, museum := range m.museums {
		out = append(out, *museum)
	}
	return out
}

// Stats reports how many distinct museums are held and how many incoming
// records were folded into an existing one.
func (m *Merger) Stats() (distinct, merged int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.museums), m.merged
}

// mergeInto folds src into dst, filling gaps without overwriting facts already
// established by an earlier source.
func mergeInto(dst *models.Museum, src models.Museum) {
	fill(&dst.Name, src.Name)
	fill(&dst.Locality, src.Locality)
	fill(&dst.Description, src.Description)
	fill(&dst.WikipediaURL, src.WikipediaURL)
	fill(&dst.WikidataID, src.WikidataID)
	fill(&dst.Website, src.Website)
	fill(&dst.SourcePage, src.SourcePage)

	// Country needs more than gap-filling. The Wikipedia category crawl infers
	// a country from an ancestor category, which is wrong for satellite
	// institutions: "Centre Pompidou Hanwha" sits under a French category but
	// stands in South Korea. Wikidata states the country outright, so it wins
	// outright; otherwise a real country replaces the "unknown" placeholder.
	if known(src.Country) && (!known(dst.Country) || authoritativeCountry(src)) {
		dst.Country = src.Country
	}

	if dst.PageID == 0 {
		dst.PageID = src.PageID
	}
	// Prominence is a property of the museum, not of the source that reported
	// it, so the best-informed answer wins.
	if src.Sitelinks > dst.Sitelinks {
		dst.Sitelinks = src.Sitelinks
	}
	if !dst.HasCoordinates() && src.HasCoordinates() {
		dst.Latitude, dst.Longitude = src.Latitude, src.Longitude
	}
	// Verification is a claim that an English article exists; any source
	// establishing it is enough.
	dst.Verified = dst.Verified || src.Verified

	// The incoming record's own name is an alias too when it differs from the
	// one already stored — that is the usual case for an OpenStreetMap record
	// merging into a Wikidata one, and losing it would throw away the museum's
	// name in its own language.
	for _, alias := range append([]string{src.Name}, src.AlsoKnownAs...) {
		if alias != "" && alias != dst.Name && !slices.Contains(dst.AlsoKnownAs, alias) {
			dst.AlsoKnownAs = append(dst.AlsoKnownAs, alias)
		}
	}
	slices.Sort(dst.AlsoKnownAs)

	for _, source := range src.Sources {
		if !slices.Contains(dst.Sources, source) {
			dst.Sources = append(dst.Sources, source)
		}
	}
	slices.Sort(dst.Sources)
}

// CleanName normalises a museum name for storage and display.
//
// Source labels carry things no reader can see and no consumer wants: HTML
// entities that survived one encoding step too many, and Unicode formatting
// characters such as the left-to-right mark (U+200E) and the line separator
// (U+2028) that editors paste in by accident. They break sorting, matching and
// display while looking identical to a correct name.
func CleanName(name string) string {
	name = html.UnescapeString(name)

	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case unicode.Is(unicode.Cf, r):
			// Format characters: invisible, and meaningless outside the
			// bidirectional text they were pasted from.
		case r == '\u2028' || r == '\u2029':
			// Line and paragraph separators, which are whitespace in disguise.
			b.WriteRune(' ')
		case unicode.IsControl(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}

	return strings.TrimSpace(spacesRe.ReplaceAllString(b.String(), " "))
}

// spacesRe collapses the whitespace that cleaning can leave behind.
var spacesRe = regexp.MustCompile(`\s+`)

// fill assigns value to target when target is still empty.
func fill(target *string, value string) {
	if *target == "" {
		*target = value
	}
}

// CountryAuthority names the source whose country statement overrides one
// inferred by another source.
const CountryAuthority = "wikidata"

// authoritativeCountry reports whether the record's country came from the
// source that states it explicitly rather than inferring it.
func authoritativeCountry(museum models.Museum) bool {
	return slices.Contains(museum.Sources, CountryAuthority)
}

// known reports whether country is a real country rather than empty or the
// "unknown" placeholder.
func known(country string) bool {
	return country != "" && !strings.EqualFold(country, "unknown")
}

// nameKeys builds the fallback matching keys: each of the museum's names,
// paired with its country, reduced to lowercase letters and digits.
//
// It returns nothing when the country is unknown, because a bare name is not
// enough evidence on its own — "City Museum" names dozens of unrelated
// institutions.
func nameKeys(museum models.Museum) []string {
	if !known(museum.Country) {
		return nil
	}
	country := normalizeForMatch(museum.Country)

	var keys []string
	seen := make(map[string]struct{})
	for _, name := range append([]string{museum.Name}, museum.AlsoKnownAs...) {
		normalized := normalizeForMatch(name)
		if normalized == "" {
			continue
		}
		key := normalized + "\x00" + country
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// normalizeForMatch reduces s to its lowercase letters and digits, dropping
// punctuation, spacing and articles that vary between catalogues.
func normalizeForMatch(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
