// Package models holds the data types that flow through the scraping and
// enrichment pipelines.
package models

// Museum is a single museum discovered on a Wikipedia list page and resolved
// against the Wikipedia API. Name is the canonical article title, so it is
// stable enough to use as a storage key and to re-query later.
type Museum struct {
	Name        string `json:"name"`
	Country     string `json:"country"`
	Locality    string `json:"locality,omitempty"`
	Description string `json:"description,omitempty"`

	WikipediaURL string `json:"wikipedia_url,omitempty"`
	WikidataID   string `json:"wikidata_id,omitempty"`
	PageID       int    `json:"page_id,omitempty"`
	// Website is the museum's own site, where a source records one.
	Website string `json:"website,omitempty"`

	// Address is the postal address the enrichment stage resolved, where it
	// found one.
	Address Address `json:"address,omitzero"`

	// Sitelinks counts the Wikipedia language editions with an article about
	// this museum. It is the closest thing the sources offer to prominence:
	// the Louvre has 167, the Detroit Institute of Arts 29, a village museum
	// one or none. Without it, ranking between two legitimately-named museums
	// is arbitrary.
	Sitelinks int `json:"sitelinks,omitempty"`

	// Latitude and Longitude come from Wikipedia's primary coordinates for the
	// article. They are zero when the article carries no coordinates; use
	// HasCoordinates to tell "unset" apart from a genuine 0,0.
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`

	// SourcePage is the list article the museum was extracted from, kept so a
	// surprising record can be traced back to its origin.
	SourcePage string `json:"source_page,omitempty"`

	// Verified is true when the museum has its own English Wikipedia article.
	// When false the museum was named by a list page but has no English article
	// yet — common for museums documented only on their own language's
	// Wikipedia — so it carries no URL or coordinates from Wikipedia and
	// depends on the enrichment stage for location data.
	Verified bool `json:"verified"`

	// AlsoKnownAs holds the other names a source recorded for this museum —
	// typically its name in the local language when the primary name is the
	// English one, or vice versa. Matching across catalogues uses these as well
	// as Name, because OpenStreetMap names a museum in the local language while
	// Wikidata often labels it in English.
	AlsoKnownAs []string `json:"also_known_as,omitempty"`

	// Sources names every catalogue this record was seen in, e.g. "wikidata",
	// "wikipedia-category", "wikipedia-list". A museum found in several is
	// merged into one record listing them all.
	Sources []string `json:"sources,omitempty"`

	// Classes are the kinds of thing this museum is, as its source classifies
	// it — Wikidata's P31 labels, e.g. "steamboat", "passenger ship", "working
	// life museum" for S/S Bohuslän.
	//
	// Without them a record says what it is called and where it is, but never
	// what it is. That reads as a mis-scrape whenever the name alone is
	// ambiguous: the catalogue holds "Bohuslän", described as a "working life
	// museum in Gothenburg Municipality", which is also the name of a Swedish
	// province — so on a map it looks like a region that was scraped by
	// mistake rather than a preserved 1914 steamship.
	//
	// Multi-valued because classification is: a museum ship is a ship and a
	// museum at once, and dropping either loses the thing that explains it.
	Classes []string `json:"classes,omitempty"`
}

// HasCoordinates reports whether Wikipedia supplied coordinates for the museum.
func (m Museum) HasCoordinates() bool {
	return m.Latitude != 0 || m.Longitude != 0
}

// EnrichedMuseum is a Museum plus the flattened output of the enrichment
// pipeline (geocoding, place details, and anything added by later steps).
type EnrichedMuseum struct {
	Museum Museum         `json:"museum"`
	Data   map[string]any `json:"data,omitempty"`
}
