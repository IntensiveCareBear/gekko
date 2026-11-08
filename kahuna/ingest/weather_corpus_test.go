package ingest

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestComputeBiasTable_BiasAndSeasoning(t *testing.T) {
	var recs []CorpusRecord
	// KNYC: 12 pairs, realized always forecast+3 ⇒ bias +3, near-zero spread, seasoned.
	for i := 0; i < 12; i++ {
		recs = append(recs, CorpusRecord{Station: "KNYC", Date: "2026-07-01", ForecastHigh: fptr(80), RealizedHigh: 83})
	}
	// KLAX: only 3 pairs ⇒ not seasoned.
	for i := 0; i < 3; i++ {
		recs = append(recs, CorpusRecord{Station: "KLAX", ForecastHigh: fptr(75), RealizedHigh: 74})
	}
	// KDEN: a row with no forecast must be ignored (leaves KDEN out entirely).
	recs = append(recs, CorpusRecord{Station: "KDEN", ForecastHigh: nil, RealizedHigh: 90})

	tbl := computeBiasTable(recs)

	ny, ok := tbl["KNYC"]
	if !ok {
		t.Fatal("KNYC missing from table")
	}
	if math.Abs(ny.Bias-3) > 1e-9 {
		t.Errorf("KNYC bias = %v, want 3", ny.Bias)
	}
	if !ny.Seasoned {
		t.Errorf("KNYC should be seasoned at N=%d", ny.N)
	}
	if ny.Sigma < 1.5 {
		t.Errorf("KNYC sigma %v should respect the floor", ny.Sigma)
	}

	lax, ok := tbl["KLAX"]
	if !ok {
		t.Fatal("KLAX missing from table")
	}
	if lax.Seasoned {
		t.Error("KLAX should NOT be seasoned at N=3")
	}
	if lax.Sigma < defaultSigmaF {
		t.Errorf("thin-data KLAX sigma %v should be at least the neutral prior %v", lax.Sigma, defaultSigmaF)
	}

	if _, ok := tbl["KDEN"]; ok {
		t.Error("KDEN had no forecasted pairs and must be omitted")
	}
}

func TestComputeBiasTable_RecentWindow(t *testing.T) {
	var recs []CorpusRecord
	for i := 0; i < 40; i++ { // old regime: +5, then the last biasWindow pairs: -2
		r := 85.0
		if i >= 40-biasWindow {
			r = 78
		}
		recs = append(recs, CorpusRecord{Station: "KLAX", ForecastHigh: fptr(80), RealizedHigh: r})
	}
	if b := computeBiasTable(recs)["KLAX"]; math.Abs(b.Bias+2) > 1e-9 || b.N != biasWindow {
		t.Errorf("bias=%v n=%d, want -2 over the last %d pairs", b.Bias, b.N, biasWindow)
	}
}

func TestQuantile(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5}
	if got := quantile(s, 0.5); got != 3 {
		t.Errorf("median = %v, want 3", got)
	}
	if got := quantile(s, 0); got != 1 {
		t.Errorf("p0 = %v, want 1", got)
	}
	if got := quantile(s, 1); got != 5 {
		t.Errorf("p100 = %v, want 5", got)
	}
}

func TestCorpusRecorder_AppendDedupeAndBias(t *testing.T) {
	dir := t.TempDir()
	histPath := filepath.Join(dir, "history.jsonl")
	biasPath := filepath.Join(dir, "bias.json")
	store := NewForecastStore(filepath.Join(dir, "forecasts.json"))
	store.Put(ForecastRecord{Station: "KNYC", Date: "2026-07-10", HighF: 80, CapturedAt: time.Now()})

	cr := NewCorpusRecorder(histPath, biasPath, store)
	cr.RecordResolved("KNYC", "2026-07-10", 84) // realized 84, forecast 80 ⇒ resid +4
	cr.RecordResolved("KNYC", "2026-07-10", 99) // duplicate (station,date) ⇒ ignored

	recs := cr.readHistory()
	if len(recs) != 1 {
		t.Fatalf("history has %d rows, want 1 (dedupe)", len(recs))
	}
	if recs[0].ForecastHigh == nil || *recs[0].ForecastHigh != 80 {
		t.Errorf("forecast not joined: %+v", recs[0])
	}
	if recs[0].RealizedHigh != 84 {
		t.Errorf("realized = %v, want 84", recs[0].RealizedHigh)
	}

	// A fresh recorder over the same files must see the pair already recorded (restart-safe).
	cr2 := NewCorpusRecorder(histPath, biasPath, store)
	cr2.RecordResolved("KNYC", "2026-07-10", 100)
	if recs := cr2.readHistory(); len(recs) != 1 {
		t.Errorf("restart dedupe failed: %d rows", len(recs))
	}

	if _, err := os.Stat(biasPath); err != nil {
		t.Errorf("bias.json not written: %v", err)
	}
}

func TestCorpusRecorder_MissingForecastStillRecords(t *testing.T) {
	dir := t.TempDir()
	store := NewForecastStore(filepath.Join(dir, "forecasts.json")) // empty
	cr := NewCorpusRecorder(filepath.Join(dir, "h.jsonl"), filepath.Join(dir, "b.json"), store)
	cr.RecordResolved("KMIA", "2026-07-10", 90)
	recs := cr.readHistory()
	if len(recs) != 1 || recs[0].ForecastHigh != nil {
		t.Fatalf("want 1 row with nil forecast, got %+v", recs)
	}
}
