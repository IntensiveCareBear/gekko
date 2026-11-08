package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
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
	Success    bool   `json:"success"`
	Message    string `json:"message"`
	Exchange   string `json:"exchange"`
	BrokerTxID string `json:"broker_tx_id,omitempty"`
	Signature  string `json:"signature,omitempty"`
	// Fill report (Polymarket). FilledSize is the confirmed matched size in whole contracts;
	// 0 with Success=true means the exchange does not report fills (Kalshi today), so the
	// caller must not assume more than it asked for. FilledPrice is the order's limit price,
	// an upper bound on a BUY's average fill (the exact average needs the trades endpoint).
	FilledSize  int     `json:"filled_size,omitempty"`
	FilledPrice float64 `json:"filled_price,omitempty"`
	Status      string  `json:"status,omitempty"` // filled | partial | unfilled
}

func main() {
	log.Println("[INFO] Starting Gekko Exchange Broker Worker...")

	socketPath := getEnv("BROKER_SOCKET_PATH", "/tmp/gekko-broker.sock")

	authToken := os.Getenv("GEKKO_BROKER_TOKEN")
	if authToken == "" {
		log.Fatalf("[FATAL] GEKKO_BROKER_TOKEN env var must be set — refusing to start without an explicit auth token")
	}

	// Pre-load keys to verify they exist in broker environment space
	kalshiKey := os.Getenv("KALSHI_API_KEY")
	kalshiKeyPEM := os.Getenv("KALSHI_PRIVATE_KEY_PEM")
	polyKey := brokerSecret("POLYMARKET_PRIVATE_KEY")

	log.Printf("[INFO] LIVE_TRADING=%v (exchange secrets are only loaded when true); Polymarket API creds complete=%v",
		liveTradingEnabled(), polyCredsFromEnv().complete())
	log.Printf("[INFO] Loaded Broker Credentials status: Kalshi=%v | Polymarket=%v",
		kalshiKey != "" && kalshiKeyPEM != "", polyKey != "")

	// Expose /healthz on a dedicated TCP port (separate from the order socket).
	startHealthServer(getEnv("HEALTH_ADDR", ":8090"), kalshiKey != "" && kalshiKeyPEM != "", polyKey != "")

	// Clean up old socket file if it exists
	if _, err := os.Stat(socketPath); err == nil {
		log.Printf("[INFO] Cleaning up existing socket file: %s", socketPath)
		if err := os.Remove(socketPath); err != nil {
			log.Fatalf("[FATAL] Failed to remove existing socket file: %v", err)
		}
	}

	// Listen on Unix domain socket
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("[FATAL] Failed to listen on Unix socket %s: %v", socketPath, err)
	}
	defer listener.Close()

	// Adjust permissions on socket file so breen can access it
	if err := os.Chmod(socketPath, 0660); err != nil {
		log.Printf("[WARNING] Failed to set socket permissions: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/execute", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requestsTotal.Add(1)

		// 1. Authenticate request — constant-time comparison to prevent timing attacks
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, "Unauthorized: Missing or invalid token format", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(authToken)) != 1 {
			http.Error(w, "Unauthorized: Handshake token mismatch", http.StatusUnauthorized)
			return
		}

		// 2. Parse request body — cap at 64 KB to prevent OOM
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Bad request body reading", http.StatusBadRequest)
			return
		}
		var intent TradeIntent
		if err := json.Unmarshal(bodyBytes, &intent); err != nil {
			http.Error(w, "Bad request JSON parsing", http.StatusBadRequest)
			return
		}

		// 3. Security check: broker rejects dry-mode orders
		if intent.DryMode {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(TradeResult{
				Success: false,
				Message: "Broker-worker rejects dry-mode orders. Executive must bypass network routing for Dry Mode.",
			})
			return
		}

		// 4. Determine target exchange and route
		exchange := routeExchange(intent)

		var res TradeResult
		if exchange == "kalshi" {
			if kalshiKey == "" || kalshiKeyPEM == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(TradeResult{
					Success:  false,
					Message:  "Kalshi credentials missing from broker environment (need KALSHI_API_KEY and KALSHI_PRIVATE_KEY_PEM)",
					Exchange: "kalshi",
				})
				return
			}
			res = ExecuteKalshi(intent, kalshiKey, kalshiKeyPEM)
		} else if exchange == "polymarket" {
			if polyKey == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(TradeResult{
					Success:  false,
					Message:  "Polymarket corporate wallet key missing from broker environment (need POLYMARKET_PRIVATE_KEY)",
					Exchange: "polymarket",
				})
				return
			}
			res = ExecutePolymarket(intent, polyKey)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(TradeResult{
				Success: false,
				Message: fmt.Sprintf("Unsupported exchange: %s", exchange),
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if res.Success {
			ordersExecuted.Add(1)
			w.WriteHeader(http.StatusOK)
		} else {
			ordersFailed.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		}
		_ = json.NewEncoder(w).Encode(res)
	})

	server := &http.Server{
		Handler: mux,
	}

	// Graceful shutdown listener
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("[INFO] Shutting down broker worker and cleaning up Unix socket...")
		_ = server.Close()
		_ = os.Remove(socketPath)
		os.Exit(0)
	}()

	log.Printf("[INFO] Listening for executive connections on Unix Socket: %s", socketPath)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[FATAL] Broker server failure: %v", err)
	}
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

// routeExchange picks the venue: an explicit Exchange wins, otherwise a "POLY-" ticker (the
// form marketdata publishes for Polymarket tokens) goes to Polymarket and everything else to
// Kalshi. This used to match "btc" anywhere in the ticker, which sent Kalshi's KXBTCD-… to Polymarket.
func routeExchange(intent TradeIntent) string {
	if ex := strings.ToLower(strings.TrimSpace(intent.Exchange)); ex != "" {
		return ex
	}
	if strings.HasPrefix(strings.ToUpper(intent.ContractID), polyTickerPrefix) {
		return "polymarket"
	}
	return "kalshi"
}
