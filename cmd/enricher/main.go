package main

import (
	"context"
	"log"
	"museum/internal/enrich"
	"museum/internal/env"
	"museum/internal/keys"
	"museum/internal/models"
	"museum/internal/service"
	"museum/internal/storage"
	"museum/pkg/graceful"
	"museum/pkg/kafkaclient"
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
