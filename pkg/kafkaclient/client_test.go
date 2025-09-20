package kafkaclient

import (
	"context"
	"fmt"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// mockReader simulates the kafka-go Reader for unit testing.
type mockReader struct {
	messages   chan kafka.Message
	commitChan chan kafka.Message
	wg         sync.WaitGroup
	isClosed   bool
}

// newMockReader creates a new mock reader.
func newMockReader() *mockReader {
	return &mockReader{
		messages:   make(chan kafka.Message, 10),
		commitChan: make(chan kafka.Message, 10),
	}
}

// StartSimulatingConsumption simulates messages being produced to the reader.
func (mr *mockReader) StartSimulatingConsumption(count int) {
	mr.wg.Add(1)
	go func() {
		defer mr.wg.Done()
		defer close(mr.messages) // Critical: close the channel when done.

		for i := 0; i < count; i++ {
			msg := kafka.Message{
				Topic:     "test-topic",
				Partition: 0,
				Offset:    int64(i),
				Value:     []byte(fmt.Sprintf("mock-message-%d", i)),
			}
			mr.messages <- msg
			// Simulate network delay
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

// ReadMessage simulates reading a message from the Kafka stream.
func (mr *mockReader) ReadMessage(ctx context.Context) (kafka.Message, error) {
	if mr.isClosed {
		return kafka.Message{}, fmt.Errorf(" ReadMessage  mr.isClosed  kafka: reader closed")
	}
	select {
	case <-ctx.Done():
		return kafka.Message{}, ctx.Err()
	case msg, ok := <-mr.messages:
		if !ok {
			return kafka.Message{}, fmt.Errorf("ReadMessage kafka: reader closed")
		}
		return msg, nil
	}
}

// CommitMessages simulates committing message offsets.
func (mr *mockReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	if mr.isClosed {
		return fmt.Errorf("kafka: reader closed")
	}
	for _, msg := range msgs {
		mr.commitChan <- msg
	}
	return nil
}

// Close simulates closing the reader.
func (mr *mockReader) Close() error {
	log.Println("Mock reader closing.")
	mr.isClosed = true
	close(mr.commitChan)
	return nil
}

// TestKafkaConsumerAndIterator_WithMock tests the full consumption and iteration flow using a mock reader.
func TestKafkaConsumerAndIterator_WithMock(t *testing.T) {
	// Create a context for the test with a timeout to prevent it from hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Create a real KafkaConsumer instance, but inject our mock reader into it.
	mockReader := newMockReader()
	consumer := &KafkaConsumer{
		reader:      mockReader,
		doneChan:    make(chan struct{}),
		messageChan: make(chan kafka.Message),
	}

	// Start simulating messages being produced.
	const expectedMessages = 3
	mockReader.StartSimulatingConsumption(expectedMessages)

	// Start the consumer loop, which will read from the mock reader.
	consumer.StartConsuming(ctx)

	// Get the iterator that our test will use.
	iterator := consumer.NewIterator()

	// Consume the messages using the for-range loop.
	messagesReceived := 0
	for msg := range iterator.Messages() {
		expectedValue := fmt.Sprintf("mock-message-%d", messagesReceived)
		if string(msg.Value) != expectedValue {
			t.Errorf("Expected message value %q, got %q", expectedValue, string(msg.Value))
		}

		// Test the commit functionality.
		if err := iterator.CommitOffset(ctx, msg); err != nil {
			t.Errorf("CommitOffset() failed: %v", err)
		}

		messagesReceived++
	}

	// Verify that the correct number of messages was received.
	if messagesReceived != expectedMessages {
		t.Errorf("Expected to receive %d messages, but got %d", expectedMessages, messagesReceived)
	}

	// Wait for the consumer to finish its graceful shutdown.
	consumer.Stop()

	// Verify that the offsets were committed correctly by checking the mockReader's commit channel.
	committedMessages := 0
	for range mockReader.commitChan {
		committedMessages++
	}

	if committedMessages != expectedMessages {
		t.Errorf("Expected to commit %d messages, but committed %d", expectedMessages, committedMessages)
	}
}

// TestKafkaConsumer_GracefulShutdown verifies that the consumer can be stopped gracefully
// even if the Kafka stream is still active.
func TestKafkaConsumer_GracefulShutdown(t *testing.T) {
	// Create a context for the test with a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Use a mock reader that will produce a large number of messages.
	mockReader := newMockReader()
	consumer := &KafkaConsumer{
		reader:      mockReader,
		doneChan:    make(chan struct{}),
		messageChan: make(chan kafka.Message),
	}

	// Start a simulation of a large number of messages.
	// The consumer should stop before consuming all of them.
	mockReader.StartSimulatingConsumption(100)

	// Start the consumer's consumption loop.
	consumer.StartConsuming(ctx)

	// Get the iterator.
	iterator := consumer.NewIterator()

	// Consume a few messages to ensure the consumer loop is running.
	messagesConsumed := 0
	for i := 0; i < 5; i++ {
		select {
		case msg := <-iterator.Messages():
			t.Logf("Consumed message %d: %s", i, string(msg.Value))
			messagesConsumed++
		case <-ctx.Done():
			t.Fatal("Context canceled unexpectedly.")
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Timed out while waiting for a message.")
		}
	}

	// Signal the consumer to stop gracefully.
	consumer.Stop()

	// After stopping, try to read from the message channel to confirm it's closed.
	// This for loop should exit immediately because the channel is closed.
	remainingMessages := 0
	for range iterator.Messages() {
		remainingMessages++
	}

	if remainingMessages > 0 {
		t.Errorf("Expected 0 messages after consumer stop, but found %d", remainingMessages)
	}

	// Verify that the number of messages consumed is at least 5 but less than the total.
	if messagesConsumed < 5 {
		t.Errorf("Expected to consume at least 5 messages before stopping, but only consumed %d", messagesConsumed)
	}

	// Verify that the mock reader was closed.
	if !mockReader.isClosed {
		t.Error("Expected mock reader to be closed after consumer.Stop(), but it was not.")
	}

	t.Log("Graceful shutdown test passed.")
}
