package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBucketYesProb_Above(t *testing.T) {
	// Predicted final high 85±3, running high 80. "84° or above" ⟺ final ≥ 83.5.
	b := tempBucket{kind: bucketAbove, lo: 84}
	p := bucketYesProb(b, 85, 3, 80)
	// Untruncated P(final ≥ 83.5) = 1-Φ((83.5-85)/3) = 1-Φ(-0.5) ≈ 0.691; truncation at
	// 80 (below the mean) barely moves it.
	if !approx(p, 0.70, 0.05) {
		t.Errorf("above prob = %.3f, want ~0.70", p)
	}
}

func TestBucketYesProb_RunningHighForcesCleared(t *testing.T) {
	// Running high already 90, "88° or above" is settled YES regardless of the forecast.
	b := tempBucket{kind: bucketAbove, lo: 88}
	if p := bucketYesProb(b, 85, 3, 90); !approx(p, 1.0, 1e-6) {
		t.Errorf("cleared above prob = %.6f, want 1", p)
	}
	// And a "or below 86" bucket is dead once the high passed 90.
	b2 := tempBucket{kind: bucketBelow, hi: 86}
	if p := bucketYesProb(b2, 85, 3, 90); !approx(p, 0.0, 1e-6) {
		t.Errorf("busted below prob = %.6f, want 0", p)
	}
}

func TestBucketYesProb_RangeIntegrates(t *testing.T) {
	// A wide-enough set of adjacent range buckets should capture almost all the mass
	// above the running high. Predicted 84±2, running high 78.
	mu, sigma, run := 84.0, 2.0, 78.0
	total := 0.0
	for lo := 74; lo <= 94; lo += 2 {
		total += bucketYesProb(tempBucket{kind: bucketRange, lo: float64(lo), hi: float64(lo + 1)}, mu, sigma, run)
	}
	if total < 0.95 {
		t.Errorf("adjacent range buckets summed to %.3f, expected ≈1", total)
	}
}

func TestBiasTable_SeasonedGate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bias.json")
	// No file yet ⇒ no lookup.
	bt := NewBiasTable(path)
	if _, ok := bt.Lookup("KNYC"); ok {
		t.Error("lookup should fail with no bias file")
	}
	// Write a table: KNYC seasoned, KLAX not.
	writeFile(t, path, `[
	  {"station":"KNYC","bias_f":2.5,"sigma_f":3,"n":15,"seasoned":true},
	  {"station":"KLAX","bias_f":-1,"sigma_f":6,"n":4,"seasoned":false}
	]`)
	// Force a reload past the 30s throttle by resetting lastCheck via a fresh table.
	bt = NewBiasTable(path)
	e, ok := bt.Lookup("KNYC")
	if !ok || !approx(e.Bias, 2.5, 1e-9) {
		t.Errorf("seasoned KNYC lookup failed: %+v ok=%v", e, ok)
	}
	if _, ok := bt.Lookup("KLAX"); ok {
		t.Error("unseasoned KLAX must not be returned")
	}
}

func TestSelectPredictiveBuckets_Gating(t *testing.T) {
	dir := t.TempDir()
	biasPath := filepath.Join(dir, "bias.json")
	writeFile(t, biasPath, `[{"station":"KNYC","bias_f":0,"sigma_f":3,"n":20,"seasoned":true}]`)

	cfg := DefaultWeatherConfig()
	cfg.PredictiveMinAsk = 0.02
	ws := &WeatherStrategy{cfg: cfg, bias: NewBiasTable(biasPath), forecasts: newForecastCache()}

	obs := weatherObs{Station: "KNYC", City: "New York City", DayHighF: 80, LocalDate: "2026-07-11"}
	markets := []catalogMarket{
		// "83° to 84°" — with forecast 84 this bucket is near the mode ⇒ underpriced at 0.10.
		{Ticker: "KXHIGHNY-26JUL11-B83.5", Title: "high 83-84 — 83° to 84°", YesAsk: 0.10},
	}

	// No forecast cached yet ⇒ no picks (self-gates).
	if got := ws.selectPredictiveBuckets(markets, "KXHIGHNY", obs, nil); len(got) != 0 {
		t.Fatalf("expected 0 picks without a forecast, got %d", len(got))
	}
	// Cache the forecast; now the underpriced range bucket should be picked.
	ws.forecasts.put("KNYC", "2026-07-11", 84)
	got := ws.selectPredictiveBuckets(markets, "KXHIGHNY", obs, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 predictive pick, got %d", len(got))
	}
	if got[0].edge <= 0 || got[0].prob <= got[0].market.YesAsk {
		t.Errorf("pick should be underpriced YES: %+v", got[0])
	}
	// A ticker already emitted as cleared is skipped.
	if got := ws.selectPredictiveBuckets(markets, "KXHIGHNY", obs, map[string]bool{"KXHIGHNY-26JUL11-B83.5": true}); len(got) != 0 {
		t.Errorf("skip set should suppress the pick, got %d", len(got))
	}
}

func TestSelectPredictiveBuckets_EdgeCapAndSigmaFloor(t *testing.T) {
	dir := t.TempDir()
	biasPath := filepath.Join(dir, "bias.json")
	writeFile(t, biasPath, `[{"station":"KNYC","bias_f":0,"sigma_f":1.5,"n":20,"seasoned":true}]`)
	cfg := DefaultWeatherConfig()
	cfg.PredictiveMaxEdge = 0.20
	ws := &WeatherStrategy{cfg: cfg, bias: NewBiasTable(biasPath), forecasts: newForecastCache()}
	ws.forecasts.put("KNYC", "2026-07-11", 84)
	obs := weatherObs{Station: "KNYC", DayHighF: 80, LocalDate: "2026-07-11"}
	mk := func(ask float64) []catalogMarket {
		return []catalogMarket{{Ticker: "KXHIGHNY-26JUL11-B83.5", Title: "83-84 — 83° to 84°", YesAsk: ask}}
	}
	// Edge (~0.23) above the 0.20 cap (market says 5¢, model says ~28%): refused as model error.
	if got := ws.selectPredictiveBuckets(mk(0.05), "KXHIGHNY", obs, nil); len(got) != 0 {
		t.Errorf("edge above the cap must be skipped, got %+v", got)
	}
	// Learned sigma 1.5 is floored to MinSigmaF: prob must match the floored sigma.
	got := ws.selectPredictiveBuckets(mk(0.20), "KXHIGHNY", obs, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 pick at a plausible edge, got %d", len(got))
	}
	want := bucketYesProb(tempBucket{kind: bucketRange, lo: 83, hi: 84}, 84, cfg.MinSigmaF, 80)
	if !approx(got[0].prob, want, 1e-9) {
		t.Errorf("prob %v, want %v (sigma floored to %v)", got[0].prob, want, cfg.MinSigmaF)
	}
}

func TestStationMinSigma(t *testing.T) {
	cfg := DefaultWeatherConfig()
	if got := cfg.minSigma("KLAX"); got != 5.0 {
		t.Errorf("KLAX floor %v, want 5.0", got)
	}
	if got := cfg.minSigma("KNYC"); got != cfg.MinSigmaF {
		t.Errorf("KNYC floor %v, want global %v", got, cfg.MinSigmaF)
	}
	m := parseStationSigma("kaus=4, bad, KDEN=x, KMIA=-1")
	if len(m) != 1 || m["KAUS"] != 4 {
		t.Errorf("parse: %v", m)
	}
}

func TestPickLedger_RecordsOncePerForecastValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pf.jsonl")
	pl := newPickLedger(path)
	row := pickLedgerRow{TS: time.Now(), Station: "KAUS", Date: "2026-10-16", Forecast: 93, Bias: 0.9, Sigma: 4, Mu: 93.9}
	pl.record(row)
	pl.record(row) // same forecast: deduped
	row.Forecast = 91
	pl.record(row) // forecast moved: new row
	var nilLedger *pickLedger
	nilLedger.record(row) // must not panic
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Errorf("want 2 rows, got %d:\n%s", n, b)
	}
}
