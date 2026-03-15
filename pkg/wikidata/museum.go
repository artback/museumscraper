package wikidata

import (
	"context"
	"fmt"
	"strings"
)

// MuseumInfo holds structured data about a museum from Wikidata.
type MuseumInfo struct {
	WikidataID  string       `json:"wikidata_id,omitempty"`
	Website     string       `json:"website,omitempty"`
	Inception   string       `json:"inception,omitempty"`
	Image       string       `json:"image,omitempty"`
	Collections []string     `json:"collections,omitempty"`
	Exhibitions []Exhibition `json:"exhibitions,omitempty"`
}

// Exhibition represents a museum exhibition with available metadata.
type Exhibition struct {
	Name      string `json:"name"`
	StartDate string `json:"start_date,omitempty"`
	EndDate   string `json:"end_date,omitempty"`
}

// MuseumDetails holds operational details typically from OSM extratags.
type MuseumDetails struct {
	OpeningHours string `json:"opening_hours,omitempty"`
	Admission    string `json:"admission,omitempty"`
	Website      string `json:"website,omitempty"`
	Phone        string `json:"phone,omitempty"`
	Email        string `json:"email,omitempty"`
	Wheelchair   string `json:"wheelchair,omitempty"`
	Description  string `json:"description,omitempty"`
}

// FetchMuseumInfo queries Wikidata for museum information by name and optional country.
func (c *Client) FetchMuseumInfo(ctx context.Context, museumName, country string) (*MuseumInfo, error) {
	// Escape quotes in names for SPARQL
	safeName := strings.ReplaceAll(museumName, `"`, `\"`)
	safeCountry := strings.ReplaceAll(country, `"`, `\"`)

	// SPARQL query: find museum by label, get metadata
	query := fmt.Sprintf(`
SELECT DISTINCT ?museum ?museumLabel ?website ?inception ?image WHERE {
  ?museum rdfs:label "%s"@en .
  ?museum wdt:P31/wdt:P279* wd:Q33506 .
  OPTIONAL { ?museum wdt:P856 ?website . }
  OPTIONAL { ?museum wdt:P571 ?inception . }
  OPTIONAL { ?museum wdt:P18 ?image . }
  %s
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en". }
}
LIMIT 1
`, safeName, countryFilter(safeCountry))

	resp, err := c.query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("fetch museum info for %q: %w", museumName, err)
	}

	if len(resp.Results.Bindings) == 0 {
		return nil, fmt.Errorf("wikidata: no results for museum %q", museumName)
	}

	b := resp.Results.Bindings[0]
	info := &MuseumInfo{
		WikidataID: extractQID(getVal(b, "museum")),
		Website:    getVal(b, "website"),
		Inception:  getVal(b, "inception"),
		Image:      getVal(b, "image"),
	}

	// Fetch exhibitions for this museum
	if info.WikidataID != "" {
		exhibitions, err := c.fetchExhibitions(ctx, info.WikidataID)
		if err == nil {
			info.Exhibitions = exhibitions
		}
	}

	// Fetch notable collections
	if info.WikidataID != "" {
		collections, err := c.fetchCollections(ctx, info.WikidataID)
		if err == nil {
			info.Collections = collections
		}
	}

	return info, nil
}

// fetchExhibitions queries Wikidata for exhibitions held at a museum.
func (c *Client) fetchExhibitions(ctx context.Context, wikidataID string) ([]Exhibition, error) {
	// P276 = location, P31 = instance of, Q464980 = exhibition
	// P580 = start time, P582 = end time
	query := fmt.Sprintf(`
SELECT ?exhibitionLabel ?startDate ?endDate WHERE {
  ?exhibition wdt:P31/wdt:P279* wd:Q464980 .
  ?exhibition wdt:P276 wd:%s .
  OPTIONAL { ?exhibition wdt:P580 ?startDate . }
  OPTIONAL { ?exhibition wdt:P582 ?endDate . }
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en". }
}
ORDER BY DESC(?startDate)
LIMIT 50
`, wikidataID)

	resp, err := c.query(ctx, query)
	if err != nil {
		return nil, err
	}

	var exhibitions []Exhibition
	for _, b := range resp.Results.Bindings {
		name := getVal(b, "exhibitionLabel")
		if name == "" {
			continue
		}
		exhibitions = append(exhibitions, Exhibition{
			Name:      name,
			StartDate: truncateDate(getVal(b, "startDate")),
			EndDate:   truncateDate(getVal(b, "endDate")),
		})
	}
	return exhibitions, nil
}

// fetchCollections queries Wikidata for notable collections at a museum.
func (c *Client) fetchCollections(ctx context.Context, wikidataID string) ([]string, error) {
	// P195 = collection, but we want to find what collections the museum has
	// P31 = instance of, Q7328910 = art collection
	query := fmt.Sprintf(`
SELECT ?collectionLabel WHERE {
  ?collection wdt:P195 wd:%s .
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en". }
}
LIMIT 20
`, wikidataID)

	resp, err := c.query(ctx, query)
	if err != nil {
		return nil, err
	}

	var collections []string
	for _, b := range resp.Results.Bindings {
		name := getVal(b, "collectionLabel")
		if name != "" {
			collections = append(collections, name)
		}
	}
	return collections, nil
}

// ExtractMuseumDetails extracts operational info from Nominatim extratags.
func ExtractMuseumDetails(extratags map[string]string) *MuseumDetails {
	if len(extratags) == 0 {
		return nil
	}

	details := &MuseumDetails{
		OpeningHours: extratags["opening_hours"],
		Admission:    firstNonEmpty(extratags["fee"], extratags["charge"]),
		Website:      firstNonEmpty(extratags["website"], extratags["url"], extratags["contact:website"]),
		Phone:        firstNonEmpty(extratags["phone"], extratags["contact:phone"]),
		Email:        firstNonEmpty(extratags["email"], extratags["contact:email"]),
		Wheelchair:   extratags["wheelchair"],
		Description:  firstNonEmpty(extratags["description"], extratags["description:en"]),
	}

	if details.OpeningHours == "" && details.Website == "" && details.Phone == "" && details.Admission == "" {
		return nil
	}
	return details
}

func countryFilter(country string) string {
	if country == "" {
		return ""
	}
	return fmt.Sprintf(`?museum wdt:P17 ?country . ?country rdfs:label "%s"@en .`, country)
}

func getVal(binding map[string]struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}, key string) string {
	if v, ok := binding[key]; ok {
		return v.Value
	}
	return ""
}

func extractQID(uri string) string {
	if idx := strings.LastIndex(uri, "/"); idx >= 0 {
		return uri[idx+1:]
	}
	return uri
}

func truncateDate(datetime string) string {
	if len(datetime) >= 10 {
		return datetime[:10]
	}
	return datetime
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
