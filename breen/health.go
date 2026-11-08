package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

var healthStartTime = time.Now()

// HealthResponse is the common shape every Gekko component returns from /healthz.
type HealthResponse struct {
	Component     string                 `json:"component"`
	Status        string                 `json:"status"`
	UptimeSeconds float64                `json:"uptime_seconds"`
	Stats         map[string]interface{} `json:"stats"`
}

// startHealthServer exposes /healthz reporting breen state. Breen
// has no other HTTP surface, so it runs on its own dedicated port.
func startHealthServer(addr string, tracker *PositionTracker, ledger *ImmutableLedger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		tracker.mu.RLock()
		positions := len(tracker.positions)
		auto := tracker.autoApprove
		dry := tracker.dryMode
		tracker.mu.RUnlock()

		blockCount := 0
		if blocks, err := ledger.GetBlocks(); err == nil {
			blockCount = len(blocks)
		}

		_ = json.NewEncoder(w).Encode(HealthResponse{
			Component:     "breen",
			Status:        "ok",
			UptimeSeconds: time.Since(healthStartTime).Seconds(),
			Stats: map[string]interface{}{
				"orders_executed":    tracker.ordersExecuted.Load(),
				"proposals_rejected": tracker.proposalsRejected.Load(),
				"consensus_orders":   tracker.consensusOrders.Load(),
				"consensus_skipped":  tracker.consensusSkipped.Load(),
				"no_quote_marks":     tracker.noQuoteMarks.Load(),
				"one_sided_marks":    tracker.oneSidedMarks.Load(),
				"active_positions":   positions,
				"ledger_blocks":      blockCount,
				"dry_mode":           dry,
				"auto_approve":       auto,
			},
		})
	})

	go func() {
		log.Printf("[INFO] Breen health endpoint listening on %s/healthz", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("[WARNING] Breen health server stopped: %v", err)
		}
	}()
}
