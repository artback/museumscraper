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

	// Create a Nominatim detailer for place details (only Nominatim
	// provides the /details endpoint).
	detailer := location.NewNominatimGeocoder()
	defer detailer.Close()

	consumer.StartConsuming(ctx)
	iterator := service.NewIterator(consumer, func(ctx context.Context, bucket, key string) (*models.Museum, error) {
		return s3Service.GetObject(ctx, bucket, key)
	})

	pipeline := enrich.NewPipeline(
		enrich.NewStage(stepLocation(geocoder)),
		enrich.NewStage(stepLocationDetails(detailer)),
	)
	it := initializePipelineItems(iterator.Objects(ctx))
	pipeline.Process(ctx, it)

	log.Println("Main method finished, application exiting.")
}

type PipelineItem[T any] struct {
	Object  T
	Results map[string]any
}

func NewPipelineItem[T any](obj T) *PipelineItem[T] {
	return &PipelineItem[T]{
		Object:  obj,
		Results: make(map[string]any),
	}
}

// mergeIntoResults flattens a source struct into a map and merges its keys
// into the target map. It uses JSON marshalling as an intermediary step.
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

// stepLocation returns an enrichment step that geocodes the museum using the
// provided Geocoder implementation.
func stepLocation(geocoder location.Geocoder) enrich.Step[PipelineItem[*models.Museum]] {
	return func(ctx context.Context, item *PipelineItem[*models.Museum]) error {
		locationTerm := fmt.Sprintf("%s %s", item.Object.Name, item.Object.Country)
		result, err := geocoder.Geocode(ctx, locationTerm)
		if err != nil {
			return err
		}
		return mergeIntoResults(item.Results, result)
	}
}

// stepLocationDetails returns an enrichment step that fetches place details
// using the provided PlaceDetailer.
func stepLocationDetails(detailer location.PlaceDetailer) enrich.Step[PipelineItem[*models.Museum]] {
	return func(ctx context.Context, item *PipelineItem[*models.Museum]) error {
		if item.Results["osm_type"] == nil || item.Results["osm_id"] == nil {
			return nil
		}

		osmType, ok := item.Results["osm_type"].(string)
		if !ok {
			return fmt.Errorf("osm_type is not a string: %v", item.Results["osm_type"])
		}

		// JSON unmarshals numbers as float64
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
		return mergeIntoResults(item.Results, details)
	}
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
