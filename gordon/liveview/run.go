package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handleLedgerStats counts BUY vs SELL entries across the full immutable ledger
// file (NDJSON, one TransactionBlock per line). Only "open" entries are counted
// so each position is counted once regardless of how many iceberg fills it has.
func handleLedgerStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ledgerPath := getEnv("LEDGER_FILE_PATH", "")
	if ledgerPath == "" {
		w.Write([]byte(`{"buys":0,"sells":0}`)) //nolint:errcheck
		return
	}

	f, err := os.Open(ledgerPath)
	if err != nil {
		w.Write([]byte(`{"buys":0,"sells":0}`)) //nolint:errcheck
		return
	}
	defer f.Close()

	var buys, sells int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var block struct {
			Action string `json:"action"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(sc.Bytes(), &block); err != nil {
			continue
		}
		if block.Status != "DRY_OPEN" && block.Status != "LIVE_OPEN" {
			continue
		}
		switch block.Action {
		case "BUY":
			buys++
		case "SELL":
			sells++
		}
	}

	json.NewEncoder(w).Encode(map[string]int{"buys": buys, "sells": sells}) //nolint:errcheck
}

// handleAccount returns the paper-trading account snapshot (gekko:account).
func handleAccount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	defaultAcct := `{"starting_capital":0,"cash":0,"nav":0,"realized_pnl":0,"unrealized_pnl":0,"open_positions":0,"peak_nav":0,"drawdown_pct":0}`
	if redisClient == nil {
		w.Write([]byte(defaultAcct))
		return
	}
	val, err := redisClient.Get(r.Context(), "gekko:account").Result()
	if err != nil {
		w.Write([]byte(defaultAcct))
		return
	}
	w.Write([]byte(val))
}

// handleTrades returns the closed-trade history (gekko:trades).
func handleTrades(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if redisClient == nil {
		w.Write([]byte("[]"))
		return
	}
	val, err := redisClient.Get(r.Context(), "gekko:trades").Result()
	if err != nil {
		w.Write([]byte("[]"))
		return
	}
	w.Write([]byte(val))
}

// trmnlWindow is one time-scoped set of trade metrics for the TRMNL display.
// Values are pre-formatted strings so the Liquid template stays dumb; the raw
// *Positive booleans drive red/green styling on the sign.
type trmnlWindow struct {
	PnL          string  `json:"pnl"`           // signed dollar string, e.g. "+142.18"
	PnLValue     float64 `json:"pnl_value"`     // raw for any client-side logic
	PnLPositive  bool    `json:"pnl_positive"`  // pnl_value >= 0
	ProfitFactor string  `json:"profit_factor"` // "1.62", or "—" when undefined
	WinRate      string  `json:"win_rate"`      // integer percent, e.g. "58", or "—"
	Trades       int     `json:"trades"`
}

// trmnlTrade is the minimal slice of a ClosedTrade the TRMNL endpoint needs.
type trmnlTrade struct {
	RealizedPnL float64 `json:"realized_pnl"`
	ClosedAtMs  int64   `json:"closed_at_ms"`
}

// trmnlMetrics buckets closed trades into today / this-week windows and computes
// P&L, profit factor and win rate for each. Mirrors breen's book-metrics math
// (gross wins / gross losses; win rate = wins/total) applied per time window.
func trmnlMetrics(trades []trmnlTrade, now time.Time) (today, week trmnlWindow) {
	loc := now.Location()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	// Go's Weekday() is Sunday=0..Saturday=6; roll back to the most recent Monday.
	daysSinceMonday := (int(midnight.Weekday()) + 6) % 7
	weekStart := midnight.AddDate(0, 0, -daysSinceMonday)

	var todayTrades, weekTrades []trmnlTrade
	for _, t := range trades {
		if t.ClosedAtMs == 0 {
			continue
		}
		closed := time.UnixMilli(t.ClosedAtMs)
		if !closed.Before(weekStart) {
			weekTrades = append(weekTrades, t)
			if !closed.Before(midnight) {
				todayTrades = append(todayTrades, t)
			}
		}
	}
	return trmnlWindowFor(todayTrades), trmnlWindowFor(weekTrades)
}

func trmnlWindowFor(trades []trmnlTrade) trmnlWindow {
	w := trmnlWindow{PnL: "0.00", PnLPositive: true, ProfitFactor: "—", WinRate: "—", Trades: len(trades)}
	if len(trades) == 0 {
		return w
	}
	var pnl, grossWin, grossLoss float64
	wins := 0
	for _, t := range trades {
		pnl += t.RealizedPnL
		if t.RealizedPnL > 0 {
			wins++
			grossWin += t.RealizedPnL
		} else {
			grossLoss += -t.RealizedPnL
		}
	}
	w.PnLValue = pnl
	w.PnLPositive = pnl >= 0
	w.PnL = fmt.Sprintf("%+.2f", pnl)
	w.WinRate = fmt.Sprintf("%.0f", float64(wins)/float64(len(trades))*100)
	if grossLoss > 0 {
		w.ProfitFactor = fmt.Sprintf("%.2f", grossWin/grossLoss)
	} else if grossWin > 0 {
		w.ProfitFactor = "∞" // wins, no losses yet
	}
	return w
}

// handleTRMNL serves a compact, pre-formatted metrics payload for a TRMNL e-ink
// display, wrapped in the {"merge_variables": …} envelope its private-plugin
// webhook expects. Sourced from gekko:trades, scoped to today and this week.
func handleTRMNL(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	now := time.Now()
	var trades []trmnlTrade
	if redisClient != nil {
		if val, err := redisClient.Get(r.Context(), "gekko:trades").Result(); err == nil {
			_ = json.Unmarshal([]byte(val), &trades)
		}
	}
	today, week := trmnlMetrics(trades, now)

	payload := map[string]interface{}{
		"merge_variables": map[string]interface{}{
			"today":     today,
			"week":      week,
			"updated":   now.Format("Jan 2 15:04"),
			"timestamp": now.UnixMilli(),
		},
	}
	json.NewEncoder(w).Encode(payload) //nolint:errcheck
}

// handleEquityCurve returns the equity-curve samples (gekko:equity_curve list) as a JSON array.
func handleEquityCurve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if redisClient == nil {
		w.Write([]byte("[]"))
		return
	}
	items, err := redisClient.LRange(r.Context(), "gekko:equity_curve", 0, -1).Result()
	if err != nil || len(items) == 0 {
		w.Write([]byte("[]"))
		return
	}
	// Each list element is already a JSON object; assemble them into an array.
	w.Write([]byte("[" + strings.Join(items, ",") + "]"))
}

// handleRunReset asks breen to reset the paper run back to its starting capital.
func handleRunReset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if redisClient == nil {
		http.Error(w, "Redis not connected", http.StatusInternalServerError)
		return
	}
	// An optional note in the body rides along on the reset command so breen can
	// archive it next to the run files (run_note.txt).
	var body struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	cmd := "reset_run"
	if body.Note != "" {
		cmd += ":" + body.Note
	}
	if err := redisClient.Publish(r.Context(), "gekko:commands", cmd).Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success":true,"message":"Run reset command sent to breen"}`))
}

// handleRunLiquidate asks breen to close all open positions at their current marks.
// Used by the reset flow before the export bundle so the closing trades are included.
func handleRunLiquidate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if redisClient == nil {
		http.Error(w, "Redis not connected", http.StatusInternalServerError)
		return
	}
	if err := redisClient.Publish(r.Context(), "gekko:commands", "liquidate_all").Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success":true,"message":"Liquidate command sent to breen"}`))
}

// allowedExportFiles is the whitelist of run-log files downloadable via /api/run/export.
var allowedExportFiles = map[string]string{
	"trades.csv":      "text/csv",
	"equity.csv":      "text/csv",
	"run_summary.txt": "text/plain",
	"trades.jsonl":    "application/x-ndjson",
	"equity.jsonl":    "application/x-ndjson",
}

// handleRunExport serves a whitelisted run-log file from RUN_EXPORT_DIR as a download.
func handleRunExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")

	name := r.URL.Query().Get("file")
	ctype, ok := allowedExportFiles[name]
	if !ok {
		http.Error(w, "Unknown export file", http.StatusBadRequest)
		return
	}

	dir := getEnv("RUN_EXPORT_DIR", "./runs")
	path := filepath.Join(dir, name) // name is whitelisted, no traversal possible
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "Export file not available yet", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Write(data)
}

// handleRunBundle zips the whole run — settings, account, trades, positions,
// equity curve and the run-log files, plus an optional user note — and serves
// it as a single download for offline analysis. POST body: {"note": "..."}.
func handleRunBundle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	addFile := func(name string, data []byte) {
		if f, err := zw.Create(name); err == nil {
			_, _ = f.Write(data)
		}
	}

	// Run-log files written by breen's exporter.
	dir := getEnv("RUN_EXPORT_DIR", "./runs")
	for name := range allowedExportFiles {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			addFile(name, data)
		}
	}

	// Live state from Redis.
	if redisClient != nil {
		ctx := r.Context()
		for name, key := range map[string]string{
			"settings.json":      "gekko:run_config",
			"account.json":       "gekko:account",
			"trades.json":        "gekko:trades",
			"positions.json":     "gekko:positions",
			"persona_stats.json": "gekko:persona_stats",
			"book_metrics.json":  "gekko:book_metrics",
			"calibration.json":   "gekko:calibration",
		} {
			if val, err := redisClient.Get(ctx, key).Result(); err == nil {
				addFile(name, []byte(val))
			}
		}
		if items, err := redisClient.LRange(ctx, "gekko:equity_curve", 0, -1).Result(); err == nil && len(items) > 0 {
			addFile("equity_curve.json", []byte("["+strings.Join(items, ",")+"]"))
		}
	}

	addFile("run_note.txt", []byte(fmt.Sprintf("exported_at: %s\nnote: %s\n",
		time.Now().Format(time.RFC3339), body.Note)))

	if err := zw.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	filename := fmt.Sprintf("gekko-run-%s.zip", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	_, _ = w.Write(buf.Bytes())
}
