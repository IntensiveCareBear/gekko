package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ClosedTrade mirrors the schema breen writes to runs/**/trades.jsonl (one JSON
// object per closed position). Lou only reads; the writer lives in breen.
type ClosedTrade struct {
	ContractID    string         `json:"contract_id"`
	Exchange      string         `json:"exchange"`
	Side          string         `json:"side"`
	EntryPrice    float64        `json:"entry_price"`
	ExitPrice     float64        `json:"exit_price"`
	Size          int            `json:"size"`
	RealizedPnL   float64        `json:"realized_pnl"`
	ReturnPct     float64        `json:"return_pct"`
	OpenedAtMS    int64          `json:"opened_at_ms"`
	ClosedAtMS    int64          `json:"closed_at_ms"`
	Status        string         `json:"status"`
	Persona       string         `json:"persona"`
	Contributors  []string       `json:"contributors"`
	PersonaScores []PersonaScore `json:"persona_scores"`

	// Run tags which archive the trade came from ("current" for the live run's
	// top-level files). Set at load time, never serialized.
	Run string `json:"-"`
}

// PersonaScore is one contributing persona's own-proposal outcome on a close
// (breen's per-persona scorecard data).
type PersonaScore struct {
	Persona     string  `json:"persona"`
	RealizedPnL float64 `json:"realized_pnl"`
	ReturnPct   float64 `json:"return_pct"`
	Cost        float64 `json:"cost"`
}

// HoldSeconds is the position's lifetime. 0 when either timestamp is missing.
func (t ClosedTrade) HoldSeconds() float64 {
	if t.OpenedAtMS <= 0 || t.ClosedAtMS <= t.OpenedAtMS {
		return 0
	}
	return float64(t.ClosedAtMS-t.OpenedAtMS) / 1000.0
}

var monthAbbr = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

// contractExpiryUnixMS parses the resolution instant encoded in a Kalshi ticker's
// event segment (2nd dash-part: YYMMMDD optionally followed by an intraday time)
// and returns it as unix ms, ok=false for tickers without a parseable date
// (POLY-<token>, synthetics). It mirrors gordon's tickerExpiry — including the
// bare-HHMM / bare-HH intraday forms used by the sub-hourly crypto series — so
// Lou measures the same "time to resolution" the entry guard does. UTC.
func contractExpiryUnixMS(contractID string) (int64, bool) {
	parts := strings.Split(contractID, "-")
	if len(parts) < 2 {
		return 0, false
	}
	seg := strings.ToUpper(parts[1])
	if len(seg) < 7 {
		return 0, false
	}
	yy, err1 := strconv.Atoi(seg[0:2])
	mon, ok := monthAbbr[seg[2:5]]
	dd, err2 := strconv.Atoi(seg[5:7])
	if err1 != nil || !ok || err2 != nil || dd < 1 || dd > 31 {
		return 0, false
	}
	hour, min := 23, 59
	parseHM := func(s string) (int, int, bool) {
		h, e1 := strconv.Atoi(s[0:2])
		m, e2 := strconv.Atoi(s[2:4])
		if e1 != nil || e2 != nil || h >= 24 || m >= 60 {
			return 0, 0, false
		}
		return h, m, true
	}
	switch rest := seg[7:]; {
	case len(rest) >= 5 && rest[0] == 'H':
		if h, m, ok := parseHM(rest[1:5]); ok {
			hour, min = h, m
		}
	case len(rest) == 4:
		if h, m, ok := parseHM(rest); ok {
			hour, min = h, m
		}
	case len(rest) == 2:
		if h, e := strconv.Atoi(rest); e == nil && h < 24 {
			hour, min = h, 0
		}
	}
	// Kalshi encodes resolution times in US Eastern (mirrors gordon's tickerExpiry);
	// parsing as UTC put intraday expiries 4–5h off.
	return time.Date(2000+yy, time.Month(mon), dd, hour, min, 0, 0, kalshiTZ).UnixMilli(), true
}

// kalshiTZ is the exchange clock (US Eastern, with DST), resolved once with a
// fixed-EST fallback when tzdata is unavailable.
var kalshiTZ = func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.FixedZone("EST", -5*3600)
}()

// LifeAtEntrySeconds is how much life the contract had left when the position was
// opened (resolution − open). Returns (0,false) when the ticker has no parseable
// expiry or the open timestamp is missing — the caller skips those.
func (t ClosedTrade) LifeAtEntrySeconds() (float64, bool) {
	if t.OpenedAtMS <= 0 {
		return 0, false
	}
	expMS, ok := contractExpiryUnixMS(t.ContractID)
	if !ok {
		return 0, false
	}
	life := float64(expMS-t.OpenedAtMS) / 1000.0
	if life <= 0 {
		return 0, false
	}
	return life, true
}

// contractFamily collapses a strike ladder to its SERIES-EVENT stem, matching
// breen's grouping (breen/tracker.go): drop the last dash segment when the id
// has ≥3 segments; POLY token ids and short ids pass through whole.
func contractFamily(contractID string) string {
	parts := strings.Split(contractID, "-")
	if len(parts) >= 3 {
		return strings.Join(parts[:len(parts)-1], "-")
	}
	return contractID
}

// discoverRuns lists the run directories under root, oldest first: every
// archive-* subdirectory containing a trades.jsonl, plus the in-progress run's
// top-level files as "current" when present. A missing root is an error; a run
// with no trades yet is simply skipped.
func discoverRuns(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("runs root %s: %w", root, err)
	}
	var runs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "archive-") {
			if _, err := os.Stat(filepath.Join(root, e.Name(), "trades.jsonl")); err == nil {
				runs = append(runs, e.Name())
			}
		}
	}
	sort.Strings(runs) // archive-YYYYMMDD-HHMMSS sorts chronologically
	if _, err := os.Stat(filepath.Join(root, "trades.jsonl")); err == nil {
		runs = append(runs, "current")
	}
	return runs, nil
}

// loadTrades reads one run's trades.jsonl, tagging each trade with the run id.
// Malformed lines are counted and skipped, not fatal — a mid-run file can have a
// partial last line.
func loadTrades(root, run string) (trades []ClosedTrade, skipped int, err error) {
	path := filepath.Join(root, "trades.jsonl")
	if run != "current" {
		path = filepath.Join(root, run, "trades.jsonl")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var t ClosedTrade
		if json.Unmarshal([]byte(line), &t) != nil || t.ContractID == "" {
			skipped++
			continue
		}
		t.Run = run
		trades = append(trades, t)
	}
	return trades, skipped, sc.Err()
}

// loadAllRuns loads every discovered run's trades into one slice (chronological
// by run, then file order — breen appends closes in time order).
func loadAllRuns(root string) (trades []ClosedTrade, runs []string, skipped int, err error) {
	runs, err = discoverRuns(root)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, run := range runs {
		rt, sk, err := loadTrades(root, run)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("run %s: %w", run, err)
		}
		skipped += sk
		trades = append(trades, rt...)
	}
	return trades, runs, skipped, nil
}
