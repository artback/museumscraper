// Package licence records what each source permits and what it asks for in
// return.
//
// Every source this catalogue reads is open, and none of them is public domain
// by default. OpenStreetMap is ODbL, which requires attribution and puts terms
// on a derived database; Wikipedia's prose is CC BY-SA, which requires
// attribution and share-alike; Wikidata is CC0, which requires nothing and is
// worth recording precisely so it can be told apart from the two that do.
// Nominatim geocodes against OpenStreetMap, so an address or a fallback
// position carries ODbL whatever the museum's own source was.
//
// Collecting the data legally is only half of it. The catalogue serves museum
// names, descriptions and positions over a public API, and redistribution is
// the point at which these licences actually bite: a response carrying a
// Wikipedia description and an OSM position with no notice attached meets
// neither licence. Which obligations a given record carries depends on where
// it came from, so this has to be derived per record rather than stated once
// in a footer.
package licence

import (
	"slices"
	"strings"
)

// Licence is one source's terms.
type Licence struct {
	// Source is the name records carry in their Sources field.
	Source string `json:"source"`
	// Name is the licence in the form people cite it.
	Name string `json:"licence"`
	// URL is the licence text.
	URL string `json:"licence_url"`
	// Attribution is the credit line to reproduce. Empty where the licence
	// asks for none.
	Attribution string `json:"attribution,omitempty"`
	// ShareAlike marks a licence that puts terms on what is built from it, so
	// a redistributor can tell the two kinds apart without parsing the name.
	ShareAlike bool `json:"share_alike,omitempty"`
}

var (
	openStreetMap = Licence{
		Name:        "ODbL 1.0",
		URL:         "https://opendatacommons.org/licenses/odbl/1-0/",
		Attribution: "© OpenStreetMap contributors",
		ShareAlike:  true,
	}
	wikipedia = Licence{
		Name:        "CC BY-SA 4.0",
		URL:         "https://creativecommons.org/licenses/by-sa/4.0/",
		Attribution: "Text from Wikipedia, by its contributors",
		ShareAlike:  true,
	}
	wikidata = Licence{
		Name: "CC0 1.0",
		URL:  "https://creativecommons.org/publicdomain/zero/1.0/",
	}
	// publicDomain covers the United States museum file. It is a work of the
	// US government, which places it outside copyright entirely; IMLS asks only
	// that reuse be acknowledged, which costs nothing and is worth doing.
	publicDomain = Licence{
		Name:        "Public domain (US Government work)",
		URL:         "https://www.imls.gov/about/privacy-terms",
		Attribution: "Museum data from the Institute of Museum and Library Services",
	}
	// licenceOuverte covers the French register. Attribution is a condition
	// rather than a courtesy, and unlike ODbL and CC BY-SA it puts no terms on
	// what is built from the data.
	licenceOuverte = Licence{
		Name:        "Licence Ouverte 2.0",
		URL:         "https://www.etalab.gouv.fr/licence-ouverte-open-licence/",
		Attribution: "Muséofile — Ministère de la Culture",
	}
	// museumWebsite covers what a museum publishes about its own programme.
	// Exhibition titles and dates are facts and are recorded as such, with the
	// page they were read from kept alongside them; the attribution is the
	// museum itself.
	museumWebsite = Licence{
		Name:        "As published by the museum",
		URL:         "",
		Attribution: "Exhibition listings from each museum's own website",
	}
)

// Geocoding is the licence covering addresses and fallback positions, which
// come from Nominatim and are therefore OpenStreetMap data whatever source
// named the museum.
var Geocoding = func() Licence {
	l := openStreetMap
	l.Source = "nominatim"
	return l
}()

// For returns the licence covering a source, and whether the source is one
// this catalogue knows. Language editions are covered by their base source, so
// "wikipedia-category-es" resolves like "wikipedia-category".
func For(source string) (Licence, bool) {
	name := strings.ToLower(strings.TrimSpace(source))

	var l Licence
	switch {
	case name == "openstreetmap":
		l = openStreetMap
	case name == "nominatim":
		return Geocoding, true
	case strings.HasPrefix(name, "wikipedia"):
		l = wikipedia
	case name == "wikidata":
		l = wikidata
	case name == "imls":
		l = publicDomain
	case name == "museofile":
		l = licenceOuverte
	case name == "website", name == "harvest":
		l = museumWebsite
	default:
		return Licence{}, false
	}

	l.Source = name
	return l, true
}

// ForSources returns the distinct licences covering a set of sources, in a
// stable order, skipping any source this package does not know.
//
// Distinct because a page of results naming four Wikipedia editions carries
// one obligation, not four, and a notice that repeats itself is one nobody
// reads.
func ForSources(sources []string) []Licence {
	var out []Licence
	for _, source := range sources {
		l, ok := For(source)
		if !ok {
			continue
		}
		// Keyed on the licence rather than the source, so the editions collapse
		// onto the single credit they actually require.
		if slices.ContainsFunc(out, func(seen Licence) bool { return seen.Name == l.Name }) {
			continue
		}
		l.Source = ""
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b Licence) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// All returns every licence this catalogue redistributes under, for the
// attribution endpoint.
func All() []Licence {
	sources := []string{"wikidata", "wikipedia", "openstreetmap", "nominatim", "imls", "museofile", "website"}
	out := make([]Licence, 0, len(sources))
	for _, s := range sources {
		if l, ok := For(s); ok {
			out = append(out, l)
		}
	}
	return out
}
