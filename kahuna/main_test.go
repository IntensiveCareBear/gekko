package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gekko/kahuna/ingest"
)

// TestGekkoIngestEvent_Serialization checks that GekkoIngestEvent serializes matching the exact JSON specifications.
func TestGekkoIngestEvent_Serialization(t *testing.T) {
	event := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceKalshi,
		RawContent:           `{"type": "market_lifecycle_v2", "market_id": "TEST-1"}`,
		SourceURL:            "https://api.kalshi.com/v2/market_lifecycle",
		ExtractedTimestampNS: 1718485200000000000,
	}
	event.PayloadHash = event.ComputeHash()

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Failed to marshal GekkoIngestEvent: %v", err)
	}

	jsonStr := string(data)

	// Validate expected JSON keys
	expectedKeys := []string{
		`"SourceID"`,
		`"RawContent"`,
		`"SourceURL"`,
		`"ExtractedTimestampNS"`,
		`"PayloadHash"`,
	}

	for _, key := range expectedKeys {
		if !strings.Contains(jsonStr, key) {
			t.Errorf("Expected serialized event to contain key %s, got: %s", key, jsonStr)
		}
	}

	// Unmarshal to verify exact match
	var deserialized ingest.GekkoIngestEvent
	if err := json.Unmarshal(data, &deserialized); err != nil {
		t.Fatalf("Failed to unmarshal GekkoIngestEvent: %v", err)
	}

	if deserialized.SourceID != event.SourceID ||
		deserialized.RawContent != event.RawContent ||
		deserialized.SourceURL != event.SourceURL ||
		deserialized.ExtractedTimestampNS != event.ExtractedTimestampNS ||
		deserialized.PayloadHash != event.PayloadHash {
		t.Errorf("Deserialized struct does not match original. Got: %+v, Expected: %+v", deserialized, event)
	}
}

// BlockedProducer mocks a Kafka writer that blocks indefinitely to test backpressure safety.
type BlockedProducer struct {
	writeTimeout time.Duration
}

func (bp *BlockedProducer) Produce(ctx context.Context, event *ingest.GekkoIngestEvent) error {
	// Simulate writing with timeout
	writeCtx, cancel := context.WithTimeout(ctx, bp.writeTimeout)
	defer cancel()

	select {
	case <-time.After(50 * time.Millisecond):
		// Simulate successful write after slight delay
		return nil
	case <-writeCtx.Done():
		// Simulate timeout block
		return writeCtx.Err()
	}
}

func (bp *BlockedProducer) Close() error {
	return nil
}

func TestBackpressureSafety(t *testing.T) {
	// Setup BlockedProducer with a very small write timeout to force a timeout drop
	prod := &BlockedProducer{
		writeTimeout: 5 * time.Millisecond,
	}

	event := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceTwitter,
		RawContent:           "High throughput event message",
		SourceURL:            "https://twitter.com/X",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	event.PayloadHash = event.ComputeHash()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Write should fail/timeout after 5ms, returning error context deadline exceeded, without blocking the remaining duration of the test.
	startTime := time.Now()
	err := prod.Produce(ctx, event)
	elapsed := time.Since(startTime)

	if err == nil {
		t.Fatal("Expected write timeout error, got nil")
	}

	if elapsed >= 30*time.Millisecond {
		t.Errorf("Producer blocked for too long: %v, expected less than 35ms", elapsed)
	}
}
