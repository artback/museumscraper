package location

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// NominatimDetailsResponse is the full detail record for a place.
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

// PlaceDetails fetches the full record for an OSM object.
//
// osmType is the single-letter form Nominatim expects ("N", "W" or "R"); the
// long form returned by a search ("node", "way", "relation") is accepted too
// and converted.
func PlaceDetails(ctx context.Context, osmType string, osmID int64) (*NominatimDetailsResponse, error) {
	shortType, err := osmTypeCode(osmType)
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("osmtype", shortType)
	params.Set("osmid", strconv.FormatInt(osmID, 10))
	params.Set("addressdetails", "1")
	params.Set("hierarchy", "0")
	params.Set("group_hierarchy", "1")
	params.Set("extratags", "1")
	params.Set("format", "json")

	var details NominatimDetailsResponse
	if err := get(ctx, "/details", params, &details); err != nil {
		return nil, fmt.Errorf("place details %s%d: %w", shortType, osmID, err)
	}
	return &details, nil
}

// osmTypeCode normalises an OSM element type to the single letter the details
// endpoint requires.
func osmTypeCode(osmType string) (string, error) {
	switch osmType {
	case "N", "W", "R":
		return osmType, nil
	case "node":
		return "N", nil
	case "way":
		return "W", nil
	case "relation":
		return "R", nil
	default:
		return "", fmt.Errorf("unknown osm type %q", osmType)
	}
}
