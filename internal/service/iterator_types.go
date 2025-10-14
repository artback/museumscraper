package service

import (
	"context"
	"github.com/minio/minio-go/v7/pkg/notification"
	"github.com/segmentio/kafka-go"
)

// MessageIterator defines the contract for consuming messages from a Kafka topic.
// It is used by the service's Iterator to abstract away the details of the
// underlying Kafka consumer.
//
// Implementations are responsible for the lifecycle of the consumer connection.
type MessageIterator interface {
	// Messages returns a receive-only channel of Kafka messages. The channel
	// is closed by the implementation when the consumer is stopped or the
	// underlying source is exhausted.
	Messages() <-chan kafka.Message

	// CommitOffset acknowledges that a message has been successfully processed.
	// The provided context should be used for cancellation and deadlines.
	// Implementations can choose to make this a no-op if using auto-commit
	// mechanisms. An error is returned if the commit fails.
	CommitOffset(ctx context.Context, msg kafka.Message) error
}

// LoaderFunc defines a function signature for loading and decoding an object
// of type T from an object store like S3 or MinIO.
//
// It is called by the service's Iterator for each storage event, using the
// event's bucket and key to fetch the corresponding data. Implementations should
// be side-effect-free (read-only) and must honor the provided context for
// cancellation and timeouts.
type LoaderFunc[T any] func(ctx context.Context, bucket, key string) (T, error)

// FetchedObject represents an object that has been loaded from the object store
// in response to a notification event. It pairs the decoded object data with the
// original event that triggered its retrieval.
//
// The generic type T is the type of the decoded data, which can be a value or
// a pointer type (e.g., *models.Museum).
type FetchedObject[T any] struct {
	// Data is the decoded object data, loaded from the object store.
	Data T
	// Event is the original MinIO/S3 notification event that triggered the fetch.
	Event notification.Event
}
