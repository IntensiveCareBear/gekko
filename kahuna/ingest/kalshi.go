package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/gorilla/websocket"
)

type KalshiSource struct {
	wsURL     string
	simulated bool
}

func NewKalshiSource(wsURL string, simulated bool) *KalshiSource {
	return &KalshiSource{
		wsURL:     wsURL,
		simulated: simulated,
	}
}

type KalshiLifecycleMessage struct {
	Type      string    `json:"type"`
	MarketID  string    `json:"market_id"`
	Status    string    `json:"status"`
	YesPrice  int64     `json:"yes_price"`
	NoPrice   int64     `json:"no_price"`
	Timestamp time.Time `json:"timestamp"`
}

func (k *KalshiSource) Start(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if k.simulated {
		return k.startSimulation(ctx, output)
	}
	return k.startLive(ctx, output)
}

func (k *KalshiSource) startSimulation(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	ticker := time.NewTicker(1200 * time.Millisecond)
	defer ticker.Stop()

	markets := []string{"FED-RATE-2026", "AAPL-UP-200", "BTC-100K-DEC", "OIL-85-DEC"}
	statuses := []string{"open", "active", "closed", "settled"}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	log.Println("[INFO] Kalshi Ingestion Source started in SIMULATED mode")

	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Kalshi Ingestion Source shutting down...")
			return nil
		case <-ticker.C:
			mID := markets[r.Intn(len(markets))]
			status := statuses[r.Intn(len(statuses))]
			yes := int64(r.Intn(100))
			no := 100 - yes

			msg := KalshiLifecycleMessage{
				Type:      "market_lifecycle_v2",
				MarketID:  mID,
				Status:    status,
				YesPrice:  yes,
				NoPrice:   no,
				Timestamp: time.Now(),
			}

			raw, err := json.Marshal(msg)
			if err != nil {
				log.Printf("[ERROR] Kalshi marshal error: %v", err)
				continue
			}

			event := &GekkoIngestEvent{
				SourceID:             SourceKalshi,
				RawContent:           string(raw),
				SourceURL:            k.wsURL,
				ExtractedTimestampNS: time.Now().UnixNano(),
			}
			event.PayloadHash = event.ComputeHash()

			select {
			case output <- event:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func (k *KalshiSource) startLive(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	log.Printf("[INFO] Kalshi Ingestion Source connecting to live WS: %s", k.wsURL)
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, k.wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to dial Kalshi WS: %w", err)
	}
	defer conn.Close()

	// Send subscription message if needed
	subMsg := map[string]interface{}{
		"action": "subscribe",
		"channels": []string{"market_lifecycle_v2"},
	}
	if err := conn.WriteJSON(subMsg); err != nil {
		return fmt.Errorf("failed to subscribe to Kalshi channel: %w", err)
	}

	// Read loop
	errChan := make(chan error, 1)
	go func() {
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			event := &GekkoIngestEvent{
				SourceID:             SourceKalshi,
				RawContent:           string(message),
				SourceURL:            k.wsURL,
				ExtractedTimestampNS: time.Now().UnixNano(),
			}
			event.PayloadHash = event.ComputeHash()

			select {
			case output <- event:
			case <-ctx.Done():
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errChan:
		return fmt.Errorf("Kalshi WS read error: %w", err)
	}
}
