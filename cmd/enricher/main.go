package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"museum/internal/enrich"
	"museum/internal/env"
	"museum/internal/keys"
	"museum/internal/models"
	"museum/internal/service"
	"museum/internal/storage"
	"museum/pkg/graceful"
	"museum/pkg/kafkaclient"
	"museum/pkg/location"
	"museum/pkg/scraper"
	"museum/pkg/wikidata"
	"time"
)

func main() {
	ctx, cancel := graceful.Context(context.Background())
	defer cancel()

	env.LoadEnv()

	kafkaBroker := env.MustGetEnv("KAFKA_BROKER_LOCAL")
	kafkaTopic := env.MustGetEnv("KAFKA_TOPIC")
	kafkaGroupID := env.MustGetEnv("KAFKA_GROUP_ID")

	log.Printf("Connecting to Kafka broker: %s on topic: %s with group ID: %s", kafkaBroker, kafkaTopic, kafkaGroupID)

	consumer, err := kafkaclient.NewKafkaConsumer(kafkaTopic, kafkaGroupID, kafkaBroker)
	defer consumer.Stop()
	if err != nil {
		log.Fatalf("Failed to create kafka consumer %v", err)
	}

	s3Service, err := storage.NewS3Service(keys.Museum)
	if err != nil {
		log.Fatal(err)
	}

	// Create a reliable geocoder: Nominatim (rate-limited, detailed) with
	// Photon fallback (higher throughput, same OSM data). Both are free
	// and require no API keys.
	geocoder, cleanupGeocoder := location.NewDefaultGeocoder()
	defer cleanupGeocoder()

	// Nominatim detailer for place details (/details endpoint).
	detailer := location.NewNominatimGeocoder()
	defer detailer.Close()

	// Wikidata client for exhibition and collection data (free, no API key).
	wikidataClient := wikidata.NewClient()

	// Museum website scraper for exhibitions, prices, and schedules.
	museumScraper := scraper.NewMuseumScraper(2 * time.Second)
	defer museumScraper.Close()

	consumer.StartConsuming(ctx)
	iterator := service.NewIterator(consumer, func(ctx context.Context, bucket, key string) (*models.Museum, error) {
		return s3Service.GetObject(ctx, bucket, key)
	})

	// Pipeline stages:
	// 1. Geocode the museum name → get coordinates, OSM class/type
	// 2. Filter non-museums + fetch place details from Nominatim
	// 3. Extract details from extratags + Wikidata enrichment (parallel)
	// 4. Scrape museum website for exhibitions, prices, schedules
	pipeline := enrich.NewPipeline(
		enrich.NewStage(stepLocation(geocoder)),
		enrich.NewStage(stepFilterMuseum(), stepLocationDetails(detailer)),
		enrich.NewStage(stepExtractDetails(), stepWikidataEnrich(wikidataClient)),
		enrich.NewStage(stepScrapeWebsite(museumScraper)),
	)
	it := initializePipelineItems(iterator.Objects(ctx))
	pipeline.Process(ctx, it)

	log.Println("Main method finished, application exiting.")
}

// PipelineItem carries a museum through the enrichment pipeline.
// Skip indicates the item was filtered out (e.g. not a museum).
type PipelineItem[T any] struct {
	Object  T
	Results map[string]any
	Skip    bool
}

func NewPipelineItem[T any](obj T) *PipelineItem[T] {
	return &PipelineItem[T]{
		Object:  obj,
		Results: make(map[string]any),
	}
}

func mergeIntoResults(target map[string]any, source any) error {
	jsonData, err := json.Marshal(source)
	if err != nil {
		return fmt.Errorf("failed to marshal source data: %w", err)
	}

	var sourceMap map[string]any
	if err := json.Unmarshal(jsonData, &sourceMap); err != nil {
		return fmt.Errorf("failed to unmarshal source data into map: %w", err)
	}

	for key, value := range sourceMap {
		target[key] = value
	}

	return nil
}

// stepLocation geocodes the museum name to get coordinates and OSM metadata.
func stepLocation(geocoder location.Geocoder) enrich.Step[PipelineItem[*models.Museum]] {
	return func(ctx context.Context, item *PipelineItem[*models.Museum]) error {
		locationTerm := fmt.Sprintf("%s %s", item.Object.Name, item.Object.Country)
		result, err := geocoder.Geocode(ctx, locationTerm)
		if err != nil {
			return err
		}
		// Store the GeoResult directly for use in filter step
		item.Results["_geo_result"] = result
		return mergeIntoResults(item.Results, result)
	}
}

// stepFilterMuseum checks if the geocoded result is actually a museum.
// If not, it sets Skip=true so subsequent steps can short-circuit.
func stepFilterMuseum() enrich.Step[PipelineItem[*models.Museum]] {
	return func(_ context.Context, item *PipelineItem[*models.Museum]) error {
		if item.Skip {
			return nil
		}

		geo, ok := item.Results["_geo_result"].(*location.GeoResult)
		if !ok {
			// No geo result — can't verify, skip this item
			item.Skip = true
			log.Printf("Skipping %q: no geocoding result available", item.Object.Name)
			return nil
		}

		if !geo.IsMuseum() {
			item.Skip = true
			log.Printf("Filtered out %q: OSM class=%s type=%s (not a museum)", item.Object.Name, geo.Class, geo.Type)
			return nil
		}

		return nil
	}
}

// stepLocationDetails fetches extended place details from Nominatim.
func stepLocationDetails(detailer location.PlaceDetailer) enrich.Step[PipelineItem[*models.Museum]] {
	return func(ctx context.Context, item *PipelineItem[*models.Museum]) error {
		if item.Skip {
			return nil
		}
		if item.Results["osm_type"] == nil || item.Results["osm_id"] == nil {
			return nil
		}

		osmType, ok := item.Results["osm_type"].(string)
		if !ok {
			return fmt.Errorf("osm_type is not a string: %v", item.Results["osm_type"])
		}

		var osmID int64
		switch v := item.Results["osm_id"].(type) {
		case float64:
			osmID = int64(v)
		case int64:
			osmID = v
		case int:
			osmID = int64(v)
		default:
			return fmt.Errorf("osm_id has unexpected type %T: %v", v, v)
		}

		details, err := detailer.PlaceDetails(ctx, osmType, osmID)
		if err != nil {
			return err
		}
		item.Results["_extratags"] = details.ExtraTags
		return mergeIntoResults(item.Results, details)
	}
}

// stepExtractDetails extracts operational museum details (opening hours,
// admission, website, phone) from Nominatim's extratags.
func stepExtractDetails() enrich.Step[PipelineItem[*models.Museum]] {
	return func(_ context.Context, item *PipelineItem[*models.Museum]) error {
		if item.Skip {
			return nil
		}

		extratags, ok := item.Results["_extratags"].(map[string]string)
		if !ok {
			return nil
		}

		details := wikidata.ExtractMuseumDetails(extratags)
		if details != nil {
			item.Results["museum_details"] = details
			log.Printf("Extracted details for %q: hours=%q, admission=%q",
				item.Object.Name, details.OpeningHours, details.Admission)
		}
		return nil
	}
}

// stepWikidataEnrich queries Wikidata for exhibition schedules, collections,
// and additional metadata. Uses the free SPARQL endpoint (no API key).
func stepWikidataEnrich(client *wikidata.Client) enrich.Step[PipelineItem[*models.Museum]] {
	return func(ctx context.Context, item *PipelineItem[*models.Museum]) error {
		if item.Skip {
			return nil
		}

		info, err := client.FetchMuseumInfo(ctx, item.Object.Name, item.Object.Country)
		if err != nil {
			// Wikidata enrichment is best-effort; log and continue
			log.Printf("Wikidata enrichment failed for %q: %v", item.Object.Name, err)
			return nil
		}

		if len(info.Exhibitions) > 0 {
			log.Printf("Found %d exhibitions for %q", len(info.Exhibitions), item.Object.Name)
		}

		return mergeIntoResults(item.Results, info)
	}
}

// stepScrapeWebsite fetches the museum's website and extracts exhibitions,
// pricing, opening hours, and other data using JSON-LD, meta tags, and
// text pattern matching.
func stepScrapeWebsite(s *scraper.MuseumScraper) enrich.Step[PipelineItem[*models.Museum]] {
	return func(ctx context.Context, item *PipelineItem[*models.Museum]) error {
		if item.Skip {
			return nil
		}

		// Resolve the website URL from multiple sources (in priority order)
		websiteURL := resolveWebsiteURL(item.Results)
		if websiteURL == "" {
			log.Printf("No website URL found for %q, skipping web scrape", item.Object.Name)
			return nil
		}

		webData, err := s.ScrapeMuseum(ctx, websiteURL)
		if err != nil {
			// Web scraping is best-effort; log and continue
			log.Printf("Web scrape failed for %q (%s): %v", item.Object.Name, websiteURL, err)
			return nil
		}

		return mergeIntoResults(item.Results, webData)
	}
}

// resolveWebsiteURL finds the best website URL from the accumulated results.
// It checks multiple sources: extratags, Wikidata, and the geo result.
func resolveWebsiteURL(results map[string]any) string {
	// Check museum_details first (from Nominatim extratags)
	if details, ok := results["museum_details"]; ok {
		if md, ok := details.(*wikidata.MuseumDetails); ok && md.Website != "" {
			return md.Website
		}
	}

	// Check Wikidata website
	if website, ok := results["website"].(string); ok && website != "" {
		return website
	}

	// Check extratags directly
	if extratags, ok := results["_extratags"].(map[string]string); ok {
		for _, key := range []string{"website", "url", "contact:website"} {
			if v := extratags[key]; v != "" {
				return v
			}
		}
	}

	return ""
}

func initializePipelineItems(in <-chan *service.FetchedObject[*models.Museum]) <-chan *PipelineItem[*models.Museum] {
	out := make(chan *PipelineItem[*models.Museum])

	go func() {
		defer close(out)

		for obj := range in {
			item := NewPipelineItem(obj.Data)
			out <- item
		}
	}()

	return out
}
