package main

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"sync"
	"time"
)

// Weather Phase 2 (docs/weather-phase2-forecast-bias.md): gordon's read side of the
// forecast-bias model. kahuna accumulates the forward (forecast, realized) corpus and
// writes a per-station bias table (bias.json); this reads it — mtime-cached like the
// Lou lesson book — and turns an NWS forecast + running high into a fair probability
// for each still-live temperature bucket. Fail-safe: no table, or an unseasoned
// station, means no predictive trade (Phase 1 cleared-bucket trading is unaffected).

// weatherBiasEntry mirrors kahuna's BiasEntry (kahuna/ingest/weather_corpus.go is the
// writer and source of truth). Only the fields predictive pricing needs.
type weatherBiasEntry struct {
	Station  string  `json:"station"`
	Bias     float64 `json:"bias_f"`
	Sigma    float64 `json:"sigma_f"`
	N        int     `json:"n"`
	Seasoned bool    `json:"seasoned"`
}

// BiasTable caches bias.json, reloading when its mtime changes (checked ≤ every 30s;
// kahuna rewrites it at most a few times a day).
type BiasTable struct {
	mu        sync.Mutex
	path      string
	entries   map[string]weatherBiasEntry
	mtime     time.Time
	lastCheck time.Time
}

// NewBiasTable builds the reader for the given bias.json path.
func NewBiasTable(path string) *BiasTable {
	return &BiasTable{path: path, entries: map[string]weatherBiasEntry{}}
}

// Lookup returns the seasoned bias entry for a station, or ok=false when the table is
// missing, the station is absent, or the station isn't seasoned yet.
func (bt *BiasTable) Lookup(station string) (weatherBiasEntry, bool) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	bt.refreshLocked(time.Now())
	e, ok := bt.entries[station]
	if !ok || !e.Seasoned {
		return weatherBiasEntry{}, false
	}
	return e, true
}

func (bt *BiasTable) refreshLocked(now time.Time) {
	if now.Sub(bt.lastCheck) < 30*time.Second {
		return
	}
	bt.lastCheck = now
	st, err := os.Stat(bt.path)
	if err != nil {
		bt.entries = map[string]weatherBiasEntry{} // table gone (run reset) — stop pricing off it
		return
	}
	if st.ModTime().Equal(bt.mtime) {
		return
	}
	raw, err := os.ReadFile(bt.path)
	if err != nil {
		return
	}
	var all []weatherBiasEntry
	if err := json.Unmarshal(raw, &all); err != nil {
		return // likely caught mid-rewrite; next check retries
	}
	m := make(map[string]weatherBiasEntry, len(all))
	seasoned := 0
	for _, e := range all {
		m[e.Station] = e
		if e.Seasoned {
			seasoned++
		}
	}
	bt.entries = m
	bt.mtime = st.ModTime()
	log.Printf("[WEATHER] Loaded bias table: %d stations (%d seasoned) from %s", len(all), seasoned, bt.path)
}

// forecastCache holds the latest NWS daily-high forecast per (station, date), fed by
// WEATHER_FORECAST events. Small and bounded (7 stations × a couple of days).
type forecastCache struct {
	mu sync.Mutex
	m  map[string]float64 // "station|date" -> forecast high °F
}

func newForecastCache() *forecastCache { return &forecastCache{m: map[string]float64{}} }

func (fc *forecastCache) put(station, date string, high float64) {
	fc.mu.Lock()
	fc.m[station+"|"+date] = high
	fc.mu.Unlock()
}

func (fc *forecastCache) get(station, date string) (float64, bool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	h, ok := fc.m[station+"|"+date]
	return h, ok
}

// normCDF is the standard-normal CDF Φ(z).
func normCDF(z float64) float64 { return 0.5 * math.Erfc(-z/math.Sqrt2) }

// bucketYesProb returns P(YES) for a temperature bucket given a predicted final-high
// distribution Normal(mu, sigma) truncated below at runningHigh (the final high can't
// fall below what's already been observed today). Bucket edges use a ±0.5°F rounding
// correction because Kalshi settles on the integer-degree high. Pure.
func bucketYesProb(b tempBucket, mu, sigma, runningHigh float64) float64 {
	if sigma <= 0 {
		sigma = 0.5
	}
	// Truncated-normal CDF of the final high, P(final ≤ x | final ≥ runningHigh).
	zR := (runningHigh - mu) / sigma
	denom := 1 - normCDF(zR) // mass above the running high
	cdf := func(x float64) float64 {
		if x < runningHigh {
			return 0
		}
		if denom < 1e-9 {
			// Running high already sits deep in the upper tail: final ≈ runningHigh.
			if x >= runningHigh {
				return 1
			}
			return 0
		}
		return (normCDF((x-mu)/sigma) - normCDF(zR)) / denom
	}
	switch b.kind {
	case bucketAbove: // YES if integer high ≥ lo  ⟺ final ≥ lo-0.5
		return 1 - cdf(b.lo-0.5)
	case bucketBelow: // YES if integer high ≤ hi  ⟺ final ≤ hi+0.5
		return cdf(b.hi + 0.5)
	case bucketRange: // YES if lo ≤ integer high ≤ hi
		return cdf(b.hi+0.5) - cdf(b.lo-0.5)
	}
	return 0
}
