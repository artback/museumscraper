// Package service contains helpers used by application services.
// In particular, it provides an Iterator that consumes storage events from a
// message source (e.g., Kafka via pkg/kafkaclient) and loads the referenced
// objects from S3/MinIO using a pluggable LoaderFunc.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7/pkg/notification"
	"github.com/segmentio/kafka-go"
)

// errEmptyKey means a notification referenced an object with a blank key.
var errEmptyKey = errors.New("empty object key")

// Iterator consumes messages from a MessageIterator, interprets each message as
// a MinIO/S3 notification, loads the referenced object via LoaderFunc, and
// yields FetchedObject items on a channel. It is generic over the loaded item
// type T.
//
// The Iterator does not manage the lifecycle of the underlying message source;
// callers should start and stop their consumer outside and pass in an
// implementation of MessageIterator.
type Iterator[T any] struct {
	msgIterator MessageIterator
	loader      LoaderFunc[T]
}

// NewIterator constructs an Iterator for the provided message source and object
// loader.
func NewIterator[T any](iterator MessageIterator, loader LoaderFunc[T]) *Iterator[T] {
	return &Iterator[T]{
		msgIterator: iterator,
		loader:      loader,
	}
}

// Objects starts a goroutine that deserialises each message as a MinIO
// notification, loads the referenced object, emits it, and then commits the
// message offset.
//
// Every per-message failure is logged and skipped rather than being fatal: a
// single malformed event, a MinIO keep-alive with no records, or an object that
// has since been deleted must not take the consumer down. The output channel is
// closed when the underlying Messages() channel closes or ctx is cancelled.
func (it *Iterator[T]) Objects(ctx context.Context) <-chan *FetchedObject[T] {
	out := make(chan *FetchedObject[T])

	go func() {
		defer close(out)

		for msg := range it.msgIterator.Messages() {
			if ctx.Err() != nil {
				return
			}

			var event notification.Info
			if err := json.Unmarshal(msg.Value, &event); err != nil {
				log.Printf("Skipping message at offset %d: invalid JSON: %v", msg.Offset, err)
				continue
			}

			// MinIO sends a records-free test event when a notification target
			// is configured, and heartbeats carry no records either.
			if len(event.Records) == 0 {
				log.Printf("Skipping message at offset %d: no S3 records", msg.Offset)
				it.commit(ctx, msg)
				continue
			}

			// The offset is committed only once every object carried by this
			// message has been acknowledged by the consumer. Committing on
			// hand-off instead would mark a museum done the moment the pipeline
			// received it, and an interrupt part-way through enrichment would
			// lose it for good: the offset is past it, and the parser will not
			// re-emit an event for an object that already exists.
			pending := newAcker(len(event.Records), func() { it.commit(ctx, msg) })

			for _, record := range event.Records {
				key, err := objectKey(record.S3.Object.Key)
				if err != nil {
					log.Printf("Skipping record at offset %d: bad object key %q: %v", msg.Offset, record.S3.Object.Key, err)
					pending.done()
					continue
				}

				data, err := it.loader(ctx, record.S3.Bucket.Name, key)
				if err != nil {
					log.Printf("Skipping %s/%s: %v", record.S3.Bucket.Name, key, err)
					pending.done()
					continue
				}

				select {
				case out <- &FetchedObject[T]{Data: data, Event: event, Ack: pending.done}:
				case <-ctx.Done():
					return
				}
			}

		}
	}()

	return out
}

// acker counts outstanding acknowledgements for one message and runs commit
// once the last one arrives. Repeated calls beyond the count are ignored, so a
// consumer that acknowledges twice cannot commit early.
type acker struct {
	mu        sync.Mutex
	remaining int
	commit    func()
}

func newAcker(count int, commit func()) *acker {
	if count <= 0 {
		commit()
		return &acker{}
	}
	return &acker{remaining: count, commit: commit}
}

// done records one acknowledgement.
func (a *acker) done() {
	a.mu.Lock()
	if a.remaining <= 0 {
		a.mu.Unlock()
		return
	}
	a.remaining--
	last := a.remaining == 0
	a.mu.Unlock()

	if last && a.commit != nil {
		a.commit()
	}
}

// commit acknowledges a message, logging rather than propagating failures: a
// failed commit means the message is redelivered, which is recoverable.
func (it *Iterator[T]) commit(ctx context.Context, msg kafka.Message) {
	if err := it.msgIterator.CommitOffset(ctx, msg); err != nil {
		log.Printf("Failed to commit offset %d: %v", msg.Offset, err)
	}
}

// objectKey decodes the URL-escaped key MinIO puts in its notifications. Keys
// arrive with spaces encoded as "+" and other characters percent-encoded.
func objectKey(raw string) (string, error) {
	key, err := url.QueryUnescape(raw)
	if err != nil {
		// Fall back to path semantics, which do not treat "+" specially, in
		// case the key legitimately contains one.
		if unescaped, pathErr := url.PathUnescape(raw); pathErr == nil {
			return unescaped, nil
		}
		return "", err
	}
	if strings.TrimSpace(key) == "" {
		return "", errEmptyKey
	}
	return key, nil
}
