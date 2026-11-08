package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// Health counters, updated from the /execute handler in main.go.
var (
	healthStartTime = time.Now()
	requestsTotal   atomic.Int64
	ordersExecuted  atomic.Int64
	ordersFailed    atomic.Int64
)

// HealthResponse is the common shape every Gekko component returns from /healthz.
type HealthResponse struct {
	Component     string                 `json:"component"`
	Status        string                 `json:"status"`
	UptimeSeconds float64                `json:"uptime_seconds"`
	Stats         map[string]interface{} `json:"stats"`
}

// startHealthServer exposes /healthz on a dedicated TCP port so the liveview
// aggregator (and humans) can reach it independently of the Unix order socket.
func startHealthServer(addr string, kalshiReady, polyReady bool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Component:     "broker-worker",
			Status:        "ok",
			UptimeSeconds: time.Since(healthStartTime).Seconds(),
			Stats: map[string]interface{}{
				"requests_total":        requestsTotal.Load(),
				"orders_executed":       ordersExecuted.Load(),
				"orders_failed":         ordersFailed.Load(),
				"kalshi_configured":     kalshiReady,
				"polymarket_configured": polyReady,
			},
		})
	})

	go func() {
		log.Printf("[INFO] Broker health endpoint listening on %s/healthz", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("[WARNING] Broker health server stopped: %v", err)
		}
	}()
}
