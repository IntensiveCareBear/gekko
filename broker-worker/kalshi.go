package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	randmath "math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// KalshiOrderRequest corresponds to the Kalshi v2 API order schema.
type KalshiOrderRequest struct {
	Ticker        string `json:"ticker"`
	Action        string `json:"action"` // buy, sell
	Side          string `json:"side"`   // yes, no
	Count         int    `json:"count"`
	Price         int64  `json:"price"` // price in cents (1–99)
	Type          string `json:"type"`  // limit
	ClientOrderID string `json:"client_order_id"`
}

// ExecuteKalshi constructs a Kalshi v2 order, signs it with RSA-PSS, and submits it.
// privateKeyPEM is the PEM-encoded PKCS8 RSA private key (value of KALSHI_PRIVATE_KEY_PEM env var).
func ExecuteKalshi(intent TradeIntent, apiKey, privateKeyPEM string) TradeResult {
	apiBase := getEnv("KALSHI_API_BASE", "https://demo-api.kalshi.co")

	privateKey, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return TradeResult{
			Success:  false,
			Message:  fmt.Sprintf("Failed to parse Kalshi RSA private key: %v", err),
			Exchange: "kalshi",
		}
	}

	rng := randmath.New(randmath.NewSource(time.Now().UnixNano()))
	clientOrderID := fmt.Sprintf("gekko-%d-%d", time.Now().UnixNano(), rng.Intn(100000))

	action := strings.ToLower(intent.Action)
	if action != "buy" && action != "sell" {
		action = "buy"
	}

	priceCents := int64(intent.MaxPrice * 100)
	if priceCents <= 0 || priceCents > 99 {
		priceCents = 50
	}

	orderReq := KalshiOrderRequest{
		Ticker:        intent.ContractID,
		Action:        action,
		Side:          "yes",
		Count:         intent.Size,
		Price:         priceCents,
		Type:          "limit",
		ClientOrderID: clientOrderID,
	}

	bodyBytes, err := json.Marshal(orderReq)
	if err != nil {
		return TradeResult{
			Success:  false,
			Message:  fmt.Sprintf("Failed to marshal Kalshi request: %v", err),
			Exchange: "kalshi",
		}
	}

	method := "POST"
	path := "/trade-api/v2/portfolio/orders"
	url := apiBase + path
	timestampStr := strconv.FormatInt(time.Now().UnixMilli(), 10)

	// Kalshi v2 signature: RSA-PSS SHA-256 over "<timestamp><method><path>"
	// The request body is NOT included in the signed string per Kalshi spec.
	msgToSign := timestampStr + method + path
	hashed := sha256.Sum256([]byte(msgToSign))
	sigBytes, err := rsa.SignPSS(rand.Reader, privateKey, crypto.SHA256, hashed[:], nil)
	if err != nil {
		return TradeResult{
			Success:  false,
			Message:  fmt.Sprintf("RSA-PSS signing failed: %v", err),
			Exchange: "kalshi",
		}
	}
	signature := base64.StdEncoding.EncodeToString(sigBytes)

	req, err := http.NewRequest(method, url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return TradeResult{
			Success:  false,
			Message:  fmt.Sprintf("Failed to construct Kalshi HTTP request: %v", err),
			Exchange: "kalshi",
		}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("KALSHI-ACCESS-KEY", apiKey)
	req.Header.Set("KALSHI-ACCESS-SIGNATURE", signature)
	req.Header.Set("KALSHI-ACCESS-TIMESTAMP", timestampStr)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return TradeResult{
			Success:  false,
			Message:  fmt.Sprintf("Kalshi network call failed: %v", err),
			Exchange: "kalshi",
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return TradeResult{
			Success:  false,
			Message:  fmt.Sprintf("Kalshi API returned status %d: %s", resp.StatusCode, string(respBody)),
			Exchange: "kalshi",
		}
	}

	return TradeResult{
		Success:    true,
		Message:    "Order successfully routed and accepted by Kalshi API",
		Exchange:   "kalshi",
		BrokerTxID: clientOrderID,
	}
}

// parseRSAPrivateKey decodes a PEM-encoded PKCS8 RSA private key.
// The PEM value may have literal \n sequences instead of real newlines (common in env vars).
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	pemStr = strings.ReplaceAll(pemStr, `\n`, "\n")
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block — ensure KALSHI_PRIVATE_KEY_PEM contains a valid PEM-encoded PKCS8 RSA key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ParsePKCS8PrivateKey: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not RSA (got %T)", key)
	}
	return rsaKey, nil
}
