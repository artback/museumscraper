package location

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// NominatimLocation holds enriched info about a place
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
	Address     struct {
		Tourism      string `json:"tourism"`
		Road         string `json:"road"`
		CityBlock    string `json:"city_block"`
		Suburb       string `json:"suburb"`
		CityDistrict string `json:"city_district"`
		City         string `json:"city"`
		Town         string `json:"town"`
		Village      string `json:"village"`
		ISO3166Lvl6  string `json:"ISO3166-2-lvl6"`
		Region       string `json:"region"`
		Postcode     string `json:"postcode"`
		Country      string `json:"country"`
		CountryCode  string `json:"country_code"`
	} `json:"address"`
	BoundingBox []string `json:"boundingbox"`
}

// NominatimResponse is shaped for the API response
type NominatimResponse []NominatimLocation

// Geocode looks up a museum/location name and returns coordinates and details.
// It respects Nominatim's rate limit (1 req/sec) and uses a shared HTTP client
// with configured timeouts.
func Geocode(ctx context.Context, query string) (*NominatimLocation, error) {
	<-rateLimit() // enforce 1 req/sec

	base := "https://nominatim.openstreetmap.org/search"

	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("addressdetails", "1")
	params.Set("limit", "1")
	params.Set("accept-language", "en")

	u := fmt.Sprintf("%s?%s", base, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "golang-nominatim-client/1.0")

	resp, err := Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nominatim search: unexpected status %s", resp.Status)
	}

	var results NominatimResponse
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no results for %s", query)
	}

	return &results[0], nil
}
