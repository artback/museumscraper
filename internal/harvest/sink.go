package harvest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/artback/museumscraper/extract"
)

// Sink delivers validated output.
//
// Only passing runs ever reach one: the harvester checks the verdict before
// calling, so a sink does not have to be trusted to check it again. A suspect
// result is held and reported, never delivered, and that decision lives in one
// place rather than in every implementation.
type Sink interface {
	Publish(ctx context.Context, source extract.Source, run extract.Run, records []extract.Record) error
}

// Delivery is the payload every sink sends, and the object the pull interface
// serves.
type Delivery struct {
	// Source names what was extracted, and URL where from.
	Source string `json:"source"`
	URL    string `json:"url"`

	// Version is the artifact version that produced this, so a consumer
	// noticing a change in the data can find the change in the extractor.
	Version int `json:"artifact_version"`

	// ExtractedAt is when the run started.
	ExtractedAt time.Time `json:"extracted_at"`
	// Count is len(Records), carried separately so a consumer can check the
	// size of a delivery without decoding all of it.
	Count int `json:"count"`

	// Key identifies this delivery's content. It is a digest of the source and
	// the records, so two runs that extracted the same thing produce the same
	// key and a consumer that has seen it can drop the second without
	// comparing payloads.
	//
	// This is what makes delivery idempotent. A source scraped hourly whose
	// programme changes monthly sends the same key seven hundred times, and a
	// consumer keyed on it stores one copy.
	Key string `json:"key"`

	Records []extract.Record `json:"records"`
}

// NewDelivery builds a delivery and computes its key.
func NewDelivery(source extract.Source, run extract.Run, records []extract.Record) (Delivery, error) {
	delivery := Delivery{
		Source:      source.Name,
		URL:         source.URL,
		Version:     run.Version,
		ExtractedAt: run.At,
		Count:       len(records),
		Records:     records,
	}

	// The key covers the records and the source, and deliberately not the run
	// time or the artifact version: a heal that changes how the data is read
	// but not what it says should not look like new data downstream.
	body, err := json.Marshal(struct {
		Source  string           `json:"source"`
		Records []extract.Record `json:"records"`
	}{source.Name, records})
	if err != nil {
		return Delivery{}, fmt.Errorf("key delivery for %s: %w", source.Name, err)
	}

	sum := sha256.Sum256(body)
	delivery.Key = hex.EncodeToString(sum[:])
	return delivery, nil
}

// Sinks delivers to several sinks, and is itself a Sink.
//
// A failure in one does not stop the others: the pull interface being full is
// no reason for the push interface not to fire, and the caller is told about
// everything that went wrong rather than only the first thing.
type Sinks []Sink

func (s Sinks) Publish(ctx context.Context, source extract.Source, run extract.Run, records []extract.Record) error {
	var failures []error
	for _, sink := range s {
		if err := sink.Publish(ctx, source, run, records); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("publish %s: %w", source.Name, errors.Join(failures...))
	}
	return nil
}

// StoreSink is the pull interface: it writes each source's latest output to a
// fixed key in object storage, where the API and anything else can read it.
//
// Writing to a fixed key is what makes it idempotent. A repeated run overwrites
// with the same content rather than appending, so a consumer polling the key
// sees each extraction once however many times it ran.
type StoreSink struct {
	store *Store
}

// NewStoreSink returns a sink writing into the harvest store.
func NewStoreSink(store *Store) *StoreSink { return &StoreSink{store: store} }

func (s *StoreSink) Publish(ctx context.Context, source extract.Source, run extract.Run, records []extract.Record) error {
	delivery, err := NewDelivery(source, run, records)
	if err != nil {
		return err
	}
	return s.store.SaveOutput(ctx, delivery)
}

// WebhookSink is the push interface: it POSTs each delivery to a URL.
//
// The key travels as an Idempotency-Key header as well as in the body, because
// a receiver that wants to deduplicate should not have to parse a payload to
// discover it has already seen it.
type WebhookSink struct {
	url    string
	client *http.Client
}

// webhookTimeout bounds one delivery. Generous, because a receiver doing real
// work with a few hundred records is not misbehaving.
const webhookTimeout = 30 * time.Second

// NewWebhookSink returns a sink posting to url.
func NewWebhookSink(url string) *WebhookSink {
	return &WebhookSink{url: url, client: &http.Client{Timeout: webhookTimeout}}
}

func (s *WebhookSink) Publish(ctx context.Context, source extract.Source, run extract.Run, records []extract.Record) error {
	delivery, err := NewDelivery(source, run, records)
	if err != nil {
		return err
	}

	body, err := json.Marshal(delivery)
	if err != nil {
		return fmt.Errorf("encode delivery for %s: %w", source.Name, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build delivery request for %s: %w", source.Name, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", delivery.Key)

	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("post %s to %s: %w", source.Name, s.url, err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("post %s to %s: %s: %s",
			source.Name, s.url, response.Status, bytes.TrimSpace(detail))
	}
	return nil
}
