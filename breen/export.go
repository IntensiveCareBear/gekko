package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// RunExporter writes human-readable + machine-readable dumps of the paper run
// to RUN_EXPORT_DIR so the run can be inspected later (CSV, JSONL, text summary).
type RunExporter struct {
	mu       sync.Mutex
	dir      string
	runStart time.Time
	enabled  bool
}

func NewRunExporter(dir string) *RunExporter {
	e := &RunExporter{dir: dir, runStart: time.Now(), enabled: dir != ""}
	if e.enabled {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Printf("[WARNING] Run export disabled — cannot create %s: %v\n", dir, err)
			e.enabled = false
			return e
		}
		e.ensureHeaders()
	}
	return e
}

func (e *RunExporter) path(name string) string { return filepath.Join(e.dir, name) }

// tradesCSVHeader is the current trades.csv schema. Phase 0 appended the excursion
// and cost columns; a file written under an older schema is rotated aside rather than
// continued, so no single CSV ever mixes row widths.
const tradesCSVHeader = "closed_at,contract_id,exchange,side,entry_price,exit_price,size,realized_pnl,return_pct,status,mfe_pct,mae_pct,spread_cost_usd,fee_usd\n"

// ensureHeaders writes CSV headers if the files are new/empty.
func (e *RunExporter) ensureHeaders() {
	rotateStaleTradesCSV(e.path("trades.csv"))
	if fi, err := os.Stat(e.path("trades.csv")); err != nil || fi.Size() == 0 {
		_ = os.WriteFile(e.path("trades.csv"), []byte(tradesCSVHeader), 0o644)
	}
	if fi, err := os.Stat(e.path("equity.csv")); err != nil || fi.Size() == 0 {
		_ = os.WriteFile(e.path("equity.csv"),
			[]byte("timestamp,nav,cash,realized_pnl,unrealized_pnl,open_positions,drawdown_pct\n"), 0o644)
	}
}

// rotateStaleTradesCSV moves aside a trades.csv written under an earlier column set,
// so appending today's wider rows can't produce a ragged file that no spreadsheet or
// dataframe will load. Nothing is deleted — the old file keeps its rows under a
// .legacy-<timestamp> suffix, and trades.jsonl (which Lou reads) is untouched either way.
func rotateStaleTradesCSV(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	header, readErr := bufio.NewReader(f).ReadString('\n')
	f.Close()
	if readErr != nil || header == tradesCSVHeader {
		return
	}
	_ = os.Rename(path, fmt.Sprintf("%s.legacy-%s", path, time.Now().Format("20060102-150405")))
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// AppendTrade records a closed trade to trades.csv + trades.jsonl.
func (e *RunExporter) AppendTrade(t ClosedTrade) {
	if !e.enabled {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	closed := time.UnixMilli(t.ClosedAtMs).Format(time.RFC3339)
	appendLine(e.path("trades.csv"), fmt.Sprintf("%s,%s,%s,%s,%.4f,%.4f,%d,%.2f,%.2f,%s,%.2f,%.2f,%.2f,%.2f\n",
		closed, t.ContractID, t.Exchange, t.Side, t.EntryPrice, t.ExitPrice, t.Size, t.RealizedPnL, t.ReturnPct, t.Status,
		t.MaxFavorableExcursionPct, t.MaxAdverseExcursionPct, t.SpreadCostUSD, t.FeeUSD))
	if data, err := json.Marshal(t); err == nil {
		appendLine(e.path("trades.jsonl"), string(data)+"\n")
	}
}

// AppendEquity records an equity sample to equity.csv + equity.jsonl and refreshes the summary.
func (e *RunExporter) AppendEquity(p EquityPoint, acct AccountState, trades []ClosedTrade) {
	if !e.enabled {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	ts := time.UnixMilli(p.TimestampMs).Format(time.RFC3339)
	appendLine(e.path("equity.csv"), fmt.Sprintf("%s,%.2f,%.2f,%.2f,%.2f,%d,%.2f\n",
		ts, p.NAV, p.Cash, p.RealizedPnL, p.UnrealizedPnL, p.OpenPositions, p.DrawdownPct))
	if data, err := json.Marshal(p); err == nil {
		appendLine(e.path("equity.jsonl"), string(data)+"\n")
	}
	e.writeSummaryLocked(acct, trades)
}

func (e *RunExporter) writeSummaryLocked(acct AccountState, trades []ClosedTrade) {
	wins, losses := 0, 0
	for _, t := range trades {
		if t.RealizedPnL > 0 {
			wins++
		} else {
			losses++
		}
	}
	winRate := 0.0
	if len(trades) > 0 {
		winRate = float64(wins) / float64(len(trades)) * 100
	}

	// Top 5 winners / losers by realized PnL.
	sorted := make([]ClosedTrade, len(trades))
	copy(sorted, trades)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RealizedPnL > sorted[j].RealizedPnL })

	var b []byte
	add := func(format string, args ...interface{}) { b = append(b, []byte(fmt.Sprintf(format, args...))...) }
	add("GEKKO PAPER-TRADING RUN SUMMARY\n")
	add("================================\n")
	add("Generated:        %s\n", time.Now().Format(time.RFC3339))
	add("Run started:      %s\n", e.runStart.Format(time.RFC3339))
	add("Elapsed:          %s\n\n", time.Since(e.runStart).Round(time.Second))
	add("Starting capital: $%.2f\n", acct.StartingCapital)
	add("Current NAV:      $%.2f\n", acct.NAV)
	add("Cash:             $%.2f\n", acct.Cash)
	add("Realized PnL:     $%.2f\n", acct.RealizedPnL)
	add("Unrealized PnL:   $%.2f\n", acct.UnrealizedPnL)
	add("Net PnL:          $%.2f (%.2f%%)\n", acct.NAV-acct.StartingCapital,
		pctChange(acct.StartingCapital, acct.NAV))
	add("Open positions:   %d\n", acct.OpenPositions)
	// Report the running worst drawdown over the whole run, not the instantaneous
	// value at summary-write time (which understated it — post-mortem #11).
	add("Max drawdown:     %.2f%% (current %.2f%%)\n\n", acct.MaxDrawdownPct, acct.DrawdownPct)
	add("Closed trades:    %d (wins %d / losses %d, win rate %.1f%%)\n", len(trades), wins, losses, winRate)
	// Per-status counts so guard activity (stop-loss, loss-limit, force-exit) is
	// visible directly in the summary.
	statusCounts := map[string]int{}
	statuses := make([]string, 0, 4)
	for _, t := range trades {
		if statusCounts[t.Status] == 0 {
			statuses = append(statuses, t.Status)
		}
		statusCounts[t.Status]++
	}
	sort.Strings(statuses)
	add("By status:        ")
	for i, s := range statuses {
		if i > 0 {
			add(" | ")
		}
		add("%s %d", s, statusCounts[s])
	}
	add("\n\n")

	add("TOP WINNERS\n-----------\n")
	for i := 0; i < 5 && i < len(sorted); i++ {
		t := sorted[i]
		add("  %-16s %-4s  PnL $%.2f (%.1f%%)\n", t.ContractID, t.Side, t.RealizedPnL, t.ReturnPct)
	}
	add("\nTOP LOSERS\n----------\n")
	for i := 0; i < 5 && len(sorted)-1-i >= 0; i++ {
		t := sorted[len(sorted)-1-i]
		if t.RealizedPnL >= 0 {
			break
		}
		add("  %-16s %-4s  PnL $%.2f (%.1f%%)\n", t.ContractID, t.Side, t.RealizedPnL, t.ReturnPct)
	}

	_ = os.WriteFile(e.path("run_summary.txt"), b, 0o644)
}

func pctChange(from, to float64) float64 {
	if from == 0 {
		return 0
	}
	return (to - from) / from * 100
}

// Archive moves the current run files into a timestamped subdir, then resets headers.
// A non-empty note is saved as run_note.txt inside the archive.
func (e *RunExporter) Archive(note string) {
	if !e.enabled {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	archiveDir := e.path(fmt.Sprintf("archive-%s", time.Now().Format("20060102-150405")))
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return
	}
	for _, name := range []string{"trades.csv", "trades.jsonl", "equity.csv", "equity.jsonl", "run_summary.txt"} {
		src := e.path(name)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, filepath.Join(archiveDir, name))
		}
	}
	if note != "" {
		_ = os.WriteFile(filepath.Join(archiveDir, "run_note.txt"), []byte(note+"\n"), 0o644)
	}
	e.runStart = time.Now()
	e.ensureHeaders()
}
