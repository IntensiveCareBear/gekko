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

type PolymarketSource struct {
	wsURL     string
	simulated bool
}

func NewPolymarketSource(wsURL string, simulated bool) *PolymarketSource {
	return &PolymarketSource{
		wsURL:     wsURL,
		simulated: simulated,
	}
}

type PolymarketCLOBSnapshot struct {
	Event     string    `json:"event"`
	Topic     string    `json:"topic"`
	MarketID  string    `json:"market_id"`
	Bids      [][2]string `json:"bids"` // Price, Size
	Asks      [][2]string `json:"asks"` // Price, Size
	Timestamp time.Time `json:"timestamp"`
}

func (p *PolymarketSource) Start(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if p.simulated {
		return p.startSimulation(ctx, output)
	}
	return p.startLive(ctx, output)
}

func (p *PolymarketSource) startSimulation(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	markets := []string{
		"0x2b2e88a0", // Mock polymarket address hash
		"0x7f23c4a1",
		"0xbf1103c8",
	}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	log.Println("[INFO] Polymarket Ingestion Source started in SIMULATED mode")

	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Polymarket Ingestion Source shutting down...")
			return nil
		case <-ticker.C:
			mID := markets[r.Intn(len(markets))]
			bidPrice := 0.45 + r.Float64()*0.10
			askPrice := bidPrice + 0.01 + r.Float64()*0.02

			msg := PolymarketCLOBSnapshot{
				Event:    "snapshot",
				Topic:    "clob",
				MarketID: mID,
				Bids: [][2]string{
					{fmt.Sprintf("%.2f", bidPrice), fmt.Sprintf("%d", 1000+r.Intn(5000))},
					{fmt.Sprintf("%.2f", bidPrice-0.01), fmt.Sprintf("%d", 2000+r.Intn(8000))},
				},
				Asks: [][2]string{
					{fmt.Sprintf("%.2f", askPrice), fmt.Sprintf("%d", 1000+r.Intn(5000))},
					{fmt.Sprintf("%.2f", askPrice+0.01), fmt.Sprintf("%d", 2000+r.Intn(8000))},
				},
				Timestamp: time.Now(),
			}

			raw, err := json.Marshal(msg)
			if err != nil {
				log.Printf("[ERROR] Polymarket marshal error: %v", err)
				continue
			}

			event := &GekkoIngestEvent{
				SourceID:             SourcePolymarket,
				RawContent:           string(raw),
				SourceURL:            p.wsURL,
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

func (p *PolymarketSource) startLive(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	log.Printf("[INFO] Polymarket Ingestion Source connecting to live WS: %s", p.wsURL)
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, p.wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to dial Polymarket WS: %w", err)
	}
	defer conn.Close()

	// Subscribe to order_book topic or clob topic
	subMsg := map[string]interface{}{
		"type": "subscribe",
		"topic": "clob",
	}
	if err := conn.WriteJSON(subMsg); err != nil {
		return fmt.Errorf("failed to subscribe to Polymarket CLOB: %w", err)
	}

	errChan := make(chan error, 1)
	go func() {
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			event := &GekkoIngestEvent{
				SourceID:             SourcePolymarket,
				RawContent:           string(message),
				SourceURL:            p.wsURL,
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
		return fmt.Errorf("Polymarket WS read error: %w", err)
	}
}
