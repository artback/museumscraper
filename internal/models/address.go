package models

import "strings"

// Address is a museum's postal address, as returned by the geocoder.
//
// The enrichment pipeline flattens the geocoder's answer into an untyped map,
// which is fine for carrying results between steps and poor for anything that
// wants to read them: every consumer has to know the key names and guess at the
// types. This is the shape worth keeping.
//
// The JSON tags match Nominatim's own field names so a response decodes
// directly into it.
type Address struct {
	HouseNumber   string `json:"house_number,omitempty"`
	Road          string `json:"road,omitempty"`
	Neighbourhood string `json:"neighbourhood,omitempty"`
	Suburb        string `json:"suburb,omitempty"`
	Borough       string `json:"borough,omitempty"`
	City          string `json:"city,omitempty"`
	Town          string `json:"town,omitempty"`
	Village       string `json:"village,omitempty"`
	State         string `json:"state,omitempty"`
	Postcode      string `json:"postcode,omitempty"`
	Country       string `json:"country,omitempty"`
	CountryCode   string `json:"country_code,omitempty"`
}

// Locality returns the most specific settlement the address names.
//
// Geocoders fill exactly one of city, town or village depending on how the
// place is classified, so a caller that reads only "city" misses every museum
// in a village.
func (a Address) Locality() string {
	for _, candidate := range []string{a.City, a.Town, a.Village, a.Suburb, a.Borough} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// Street returns the street line, house number first where there is one.
func (a Address) Street() string {
	switch {
	case a.HouseNumber != "" && a.Road != "":
		return a.HouseNumber + " " + a.Road
	case a.Road != "":
		return a.Road
	default:
		return ""
	}
}

// IsZero reports whether the geocoder returned nothing usable.
func (a Address) IsZero() bool {
	return a.Street() == "" && a.Locality() == "" && a.Postcode == "" && a.Country == ""
}

// OneLine renders the address for display, skipping the parts that are missing.
func (a Address) OneLine() string {
	parts := make([]string, 0, 4)
	for _, part := range []string{a.Street(), a.Postcode, a.Locality(), a.Country} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, ", ")
}
