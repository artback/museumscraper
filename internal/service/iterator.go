// Package service contains helpers used by application services.
// In particular, it provides an Iterator that consumes storage events from a
// message source (e.g., Kafka via pkg/kafkaclient) and loads the referenced
// objects from S3/MinIO using a pluggable LoaderFunc.
package service

import (
	"context"
	"encoding/json"
	"github.com/minio/minio-go/v7/pkg/notification"
	"log"
	"net/url"
)

// Iterator consumes messages from a MessageIterator, interprets each message
// as a MinIO/S3 notification.Event, loads the referenced object via LoaderFunc,
// and yields FetchedObject items on a channel. It is generic over the loaded
// item type T.
//
// The Iterator does not manage the lifecycle of the underlying message source;
// callers should start/stop their consumer outside and pass in an implementation
// of MessageIterator.
type Iterator[T any] struct {
	msgIterator MessageIterator
	loader      LoaderFunc[T]
}

// NewIterator constructs an Iterator for the provided message source and
// object loader. The iterator is stateless and safe to use from a single
// goroutine; it spawns a goroutine per Objects() call to stream results.
func NewIterator[T any](iterator MessageIterator, loader LoaderFunc[T]) *Iterator[T] {
	return &Iterator[T]{
		msgIterator: iterator,
		loader:      loader,
	}
}

// Objects starts a goroutine that:
//  1. Receives messages from the underlying MessageIterator
//  2. Deserializes each message as a MinIO notification.Event
//  3. Loads the referenced object using the provided LoaderFunc
//  4. Emits a FetchedObject[T] on the returned channel
//  5. Attempts to commit the message offset after successful load
//
// Errors during JSON deserialization or object loading are logged and the
// message is skipped; processing continues for subsequent messages. The output
// channel is closed when the underlying Messages() channel is closed.
func (it *Iterator[T]) Objects(ctx context.Context) <-chan *FetchedObject[T] {
	out := make(chan *FetchedObject[T])
	go func() {
		defer close(out)

		for msg := range it.msgIterator.Messages() {
			var event notification.Info
			if err := json.Unmarshal(msg.Value, &event); err != nil {
				log.Printf("Error unmarshalling JSON: %v", err)
				continue
			}
			s3 := event.Records[0].S3
			objectKey, err := url.QueryUnescape(s3.Object.Key)
			if err != nil {
				// Handle potential error, though unlikely for this simple case
				log.Fatalf("Error decoding string: %v", err)
			}
			data, err := it.loader(ctx, s3.Bucket.Name, objectKey)
			if err != nil {
				log.Printf("Error loading object: %v", err)
				continue
			}

			out <- &FetchedObject[T]{Data: data, Event: event}

			if err := it.msgIterator.CommitOffset(ctx, msg); err != nil {
				log.Printf("Failed to commit offset: %v", err)
			}
		}
	}()
	return out
}
