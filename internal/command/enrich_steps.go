package command

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"museum/internal/models"
	"museum/internal/storage"
	"museum/pkg/location"
)

// StepLocation geocodes the museum against Nominatim and merges the result into
// the item.
//
// The query is built from the museum's own metadata rather than its name alone:
// "Louvre" on its own matches places worldwide, while name + locality + country
// pins it down.
func StepLocation(ctx context.Context, item *museumItem) error {
	loc, err := location.Geocode(ctx, geocodeQuery(item.Object))
	if err != nil {
		if errors.Is(err, location.ErrNoResults) {
			// Not every museum is in OpenStreetMap; that is not a failure.
			item.Set("geocoded", false)
			return nil
		}
		return err
	}

	item.Set("geocoded", true)
	if locality := loc.Locality(); locality != "" {
		item.Set("locality", locality)
	}
	// The postal address is the part of the geocoder's answer worth keeping in
	// a shape a consumer can read, so it goes onto the museum rather than only
	// into the untyped result map.
	if !loc.Address.IsZero() && item.Object != nil {
		item.Object.Address = loc.Address
	}
	if website := loc.Website(); website != "" {
		item.Set("website", website)
	}
	return item.Merge(loc)
}

// geocodeQuery assembles the most specific search string the record supports.
func geocodeQuery(m *models.Museum) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{m.Name, m.Locality, m.Country} {
		part = strings.TrimSpace(part)
		if part != "" && part != "unknown" && !slicesContainsFold(parts, part) {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, ", ")
}

// slicesContainsFold reports whether values already holds s, ignoring case, so
// "Paris, Paris, France" does not get built.
func slicesContainsFold(values []string, s string) bool {
	for _, v := range values {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// StepLocationDetails fetches the full OpenStreetMap record for whatever
// StepLocation matched.
//
// The osm_type / osm_id keys are the JSON field names produced by merging a
// NominatimLocation, and osm_id arrives as a JSON number, so it is read through
// the item's typed accessors rather than asserted directly.
func StepLocationDetails(ctx context.Context, item *museumItem) error {
	osmType, hasType := item.String("osm_type")
	osmID, hasID := item.Int64("osm_id")
	if !hasType || !hasID {
		// Nothing was geocoded; there is nothing to look up.
		return nil
	}

	details, err := location.PlaceDetails(ctx, osmType, osmID)
	if err != nil {
		return err
	}
	return item.Merge(details)
}

// s3Sink writes finished items back to object storage.
type s3Sink struct {
	store  *storage.S3Service[models.EnrichedMuseum]
	bucket string
}

// Store is the pipeline's final step. It writes the museum plus everything the
// earlier steps accumulated to the enriched_data/ prefix, overwriting any
// previous version so that a re-run refreshes the record.
//
// Writing under a different prefix than the parser matters: MinIO's bucket
// notification is scoped to raw_data/, so these writes do not feed back into
// the topic the enricher is consuming.
func (s *s3Sink) Store(ctx context.Context, item *museumItem) error {
	if item.Object == nil {
		return fmt.Errorf("cannot store enriched record: museum is nil")
	}

	record := models.EnrichedMuseum{
		Museum: *item.Object,
		Data:   item.Results(),
	}

	// Prefer coordinates Wikipedia already supplied; fall back to the geocoder.
	if !record.Museum.HasCoordinates() {
		record.Museum.Latitude, record.Museum.Longitude = coordinatesFrom(record.Data)
	}
	if record.Museum.Locality == "" {
		if locality, ok := record.Data["locality"].(string); ok {
			record.Museum.Locality = locality
		}
	}

	if err := s.store.PutObject(ctx, s.bucket, record); err != nil {
		return err
	}

	log.Printf("Enriched %q (%s)", record.Museum.Name, record.Museum.Country)
	return nil
}

// coordinatesFrom reads Nominatim's lat/lon, which it returns as strings.
func coordinatesFrom(data map[string]any) (lat, lon float64) {
	parse := func(key string) float64 {
		s, ok := data[key].(string)
		if !ok {
			return 0
		}
		var f float64
		if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
			return 0
		}
		return f
	}
	return parse("lat"), parse("lon")
}
