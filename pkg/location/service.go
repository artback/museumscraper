package location

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Location holds enriched info about a place
type Location struct {
	Name      string
	Latitude  float64
	Longitude float64
	City      string
	Country   string
	Road      string
	Type      string
	OsmId     string
}

// NominatimResponse is shaped for the API response
type NominatimResponse []struct {
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

// Geocode looks up a museum/location name and returns coordinates and details
func Geocode(query string) (NominatimResponse, error) {
	base := "https://nominatim.openstreetmap.org/search"

	// Use url.Values to construct query parameters
	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("addressdetails", "1")
	params.Set("limit", "1")
	params.Set("accept-language", "en")

	u := fmt.Sprintf("%s?%s", base, params.Encode())

	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var results NominatimResponse
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no results for %s", query)
	}
	return results, nil

	first := results[0]
	var lat, lon float64
	fmt.Sscanf(first.Lat, "%f", &lat)
	fmt.Sscanf(first.Lon, "%f", &lon)

	city := first.Address.City
	if city == "" {
		city = first.Address.Town
	}
	if city == "" {
		city = first.Address.Village
	}

	return &Location{
		Name:      query,
		Type:      first.Type,
		Latitude:  lat,
		Longitude: lon,
		City:      city,
		Country:   first.Address.Country,
	}, nil
}
