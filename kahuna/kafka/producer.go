package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"gekko/kahuna/ingest"

	"github.com/segmentio/kafka-go"
)

type Producer interface {
	Produce(ctx context.Context, event *ingest.GekkoIngestEvent) error
	Close() error
}

// KafkaProducer publishes validated events to a real Kafka cluster.
type KafkaProducer struct {
	writer       *kafka.Writer
	writeTimeout time.Duration
}

func NewKafkaProducer(brokers []string, topic string) *KafkaProducer {
	w := &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Topic:        topic,
		Balancer:     &kafka.LeastBytes{},
		WriteTimeout: 2 * time.Second, // Timeout if broker buffer/connection blocks
		RequiredAcks: kafka.RequireOne,
	}

	return &KafkaProducer{
		writer:       w,
		writeTimeout: 2 * time.Second,
	}
}

func (kp *KafkaProducer) Produce(ctx context.Context, event *ingest.GekkoIngestEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal ingest event: %w", err)
	}

	// Bounded timeout context to ensure Kahuna pipeline is never permanently blocked by Kafka issues
	writeCtx, cancel := context.WithTimeout(ctx, kp.writeTimeout)
	defer cancel()

	err = kp.writer.WriteMessages(writeCtx, kafka.Message{
		Key:   []byte(event.SourceID),
		Value: data,
	})

	if err != nil {
		// Log warning without crashing (backpressure / safety limit requirement)
		log.Printf("[WARNING] Kafka broker write failed (event dropped): %v | Source: %s, URL: %s", err, event.SourceID, event.SourceURL)
		return err
	}

	return nil
}

func (kp *KafkaProducer) Close() error {
	return kp.writer.Close()
}

// ConsoleProducer prints events to stdout, serving as a fallback and verification tool.
type ConsoleProducer struct{}

func NewConsoleProducer() *ConsoleProducer {
	return &ConsoleProducer{}
}

func (cp *ConsoleProducer) Produce(ctx context.Context, event *ingest.GekkoIngestEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	log.Printf("[CONSOLE PRODUCER] Published: %s", string(data))
	return nil
}

func (cp *ConsoleProducer) Close() error {
	return nil
}
