package main

import (
	"context"
	"fmt"
	"github.com/joho/godotenv"
	"log"
	"museum/internal/keys"
	"museum/internal/models"
	"museum/internal/service"
	"museum/internal/storage"
	"museum/pkg/graceful"
	"museum/pkg/kafkaclient"
	"os"
)

func main() {
	// Load environment variables from a .env file.
	// This is typically used in a development environment.
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, assuming environment variables are set directly.")
	}
	ctx, cancel := graceful.Context(context.Background())
	defer cancel()

	// 2. Initialize the Kafka reader with your broker and topic details from environment variables.
	// All three of these environment variables are now mandatory.
	kafkaBroker, ok := os.LookupEnv("KAFKA_BROKER_LOCAL")
	if !ok {
		log.Fatalf("Environment variable KAFKA_BROKER not set")
	}
	kafkaTopic, ok := os.LookupEnv("KAFKA_TOPIC")
	if !ok {
		log.Fatalf("Environment variable KAFKA_TOPIC not set")
	}
	kafkaGroupID, ok := os.LookupEnv("KAFKA_GROUP_ID")
	if !ok {
		log.Fatalf("Environment variable KAFKA_GROUP_ID not set")
	}

	log.Printf("Connecting to Kafka broker: %s on topic: %s with group ID: %s", kafkaBroker, kafkaTopic, kafkaGroupID)

	consumer, err := kafkaclient.NewKafkaConsumer(kafkaTopic, kafkaGroupID, kafkaBroker)
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
	for obj := range iterator.Objects(ctx) {
		fmt.Println(obj.Data.Name)
	}

	consumer.Stop()
	log.Println("Main method finished, application exiting.")
}
