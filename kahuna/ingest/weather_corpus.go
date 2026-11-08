package ingest

import (
	"bufio"
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Weather forward corpus + bias model (Phase 2). METAR history can't be backfilled and
// NWS serves only the current forecast, so (forecast, realized) training pairs must be
// collected FORWARD. Each resolved local day we append one row per station joining the
// day's NWS forecast (from the ForecastStore) with the realized METAR high, then
// recompute a small per-station bias table. The model starts neutral and self-gates
// (Seasoned=false) until it has enough pairs — it lands dark. Mirrors Lou's
// history.jsonl + JSON-table pattern (docs/weather-phase2-forecast-bias.md).

// minSeasonPairs is how many (forecast, realized) pairs a station needs before its bias
// entry is trusted for live predictive pricing. Below this, Seasoned=false.
const minSeasonPairs = 10

// CorpusRecord is one resolved day's training row. ForecastHigh is nil when no NWS
// forecast was captured for that station-date (the estimator skips such rows).
type CorpusRecord struct {
	Station      string    `json:"station"`
	Date         string    `json:"date"`
	ForecastHigh *float64  `json:"forecast_high"`
	RealizedHigh float64   `json:"realized_high"`
	RecordedAt   time.Time `json:"recorded_at"`
}

// BiasEntry is the per-station de-biasing model: add Bias to an NWS forecast to get the
// expected realized high, with Sigma the residual spread for bucket-probability pricing.
type BiasEntry struct {
	Station  string  `json:"station"`
	Bias     float64 `json:"bias_f"`   // mean(realized - forecast)
	Sigma    float64 `json:"sigma_f"`  // stddev of residuals (floored)
	N        int     `json:"n"`        // number of pairs
	Seasoned bool    `json:"seasoned"` // N >= minSeasonPairs (safe for live pricing)
	P10      float64 `json:"p10_f"`    // residual quantiles (for a future richer model)
	P50      float64 `json:"p50_f"`
	P90      float64 `json:"p90_f"`
}

// defaultSigmaF is the wide residual spread used before a station is seasoned (and as a
// floor thereafter): NWS daily-high error is typically a few °F, so a neutral prior of
// ~6°F keeps thin-data pricing from ever looking confident.
const defaultSigmaF = 6.0

// biasWindow is how many of a station's most recent pairs feed its bias: a flat
// average over months carried summer's bias (LAX +3.5°F in Sep) into October's
// opposite sign.
const biasWindow = 30

// computeBiasTable estimates per-station bias + residual spread from the corpus. Pure
// and deterministic — unit-tested. Rows without a forecast are ignored. A station with
// no usable pairs is omitted (gordon treats "absent" as unseasoned/neutral).
func computeBiasTable(records []CorpusRecord) map[string]BiasEntry {
	resid := map[string][]float64{}
	for _, r := range records {
		if r.ForecastHigh == nil {
			continue
		}
		resid[r.Station] = append(resid[r.Station], r.RealizedHigh-*r.ForecastHigh)
	}
	out := make(map[string]BiasEntry, len(resid))
	for station, rs := range resid {
		if len(rs) > biasWindow {
			rs = rs[len(rs)-biasWindow:] // history.jsonl is chronological
		}
		n := len(rs)
		mean := 0.0
		for _, x := range rs {
			mean += x
		}
		mean /= float64(n)
		sigma := defaultSigmaF
		if n >= 2 {
			var ss float64
			for _, x := range rs {
				d := x - mean
				ss += d * d
			}
			s := math.Sqrt(ss / float64(n-1))
			if s > sigma { // floor at the neutral prior; never let thin data look overconfident
				sigma = s
			} else if n >= minSeasonPairs {
				sigma = math.Max(s, 1.5) // seasoned stations may legitimately be tighter, but keep a small floor
			}
		}
		sorted := append([]float64(nil), rs...)
		sort.Float64s(sorted)
		out[station] = BiasEntry{
			Station:  station,
			Bias:     mean,
			Sigma:    sigma,
			N:        n,
			Seasoned: n >= minSeasonPairs,
			P10:      quantile(sorted, 0.10),
			P50:      quantile(sorted, 0.50),
			P90:      quantile(sorted, 0.90),
		}
	}
	return out
}

// quantile returns the linear-interpolated q-quantile of a sorted slice.
func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// CorpusRecorder appends resolved (forecast, realized) pairs to history.jsonl and
// recomputes bias.json. Safe for concurrent calls (the METAR poll goroutine).
type CorpusRecorder struct {
	mu          sync.Mutex
	historyPath string
	biasPath    string
	store       *ForecastStore
	done        map[string]bool // "station|date" already recorded this process (dedupe)
}

// NewCorpusRecorder builds a recorder and seeds its dedupe set from existing history.
func NewCorpusRecorder(historyPath, biasPath string, store *ForecastStore) *CorpusRecorder {
	cr := &CorpusRecorder{
		historyPath: historyPath,
		biasPath:    biasPath,
		store:       store,
		done:        map[string]bool{},
	}
	for _, r := range cr.readHistory() {
		cr.done[forecastKey(r.Station, r.Date)] = true
	}
	return cr
}

// RecordResolved finalizes one station-day: joins the realized high with the stored
// forecast, appends the pair to history, and recomputes the bias table. A (station,
// date) already recorded is a no-op (idempotent across restarts).
func (cr *CorpusRecorder) RecordResolved(station, date string, realizedHigh float64) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	key := forecastKey(station, date)
	if cr.done[key] {
		return
	}
	rec := CorpusRecord{Station: station, Date: date, RealizedHigh: realizedHigh, RecordedAt: time.Now()}
	if fr, ok := cr.store.Get(station, date); ok {
		h := fr.HighF
		rec.ForecastHigh = &h
	}
	if err := cr.appendHistory(rec); err != nil {
		log.Printf("[WARNING] Weather corpus: append history failed: %v", err)
		return
	}
	cr.done[key] = true
	fh := "n/a"
	if rec.ForecastHigh != nil {
		fh = strconv.FormatFloat(*rec.ForecastHigh, 'f', 1, 64)
	}
	log.Printf("[INFO] Weather corpus: resolved %s %s realized %.1f°F (forecast %s°F) — %d pairs total",
		station, date, realizedHigh, fh, len(cr.done))
	cr.recomputeBias()
}

func (cr *CorpusRecorder) appendHistory(rec CorpusRecord) error {
	if err := os.MkdirAll(filepath.Dir(cr.historyPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(cr.historyPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func (cr *CorpusRecorder) readHistory() []CorpusRecord {
	f, err := os.Open(cr.historyPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []CorpusRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r CorpusRecord
		if json.Unmarshal(line, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// recomputeBias rebuilds bias.json from the full history (atomic write). Caller holds cr.mu.
func (cr *CorpusRecorder) recomputeBias() {
	table := computeBiasTable(cr.readHistory())
	entries := make([]BiasEntry, 0, len(table))
	for _, e := range table {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Station < entries[j].Station })
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cr.biasPath), 0o755); err != nil {
		log.Printf("[WARNING] Weather corpus: mkdir for bias table failed: %v", err)
		return
	}
	tmp := cr.biasPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("[WARNING] Weather corpus: write bias table failed: %v", err)
		return
	}
	if err := os.Rename(tmp, cr.biasPath); err != nil {
		log.Printf("[WARNING] Weather corpus: rename bias table failed: %v", err)
	}
}
