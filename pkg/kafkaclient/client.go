package kafkaclient

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

// KafkaReader defines the interface for a Kafka message reader.
// This allows for easy mocking in unit tests.
type KafkaReader interface {
	ReadMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// KafkaConsumer manages the Kafka consumer and its message loop.
// It is designed to be thread-safe.
type KafkaConsumer struct {
	reader KafkaReader
	// a channel to signal a graceful shutdown.
	doneChan chan struct{}
	// a wait group to ensure all goroutines have exited before the program terminates.
	wg sync.WaitGroup
	// a channel to hold the Kafka messages, which are then consumed by the Iterator.
	messageChan chan kafka.Message
}

func (kc *KafkaConsumer) Messages() <-chan kafka.Message {
	return kc.messageChan

}

func (kc *KafkaConsumer) CommitOffset(ctx context.Context, msg kafka.Message) error {
	log.Printf("Committing offset for topic=%s, partition=%d, offset=%d", msg.Topic, msg.Partition, msg.Offset)
	return kc.reader.CommitMessages(ctx, msg)
}

// NewKafkaConsumer creates a new instance of KafkaConsumer.
func NewKafkaConsumer(topic, groupID, broker string) (*KafkaConsumer, error) {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: []string{broker},
		Topic:   topic,
		GroupID: groupID,
		// Disable auto-commit to manually control offset committing.
		CommitInterval: 0,
		// Read messages in batches of at least 10KB.
		MinBytes: 10e3,
		// Read messages in batches of at most 10MB.
		MaxBytes: 10e6,
	})
	return &KafkaConsumer{
		reader:      reader,
		doneChan:    make(chan struct{}),
		messageChan: make(chan kafka.Message),
	}, nil
}

// StartConsuming begins the Kafka message consumption loop in a separate goroutine.
func (kc *KafkaConsumer) StartConsuming(ctx context.Context) {
	kc.wg.Add(1)
	go func() {
		defer kc.wg.Done()
		defer close(kc.messageChan)

		log.Println("Starting Kafka consumer loop...")

		for {
			select {
			// Check for context cancellation or done signal.
			case <-ctx.Done():
				log.Println("Context canceled, stopping consumer loop.")
				return
			case <-kc.doneChan:
				log.Println("Shutdown signal received, stopping consumer loop.")
				return
			default:
				// Read a single message.
				msg, err := kc.reader.ReadMessage(ctx)
				if err != nil {
					log.Printf("Error reading message: %v", err)
					// Handle the specific error of a closed reader.
					if err.Error() == "kafka: reader closed" {
						return
					}
					// Introduce a backoff to prevent a tight error loop.
					time.Sleep(1 * time.Second)
					continue
				}

				// Send the message to the message channel for external consumption.
				select {
				case kc.messageChan <- msg:
					log.Printf("Message received: topic=%s, partition=%d, offset=%d\n", msg.Topic, msg.Partition, msg.Offset)
				case <-ctx.Done():
					log.Println("Context canceled, stopping consumer before sending message.")
					return
				case <-kc.doneChan:
					log.Println("Shutdown signal received, stopping consumer before sending message.")
					return
				}
			}
		}
	}()
}

// Stop gracefully shuts down the Kafka consumer.
func (kc *KafkaConsumer) Stop() {
	log.Println("Attempting to stop Kafka consumer...")
	close(kc.doneChan)
	kc.wg.Wait()
	if err := kc.reader.Close(); err != nil {
		log.Printf("Failed to close Kafka reader: %v", err)
	}
	log.Println("Kafka consumer stopped gracefully.")
}

// Iterator provides a channel-based interface to consume messages.
type Iterator struct {
	messages chan kafka.Message
	consumer *KafkaConsumer
}

// NewIterator returns a new Iterator for the consumer.
func (kc *KafkaConsumer) NewIterator() *Iterator {
	return &Iterator{
		messages: kc.messageChan,
		consumer: kc,
	}
}

// Messages returns the channel of Kafka messages.
func (it *Iterator) Messages() <-chan kafka.Message {
	return it.messages
}

// CommitOffset manually commits the offset of a message.
func (it *Iterator) CommitOffset(ctx context.Context, msg kafka.Message) error {
	log.Printf("Committing offset for topic=%s, partition=%d, offset=%d", msg.Topic, msg.Partition, msg.Offset)
	// The segmentio/kafka-go library's Reader.CommitMessages method takes a variadic list of messages.
	// You can batch messages for more efficient commits if desired.
	return it.consumer.reader.CommitMessages(ctx, msg)
}
