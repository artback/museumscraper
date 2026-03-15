package location

import (
	"context"
	"fmt"
	"log"
)

// FallbackGeocoder tries multiple geocoder backends in order, returning the
// first successful result. This provides resilience against individual service
// outages, rate limiting, or data gaps.
type FallbackGeocoder struct {
	geocoders []Geocoder
}

// NewFallbackGeocoder creates a geocoder that tries each backend in order.
// It returns the result from the first geocoder that succeeds.
func NewFallbackGeocoder(geocoders ...Geocoder) *FallbackGeocoder {
	return &FallbackGeocoder{geocoders: geocoders}
}

func (f *FallbackGeocoder) Geocode(ctx context.Context, query string) (*GeoResult, error) {
	var lastErr error
	for _, g := range f.geocoders {
		result, err := g.Geocode(ctx, query)
		if err == nil {
			return result, nil
		}
		lastErr = err
		log.Printf("geocoder fallback: %T failed for %q: %v", g, query, err)
	}
	return nil, fmt.Errorf("all geocoders failed for %q: %w", query, lastErr)
}

// NewDefaultGeocoder creates a production-ready geocoder that tries Nominatim
// first (most detailed results, includes place_id for detail lookups), then
// falls back to Photon (more lenient rate limits, same OSM data).
// Both are free and require no API keys.
func NewDefaultGeocoder() (*FallbackGeocoder, func()) {
	nom := NewNominatimGeocoder()
	photon := NewPhotonGeocoder()
	cleanup := func() {
		nom.Close()
	}
	return NewFallbackGeocoder(nom, photon), cleanup
}
