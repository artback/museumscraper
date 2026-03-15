package location

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

const (
	photonSearchURL  = "https://photon.komoot.io/api/"
	photonUserAgent  = "museumscraper/1.0"
	photonMaxRetries = 3
)

// photonResponse represents Photon's GeoJSON FeatureCollection response.
type photonResponse struct {
	Features []photonFeature `json:"features"`
}

type photonFeature struct {
	Geometry struct {
		Coordinates []float64 `json:"coordinates"` // [lon, lat]
	} `json:"geometry"`
	Properties photonProperties `json:"properties"`
}

type photonProperties struct {
	OsmID       int64   `json:"osm_id"`
	OsmType     string  `json:"osm_type"` // "N", "W", "R"
	OsmKey      string  `json:"osm_key"`
	OsmValue    string  `json:"osm_value"`
	Name        string  `json:"name"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countrycode"`
	City        string  `json:"city"`
	Postcode    string  `json:"postcode"`
	Street      string  `json:"street"`
	HouseNumber string  `json:"housenumber"`
	State       string  `json:"state"`
	Extent      []float64 `json:"extent"`
}

// PhotonGeocoder implements Geocoder using Komoot's Photon API.
// Photon is free, open-source, based on OpenStreetMap data, and does not
// require API keys. It has more generous rate limits than Nominatim.
type PhotonGeocoder struct {
	client *http.Client
}

// NewPhotonGeocoder creates a Photon geocoder.
func NewPhotonGeocoder() *PhotonGeocoder {
	return &PhotonGeocoder{
		client: newHTTPClient(),
	}
}

func (p *PhotonGeocoder) Geocode(ctx context.Context, query string) (*GeoResult, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("limit", "1")
	params.Set("lang", "en")

	reqURL := fmt.Sprintf("%s?%s", photonSearchURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("photon: create request: %w", err)
	}
	req.Header.Set("User-Agent", photonUserAgent)

	resp, err := doWithRetry(ctx, p.client, req, photonMaxRetries)
	if err != nil {
		return nil, fmt.Errorf("photon: search %q: %w", query, err)
	}
	defer resp.Body.Close()

	var result photonResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("photon: decode response: %w", err)
	}

	if len(result.Features) == 0 {
		return nil, fmt.Errorf("photon: no results for %q", query)
	}

	return photonToGeoResult(&result.Features[0]), nil
}

func photonToGeoResult(f *photonFeature) *GeoResult {
	var lat, lon float64
	if len(f.Geometry.Coordinates) >= 2 {
		lon = f.Geometry.Coordinates[0]
		lat = f.Geometry.Coordinates[1]
	}

	// Photon uses short OSM type codes; expand to match Nominatim's format
	osmType := f.Properties.OsmType
	switch osmType {
	case "N":
		osmType = "node"
	case "W":
		osmType = "way"
	case "R":
		osmType = "relation"
	}

	return &GeoResult{
		OsmType:     osmType,
		OsmID:       f.Properties.OsmID,
		Lat:         lat,
		Lon:         lon,
		Name:        f.Properties.Name,
		DisplayName: formatDisplayName(f.Properties),
		Class:       f.Properties.OsmKey,
		Type:        f.Properties.OsmValue,
		Country:     f.Properties.Country,
		CountryCode: f.Properties.CountryCode,
		City:        f.Properties.City,
		Postcode:    f.Properties.Postcode,
	}
}

func formatDisplayName(p photonProperties) string {
	parts := []string{}
	if p.Name != "" {
		parts = append(parts, p.Name)
	}
	if p.City != "" {
		parts = append(parts, p.City)
	}
	if p.State != "" {
		parts = append(parts, p.State)
	}
	if p.Country != "" {
		parts = append(parts, p.Country)
	}
	result := ""
	for i, part := range parts {
		if i > 0 {
			result += ", "
		}
		result += part
	}
	return result
}
