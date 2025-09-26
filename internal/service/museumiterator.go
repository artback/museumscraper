package service

import (
	"context"
	"encoding/json"
	"github.com/minio/minio-go/v7/pkg/notification"
	"log"
	"museum/models"
	"museum/pkg/kafkaclient"
)

type S3Service interface {
	GetMuseumObject(ctx context.Context, bucketName string, objectKey string) (*models.Museum, error)
}
type MuseumIterator struct {
	msgIterator *kafkaclient.Iterator
	s3Service   S3Service
	ctx         context.Context
}

// Constructor
func NewMuseumIterator(ctx context.Context, consumer *kafkaclient.KafkaConsumer, s3Service S3Service) *MuseumIterator {
	return &MuseumIterator{
		msgIterator: consumer.NewIterator(),
		s3Service:   s3Service,
		ctx:         ctx,
	}
}

// Objects Channel to iterate enriched museum objects
func (it *MuseumIterator) Objects() <-chan *MuseumObject {
	out := make(chan *MuseumObject)

	go func() {
		defer close(out)

		for msg := range it.msgIterator.Messages() {
			var event notification.Event
			if err := json.Unmarshal(msg.Value, &event); err != nil {
				log.Printf("Error unmarshalling JSON: %v", err)
				continue // skip bad messages
			}

			raw, err := it.s3Service.GetMuseumObject(it.ctx, event.S3.Bucket.Name, event.S3.Object.Key)
			if err != nil {
				log.Printf("Error getting museum object: %v", err)
				continue
			}

			// wrap the raw object if needed
			museumObj := &MuseumObject{
				Data:  raw,
				Event: event,
			}

			out <- museumObj

			// Commit offset after successful processing
			if err := it.msgIterator.CommitOffset(it.ctx, msg); err != nil {
				log.Printf("Failed to commit offset for message: %v", err)
			}
		}
	}()

	return out
}
