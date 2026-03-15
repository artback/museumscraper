package location

// NominatimDetailsResponse holds the full details response from Nominatim's
// /details endpoint.
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
