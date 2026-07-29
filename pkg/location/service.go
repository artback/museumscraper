// Package location geocodes place names against Nominatim (OpenStreetMap).
//
// Nominatim's usage policy requires a descriptive User-Agent identifying the
// application and allows at most one request per second. Both are enforced
// here: requests without a real User-Agent are rejected by the service, which
// is why every lookup goes through the shared client in client.go.
package location

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"museum/internal/models"
)

// NominatimLocation holds enriched info about a place.
type NominatimLocation struct {
	PlaceID     int64   `json:"place_id"`
	Licence     string  `json:"licence"`
	OsmType     string  `json:"osm_type"`
	OsmID       int64   `json:"osm_id"`
	Lat         string  `json:"lat"`
	Lon         string  `json:"lon"`
	Class       string  `json:"class"`
	Type        string  `json:"type"`
	PlaceRank   int     `json:"place_rank"`
	Importance  float64 `json:"importance"`
	AddressType string  `json:"addresstype"`
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	// Address decodes straight into the shared type, so the postal address
	// survives as structured data instead of being flattened into a map that
	// every consumer has to guess its way around.
	Address     models.Address    `json:"address"`
	ExtraTags   map[string]string `json:"extratags"`
	BoundingBox []string          `json:"boundingbox"`
}

// NominatimResponse is the shape of a Nominatim search response.
type NominatimResponse []NominatimLocation

// Coordinates parses the position Nominatim reported. It returns an error
// rather than a zero pair, because (0, 0) is a real place in the Atlantic and
// silently storing it would put museums there.
func (l NominatimLocation) Coordinates() (lat, lon float64, err error) {
	lat, latErr := strconv.ParseFloat(l.Lat, 64)
	lon, lonErr := strconv.ParseFloat(l.Lon, 64)
	if latErr != nil || lonErr != nil {
		return 0, 0, fmt.Errorf("unusable coordinates %q, %q", l.Lat, l.Lon)
	}
	return lat, lon, nil
}

// Locality returns the most specific settlement name Nominatim supplied.
func (l NominatimLocation) Locality() string { return l.Address.Locality() }

// Website returns the place's official website, when OSM records one.
func (l NominatimLocation) Website() string {
	for _, key := range []string{"website", "contact:website", "url"} {
		if v := l.ExtraTags[key]; v != "" {
			return v
		}
	}
	return ""
}

// Geocode looks up a place name and returns the best matching result.
// It returns ErrNoResults when Nominatim knows nothing about the query.
func Geocode(ctx context.Context, query string) (*NominatimLocation, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("addressdetails", "1")
	params.Set("extratags", "1")
	params.Set("limit", "1")
	params.Set("accept-language", "en")

	var results NominatimResponse
	if err := get(ctx, "/search", params, &results); err != nil {
		return nil, fmt.Errorf("geocode %q: %w", query, err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("geocode %q: %w", query, ErrNoResults)
	}
	return &results[0], nil
}
