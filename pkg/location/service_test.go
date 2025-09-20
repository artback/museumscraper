package location_test

import (
	"museum/pkg/location"
	"testing"
)

func TestGeocode_RealAPI(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		wantCity    string
		wantCountry string
		wantType    string
	}{
		{
			name:        "Louvre Museum France",
			query:       "Louvre Museum France",
			wantCity:    "Paris",
			wantCountry: "France",
			wantType:    "museum",
		},
		{
			name:        "Metropolitan Museum of Art United States",
			query:       "Metropolitan Museum of Art United States",
			wantCity:    "New York",
			wantCountry: "United States",
			wantType:    "museum",
		},
		{
			name:        "Prado Museum Spain",
			query:       "Prado Museum Spain",
			wantCity:    "Madrid",
			wantCountry: "Spain",
			wantType:    "museum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := location.Geocode(tt.query)
			if err != nil {
				t.Fatalf("Geocode(%q) returned error: %v", tt.query, err)
			}
			if got.City != tt.wantCity {
				t.Errorf("City = %s, want %s", got.City, tt.wantCity)
			}
			if got.Country != tt.wantCountry {
				t.Errorf("Country = %s, want %s", got.Country, tt.wantCountry)
			}
			if got.Type != tt.wantType {
				t.Errorf("Type = %s, want %s", got.Type, tt.wantType)
			}
		})
	}
}
