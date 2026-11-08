package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

var (
	healthStart       = time.Now()
	pollsTotal        atomic.Int64
	booksWritten      atomic.Int64
	pollErrors        atomic.Int64
	watchedTickers    atomic.Int64
	priorityTickers   atomic.Int64 // held + watch tickers refreshed every cycle
	lastRotationSecs  atomic.Int64 // duration of the last completed universe pass
	lastPollUnix      atomic.Int64
	marketsDiscovered atomic.Int64
)

// HealthResponse is the common shape every Gekko component returns from /healthz.
type HealthResponse struct {
	Component     string                 `json:"component"`
	Status        string                 `json:"status"`
	UptimeSeconds float64                `json:"uptime_seconds"`
	Stats         map[string]interface{} `json:"stats"`
}

func startHealthServer(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		lastPoll := lastPollUnix.Load()
		lastAgo := 0.0
		if lastPoll > 0 {
			lastAgo = time.Since(time.UnixMilli(lastPoll)).Seconds()
		}
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Component:     "marketdata",
			Status:        "ok",
			UptimeSeconds: time.Since(healthStart).Seconds(),
			Stats: map[string]interface{}{
				"markets_discovered": marketsDiscovered.Load(),
				"watched_tickers":    watchedTickers.Load(),
				"priority_tickers":   priorityTickers.Load(),
				"rotation_seconds":   lastRotationSecs.Load(),
				"poll_cycles":        pollsTotal.Load(),
				"books_written":      booksWritten.Load(),
				"poll_errors":        pollErrors.Load(),
				"seconds_since_poll": lastAgo,
				"mode":               "read-only",
			},
		})
	})
	go func() {
		log.Printf("[INFO] Marketdata health endpoint on %s/healthz", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("[WARNING] Marketdata health server stopped: %v", err)
		}
	}()
}
