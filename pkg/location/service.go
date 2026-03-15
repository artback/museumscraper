package location

// NominatimLocation holds enriched info about a place from the Nominatim API.
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

// NominatimResponse is the API response shape for Nominatim search.
type NominatimResponse []NominatimLocation
