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

	// 2. Initialize the Kafka reader with your broker and topic details from environment variables.
	// All three of these environment variables are now mandatory.
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

	pipeline := enrich.NewPipeline(
		enrich.NewStage(StepLocation),
		enrich.NewStage(StepLocationDetails),
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
	// Marshal the source struct (e.g., location data) into JSON bytes.
	jsonData, err := json.Marshal(source)
	if err != nil {
		return fmt.Errorf("failed to marshal source data: %w", err)
	}

	// Unmarshal the JSON into a temporary map.
	var sourceMap map[string]any
	if err := json.Unmarshal(jsonData, &sourceMap); err != nil {
		return fmt.Errorf("failed to unmarshal source data into map: %w", err)
	}

	// Copy all key-value pairs from the temporary map to the target results.
	for key, value := range sourceMap {
		target[key] = value
	}

	return nil
}

func StepLocation(_ context.Context, item *PipelineItem[*models.Museum]) error {
	locationTerm := fmt.Sprintf("%s %s", item.Object.Name, item.Object.Country)
	loc, err := location.Geocode(locationTerm)
	if err != nil {
		return err
	}
	return mergeIntoResults(item.Results, loc)
}

func StepLocationDetails(_ context.Context, item *PipelineItem[*models.Museum]) error {
	fmt.Println(item.Results)
	if item.Results["osmType"] == nil || item.Results["osmID"] == nil {
		return nil
	}
	osmType := item.Results["osmType"].(string)
	osmID := item.Results["osmID"].(int)
	details, err := location.PlaceDetails(osmType, osmID)
	fmt.Println(details)
	if err != nil {
		return err
	}
	err = mergeIntoResults(item.Results, details)
	return err
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
