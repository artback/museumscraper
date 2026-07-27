package command

import (
	"context"
	"fmt"
	"log"
	"os"

	"museum/internal/enrich"
	"museum/internal/env"
	"museum/internal/keys"
	"museum/internal/models"
	"museum/internal/service"
	"museum/internal/storage"
	"museum/pkg/graceful"
	"museum/pkg/kafkaclient"
)

// enrichCommand consumes storage events and enriches the museums they name.
func enrichCommand() Command {
	return Command{
		Name:    "enrich",
		Summary: "Consume storage events and enrich museums with geocoding",
		Usage:   "",
		Run:     runEnrich,
	}
}

// museumItem is the concrete item type flowing through the enrichment pipeline.
type museumItem = enrich.Item[*models.Museum]

func runEnrich(ctx context.Context, args []string) error {
	fs := newFlagSet("enrich", "", os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("enrich", fs.Args()); err != nil {
		return err
	}

	rawStore, bucket, err := museumStore()
	if err != nil {
		return err
	}

	broker, err := env.LookupEnv("KAFKA_BROKER_LOCAL")
	if err != nil {
		return err
	}
	topic, err := env.LookupEnv("KAFKA_TOPIC")
	if err != nil {
		return err
	}
	group, err := env.LookupEnv("KAFKA_GROUP_ID")
	if err != nil {
		return err
	}

	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	log.Printf("Connecting to Kafka broker %s, topic %s, group %s", broker, topic, group)
	consumer, err := kafkaclient.NewKafkaConsumer(topic, group, broker)
	if err != nil {
		return fmt.Errorf("create kafka consumer: %w", err)
	}
	defer consumer.Stop()

	enrichedStore, err := storage.NewS3Service(keys.EnrichedMuseum)
	if err != nil {
		return err
	}

	consumer.StartConsuming(ctx)

	iterator := service.NewIterator(consumer, func(ctx context.Context, b, key string) (*models.Museum, error) {
		return rawStore.GetObject(ctx, b, key)
	})

	sink := &s3Sink{store: enrichedStore, bucket: bucket}
	pipeline := enrich.NewPipeline(
		enrich.NewStage(StepLocation),
		enrich.NewStage(StepLocationDetails),
		enrich.NewStage(sink.Store),
	)

	processed := pipeline.Process(ctx, pipelineItems(iterator.Objects(ctx)))
	log.Printf("Enricher exiting after processing %d museums", processed)
	return nil
}

// pipelineItems adapts the stream of objects fetched from storage into pipeline
// items. The goroutine ends when in is closed, which the iterator guarantees.
//
// Each item carries its acknowledgement forward, so the Kafka offset advances
// only once the museum has been through every stage and written back. Advancing
// it earlier would drop museums whose enrichment was interrupted: the crawl does
// not re-emit an event for an object that already exists, so nothing would ever
// bring them back.
func pipelineItems(in <-chan *service.FetchedObject[*models.Museum]) <-chan *museumItem {
	out := make(chan *museumItem)

	go func() {
		defer close(out)
		for obj := range in {
			item := enrich.NewItem(obj.Data)
			item.OnDone = obj.Ack
			out <- item
		}
	}()

	return out
}
