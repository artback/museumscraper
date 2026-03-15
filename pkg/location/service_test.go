package location_test

import (
	"context"
	"museum/pkg/location"
	"testing"
)

func TestNominatimGeocoder_RealAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	geocoder := location.NewNominatimGeocoder()
	defer geocoder.Close()

	tests := []struct {
		name        string
		query       string
		wantCity    string
		wantCountry string
	}{
		{
			name:        "Louvre Museum France",
			query:       "Louvre Museum France",
			wantCity:    "Paris",
			wantCountry: "France",
		},
		{
			name:        "Metropolitan Museum of Art United States",
			query:       "Metropolitan Museum of Art United States",
			wantCity:    "New York",
			wantCountry: "United States",
		},
		{
			name:        "Prado Museum Spain",
			query:       "Prado Museum Spain",
			wantCity:    "Madrid",
			wantCountry: "Spain",
		},
	}

	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := geocoder.Geocode(ctx, tt.query)
			if err != nil {
				t.Fatalf("Geocode(%q) returned error: %v", tt.query, err)
			}
			if got.City != tt.wantCity {
				t.Errorf("City = %s, want %s", got.City, tt.wantCity)
			}
			if got.Country != tt.wantCountry {
				t.Errorf("Country = %s, want %s", got.Country, tt.wantCountry)
			}
		})
	}
}

func TestPhotonGeocoder_RealAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	geocoder := location.NewPhotonGeocoder()

	tests := []struct {
		name        string
		query       string
		wantCountry string
	}{
		{
			name:        "Louvre Museum France",
			query:       "Louvre Museum France",
			wantCountry: "France",
		},
		{
			name:        "Prado Museum Spain",
			query:       "Prado Museum Spain",
			wantCountry: "Spain",
		},
	}

	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := geocoder.Geocode(ctx, tt.query)
			if err != nil {
				t.Fatalf("Geocode(%q) returned error: %v", tt.query, err)
			}
			if got.Country != tt.wantCountry {
				t.Errorf("Country = %s, want %s", got.Country, tt.wantCountry)
			}
			if got.Lat == 0 && got.Lon == 0 {
				t.Error("expected non-zero coordinates")
			}
		})
	}
}

func TestFallbackGeocoder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	geocoder, cleanup := location.NewDefaultGeocoder()
	defer cleanup()

	ctx := context.Background()
	got, err := geocoder.Geocode(ctx, "Louvre Museum France")
	if err != nil {
		t.Fatalf("FallbackGeocoder.Geocode returned error: %v", err)
	}
	if got.Country != "France" {
		t.Errorf("Country = %s, want France", got.Country)
	}
	if got.Lat == 0 && got.Lon == 0 {
		t.Error("expected non-zero coordinates")
	}
	if got.OsmID == 0 {
		t.Error("expected non-zero OsmID")
	}
}
