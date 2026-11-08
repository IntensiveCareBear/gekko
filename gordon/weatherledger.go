package main

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// pickLedger appends the forecast the predictive path actually priced with, once per
// (station, date, forecast value). runs/weather/history.jsonl records the latest forecast
// of the day, but picks run on the evening-before forecast, so a bias fit on history may
// not describe the forecast that trades. Join this file to official highs to fit the right
// quantity. Best-effort: a write failure is logged once and never blocks pricing.
type pickLedger struct {
	path string
	mu   sync.Mutex
	seen map[string]float64
	warn sync.Once
}

type pickLedgerRow struct {
	TS       time.Time `json:"ts"`
	Station  string    `json:"station"`
	Date     string    `json:"date"`
	Forecast float64   `json:"forecast_high"`
	Bias     float64   `json:"bias_f"`
	Sigma    float64   `json:"sigma_f"` // effective: learned sigma after the global and per-station floors
	Mu       float64   `json:"mu_f"`
	DayHigh  float64   `json:"day_high_f"` // observed so far when priced
}

func newPickLedger(path string) *pickLedger {
	return &pickLedger{path: path, seen: map[string]float64{}}
}

// defaultPickLedgerPath puts the ledger beside the bias table unless WEATHER_PICK_LEDGER is set.
func defaultPickLedgerPath() string {
	return getEnv("WEATHER_PICK_LEDGER",
		filepath.Join(filepath.Dir(getEnv("WEATHER_BIAS_FILE", "runs/weather/bias.json")), "pick_forecasts.jsonl"))
}

// record writes a row when the forecast for this station-date is new or changed. nil-safe.
func (pl *pickLedger) record(row pickLedgerRow) {
	if pl == nil || pl.path == "" {
		return
	}
	k := row.Station + "|" + row.Date
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if last, ok := pl.seen[k]; ok && math.Abs(last-row.Forecast) < 1e-9 {
		return
	}
	pl.seen[k] = row.Forecast
	b, err := json.Marshal(row)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(pl.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, err = f.Write(append(b, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		pl.warn.Do(func() { log.Printf("[WEATHER] pick-forecast ledger write failed (%s): %v", pl.path, err) })
	}
}
