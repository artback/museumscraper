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

	consumer.StartConsuming(ctx)
	iterator := service.NewIterator(consumer, func(ctx context.Context, bucket, key string) (*models.Museum, error) {
		return s3Service.GetObject(ctx, bucket, key)
	})

	// Use multiple workers to process items concurrently through the pipeline.
	// The rate limiter in the location package ensures Nominatim's 1 req/sec
	// limit is respected regardless of worker count. Multiple workers help
	// overlap I/O waits (S3 reads, Kafka commits) with API calls.
	pipeline := enrich.NewPipeline(
		enrich.NewStage(StepLocation),
		enrich.NewStage(StepLocationDetails),
	).WithWorkers(4)

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

func StepLocation(ctx context.Context, item *PipelineItem[*models.Museum]) error {
	locationTerm := fmt.Sprintf("%s %s", item.Object.Name, item.Object.Country)
	loc, err := location.Geocode(ctx, locationTerm)
	if err != nil {
		return err
	}
	return mergeIntoResults(item.Results, loc)
}

func StepLocationDetails(ctx context.Context, item *PipelineItem[*models.Museum]) error {
	if item.Results["osmType"] == nil || item.Results["osmID"] == nil {
		return nil
	}
	osmType, ok := item.Results["osmType"].(string)
	if !ok {
		return fmt.Errorf("osmType is not a string: %v", item.Results["osmType"])
	}
	// JSON unmarshals numbers as float64; safely convert to int64.
	var osmID int64
	switch v := item.Results["osmID"].(type) {
	case float64:
		osmID = int64(v)
	case int64:
		osmID = v
	case int:
		osmID = int64(v)
	default:
		return fmt.Errorf("osmID has unexpected type %T: %v", v, v)
	}

	details, err := location.PlaceDetails(ctx, osmType, osmID)
	if err != nil {
		return err
	}
	return mergeIntoResults(item.Results, details)
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
