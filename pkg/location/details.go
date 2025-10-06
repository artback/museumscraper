package location

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

type NominatimDetailsResponse struct {
	PlaceID             int64             `json:"place_id"`
	ParentPlaceID       int64             `json:"parent_place_id"`
	OsmType             string            `json:"osm_type"`
	OsmID               int64             `json:"osm_id"`
	Category            string            `json:"category"`
	Type                string            `json:"type"`
	AdminLevel          int               `json:"admin_level"`
	LocalName           string            `json:"localname"`
	Names               map[string]string `json:"names"`
	AddressTags         map[string]string `json:"addresstags"`
	HouseNumber         string            `json:"housenumber"`
	CalculatedPostcode  string            `json:"calculated_postcode"`
	CountryCode         string            `json:"country_code"`
	Importance          float64           `json:"importance"`
	ExtraTags           map[string]string `json:"extratags"`
	CalculatedWikipedia string            `json:"calculated_wikipedia"`
	Centroid            struct {
		Type        string    `json:"type"`
		Coordinates []float64 `json:"coordinates"`
	} `json:"centroid"`
	Geometry struct {
		Type        string    `json:"type"`
		Coordinates []float64 `json:"coordinates"`
	} `json:"geometry"`
	Icon string `json:"icon"`
}

// PlaceDetails fetches full details about a place from Nominatim.
func PlaceDetails(osmType string, osmID int) (*NominatimDetailsResponse, error) {
	baseURL := "https://nominatim.openstreetmap.org/details"

	params := url.Values{}
	params.Set("osmtype", osmType)
	params.Set("osmid", fmt.Sprintf("%d", osmID))
	params.Set("addressdetails", "1")
	params.Set("hierarchy", "0")
	params.Set("group_hierarchy", "1")
	params.Set("format", "json")

	reqURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "golang-nominatim-client/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var details NominatimDetailsResponse
	if err := json.Unmarshal(body, &details); err != nil {
		return nil, err
	}

	return &details, nil
}
