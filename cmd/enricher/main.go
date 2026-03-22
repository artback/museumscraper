package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"museum/internal/database"
	"museum/internal/enrich"
	"museum/internal/env"
	"museum/internal/keys"
	"museum/internal/models"
	"museum/internal/repository"
	"museum/internal/service"
	"museum/internal/storage"
	"museum/pkg/graceful"
	"museum/pkg/kafkaclient"
	"museum/pkg/location"
)

const batchSize = 50

func main() {
	ctx, cancel := graceful.Context(context.Background())
	defer cancel()

	env.LoadEnv()

	// --- Kafka setup ---
	kafkaBroker := env.MustGetEnv("KAFKA_BROKER_LOCAL")
	kafkaTopic := env.MustGetEnv("KAFKA_TOPIC")
	kafkaGroupID := env.MustGetEnv("KAFKA_GROUP_ID")

	slog.Info("connecting to Kafka", "broker", kafkaBroker, "topic", kafkaTopic, "group", kafkaGroupID)

	consumer, err := kafkaclient.NewKafkaConsumer(kafkaTopic, kafkaGroupID, kafkaBroker)
	if err != nil {
		slog.Error("failed to create kafka consumer", "error", err)
		return
	}
	defer consumer.Stop()

	// --- PostgreSQL setup ---
	dbCfg := database.ConfigFromEnv()
	pool, err := database.NewPool(ctx, dbCfg)
	if err != nil {
		slog.Error("failed to connect to PostgreSQL", "error", err)
		return
	}
	defer pool.Close()

	migrationsDir, ok := os.DirFS(".").(database.MigrationsFS)
	if !ok {
		slog.Error("os.DirFS does not satisfy MigrationsFS interface")
		return
	}
	if err := database.RunMigrations(ctx, pool, migrationsDir, "up"); err != nil {
		slog.Error("failed to run migrations", "error", err)
		return
	}

	repo := repository.NewMuseumRepository(pool)

	// --- S3 / iterator setup ---
	s3Service, err := storage.NewS3Service(keys.Museum)
	if err != nil {
		slog.Error("failed to create S3 service", "error", err)
		return
	}

	consumer.StartConsuming(ctx)
	iterator := service.NewIterator(consumer, func(ctx context.Context, bucket, key string) (*models.Museum, error) {
		return s3Service.GetObject(ctx, bucket, key)
	})

	// --- Pipeline ---
	pipeline := enrich.NewPipeline(
		enrich.NewStage(StepLocation),
		enrich.NewStage(StepLocationDetails),
	).WithWorkers(4)

	in := initializePipelineItems(iterator.Objects(ctx))
	out := pipeline.Process(ctx, in)

	// --- Consume enriched items and batch-upsert into PostgreSQL ---
	buffer := make([]repository.Museum, 0, batchSize)

	flush := func() {
		if len(buffer) == 0 {
			return
		}
		if _, err := repo.UpsertMuseumBatch(ctx, buffer); err != nil {
			slog.Error("failed to upsert museum batch", "error", err, "count", len(buffer))
		} else {
			slog.Info("upserted museum batch", "count", len(buffer))
		}
		buffer = buffer[:0]
	}

	for item := range out {
		buffer = append(buffer, toRepoMuseum(item.Object))
		if len(buffer) >= batchSize {
			flush()
		}
	}
	// Flush remaining items on shutdown.
	flush()

	slog.Info("enricher finished, application exiting")
}

// toRepoMuseum converts a models.Museum to a repository.Museum for storage.
func toRepoMuseum(m *models.Museum) repository.Museum {
	rm := repository.Museum{
		Name:    m.Name,
		Country: m.Country,
	}
	if m.City != "" {
		rm.City = &m.City
	}
	if m.State != "" {
		rm.State = &m.State
	}
	if m.Address != "" {
		rm.Address = &m.Address
	}
	if m.Lat != 0 {
		rm.Lat = &m.Lat
	}
	if m.Lon != 0 {
		rm.Lon = &m.Lon
	}
	if m.OsmID != 0 {
		rm.OsmID = &m.OsmID
	}
	if m.OsmType != "" {
		rm.OsmType = &m.OsmType
	}
	if m.Category != "" {
		rm.Category = &m.Category
	}
	if m.MuseumType != "" {
		rm.MuseumType = &m.MuseumType
	}
	if m.WikipediaURL != "" {
		rm.WikipediaURL = &m.WikipediaURL
	}
	if m.Website != "" {
		rm.Website = &m.Website
	}
	if len(m.RawTags) > 0 {
		rm.RawTags = m.RawTags
	}
	return rm
}

// PipelineItem wraps a museum object flowing through the enrichment pipeline.
type PipelineItem struct {
	Object *models.Museum
}

// NewPipelineItem creates a new PipelineItem wrapping the given museum.
func NewPipelineItem(obj *models.Museum) *PipelineItem {
	return &PipelineItem{Object: obj}
}

// StepLocation enriches a museum with geocoding data from Nominatim.
func StepLocation(ctx context.Context, item *PipelineItem) error {
	m := item.Object
	locationTerm := fmt.Sprintf("%s %s", m.Name, m.Country)
	loc, err := location.Geocode(ctx, locationTerm)
	if err != nil {
		return err
	}

	if lat, err := strconv.ParseFloat(loc.Lat, 64); err == nil {
		m.Lat = lat
	}
	if lon, err := strconv.ParseFloat(loc.Lon, 64); err == nil {
		m.Lon = lon
	}
	m.OsmID = loc.OsmID
	m.OsmType = loc.OsmType

	// Resolve city from the address hierarchy: city > town > village.
	city := loc.Address.City
	if city == "" {
		city = loc.Address.Town
	}
	if city == "" {
		city = loc.Address.Village
	}
	m.City = city
	m.Address = loc.DisplayName

	return nil
}

// StepLocationDetails enriches a museum with detailed place data from Nominatim.
func StepLocationDetails(ctx context.Context, item *PipelineItem) error {
	m := item.Object
	if m.OsmType == "" || m.OsmID == 0 {
		return nil
	}

	details, err := location.PlaceDetails(ctx, m.OsmType, m.OsmID)
	if err != nil {
		return err
	}

	m.Category = details.Category
	m.MuseumType = details.Type
	m.WikipediaURL = details.CalculatedWikipedia

	if details.ExtraTags != nil {
		if website, ok := details.ExtraTags["website"]; ok {
			m.Website = website
		}
		m.RawTags = details.ExtraTags
	}

	return nil
}

func initializePipelineItems(in <-chan *service.FetchedObject[*models.Museum]) <-chan *PipelineItem {
	out := make(chan *PipelineItem)

	go func() {
		defer close(out)

		for obj := range in {
			item := NewPipelineItem(obj.Data)
			out <- item
		}
	}()

	return out
}
