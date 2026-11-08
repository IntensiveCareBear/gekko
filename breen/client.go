package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type TradeIntent struct {
	ContractID string  `json:"contract_id"`
	Action     string  `json:"action"` // BUY, SELL
	Size       int     `json:"size"`
	MaxPrice   float64 `json:"max_price"`
	Exchange   string  `json:"exchange"` // kalshi, polymarket
	DryMode    bool    `json:"dry_mode"`
}

type TradeResult struct {
	Success     bool   `json:"success"`
	Message     string `json:"message"`
	Exchange    string `json:"exchange"`
	BrokerTxID  string `json:"broker_tx_id,omitempty"`
	Signature   string `json:"signature,omitempty"`
	PayloadSent string `json:"payload_sent,omitempty"`
	// Fill report from the broker. FilledSize > 0 is the confirmed matched size and replaces the
	// proposal size; 0 means the exchange gave no fill data and the proposal size stands.
	FilledSize  int     `json:"filled_size,omitempty"`
	FilledPrice float64 `json:"filled_price,omitempty"`
	Status      string  `json:"status,omitempty"`
}

type BrokerClient struct {
	client     *http.Client
	socketPath string
	token      string
}

func NewBrokerClient(socketPath, token string) *BrokerClient {
	// Configure custom transport to dial over Unix domain socket
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	return &BrokerClient{
		client: &http.Client{
			Transport: transport,
			// Longer than the broker's fill wait (POLYMARKET_FILL_WAIT, default 8s) plus its cancel
			// round trip, so a confirmed fill is not abandoned by the caller.
			Timeout: 30 * time.Second,
		},
		socketPath: socketPath,
		token:      token,
	}
}

// ForwardToBroker serializes the trade intent and forwards it to the broker-worker over Unix socket.
func (bc *BrokerClient) ForwardToBroker(ctx context.Context, contractID, action string, size int, price float64) (*TradeResult, error) {
	intent := TradeIntent{
		ContractID: contractID,
		Action:     action,
		Size:       size,
		MaxPrice:   price,
		DryMode:    false, // Must explicitly set dry_mode to false for broker-worker
	}

	bodyBytes, err := json.Marshal(intent)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal trade intent: %w", err)
	}

	// Route request using a dummy host name over Unix socket
	url := "http://unix/execute"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to construct HTTP request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bc.token)

	resp, err := bc.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network call to broker socket failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read broker response: %w", err)
	}

	var result TradeResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		// Try to read raw error string if not JSON
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("broker HTTP %d error: %s", resp.StatusCode, string(respBody))
		}
		return nil, fmt.Errorf("failed to unmarshal broker response JSON: %w (Raw: %s)", err, string(respBody))
	}

	return &result, nil
}
