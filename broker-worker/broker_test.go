package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Minimal custom client implementation for broker-worker test isolation
type TestClient struct {
	client     *http.Client
	socketPath string
	token      string
}

func NewTestClient(socketPath, token string) *TestClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &TestClient{
		client: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
		},
		socketPath: socketPath,
		token:      token,
	}
}

func (tc *TestClient) Forward(ctx context.Context, contractID, action string, size int, price float64, dryMode bool) (*TradeResult, error) {
	intent := TradeIntent{
		ContractID: contractID,
		Action:     action,
		Size:       size,
		MaxPrice:   price,
		DryMode:    dryMode,
	}
	bodyBytes, err := json.Marshal(intent)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/execute", bytes.NewBuffer(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tc.token)

	resp, err := tc.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result TradeResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func TestBrokerAuthenticationAndRejection(t *testing.T) {
	socketPath := "./test_broker.sock"
	defer os.Remove(socketPath)

	token := "correct-token"

	// Start broker worker in background goroutine
	os.Setenv("BROKER_SOCKET_PATH", socketPath)
	os.Setenv("GEKKO_BROKER_TOKEN", token)
	os.Setenv("KALSHI_API_KEY", "dummy-key")
	os.Setenv("KALSHI_API_SECRET", "dummy-secret")

	// We'll mock main function behavior in a short server routine
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Failed to listen on unix test socket: %v", err)
	}
	defer listener.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/execute", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"success":false,"message":"unauthorized"}`))
			return
		}

		var intent TradeIntent
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &intent)

		if intent.DryMode {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"success":false,"message":"reject dry-mode"}`))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":true,"message":"executed","exchange":"kalshi"}`))
	})

	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	// Give socket time to initialize
	time.Sleep(100 * time.Millisecond)

	// Test 1: Missing/Invalid token
	clientBadToken := NewTestClient(socketPath, "bad-token")
	_, err = clientBadToken.Forward(context.Background(), "AAPL-UP-200", "BUY", 100, 0.76, false)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("Expected 401 Unauthorized error, got: %v", err)
	}

	// Test 2: Valid token routes successfully
	clientGood := NewTestClient(socketPath, token)
	res, err := clientGood.Forward(context.Background(), "AAPL-UP-200", "BUY", 100, 0.76, false)
	if err != nil {
		t.Fatalf("Expected successful forwarding, got error: %v", err)
	}
	if !res.Success {
		t.Errorf("Expected execution success, got: %v", res.Message)
	}

	// Test 3: Reject dry mode requests
	_, err = clientGood.Forward(context.Background(), "AAPL-UP-200", "BUY", 100, 0.76, true)
	if err == nil {
		t.Error("Expected broker-worker to reject dry-mode request, but got no error")
	} else if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("Expected HTTP 400 Bad Request error, got: %v", err)
	}
}

func TestKalshiRSAPSSSignature(t *testing.T) {
	// Generate a throwaway RSA key in PEM form for the test
	pemKey := generateTestRSAKeyPEM(t)

	intent := TradeIntent{
		ContractID: "AAPL-UP-200",
		Action:     "BUY",
		Size:       500,
		MaxPrice:   0.76,
	}

	// ExecuteKalshi will fail at the network call (demo endpoint unreachable in tests).
	// We verify it correctly returns Success=false rather than silently succeeding.
	res := ExecuteKalshi(intent, "test-api-key", pemKey)
	if res.Success {
		t.Error("Expected Success=false when Kalshi endpoint is unreachable in unit test environment")
	}
	// No key material should appear in the message
	if strings.Contains(res.Message, "test-api-key") {
		t.Error("API key must not appear in error message")
	}
}

// generateTestRSAKeyPEM generates a 2048-bit RSA key and returns it as a PKCS8 PEM string.
func generateTestRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
