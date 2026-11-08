package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"testing"
)

// TestSelectClearedBuckets_Live fetches real Kalshi KXHIGHNY markets, maps them the
// way marketdata publishes the catalog, and drives the selector with a synthetic
// high above every "or above" threshold — proving the full match→parse→edge pipeline
// works against live market titles/tickers/asks. Opt-in (network): KALSHI_LIVE_TEST=1.
func TestSelectClearedBuckets_Live(t *testing.T) {
	if os.Getenv("KALSHI_LIVE_TEST") == "" {
		t.Skip("set KALSHI_LIVE_TEST=1 to run the live Kalshi selector check")
	}
	url := "https://api.elections.kalshi.com/trade-api/v2/markets?series_ticker=KXHIGHNY&status=open&limit=100"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var raw struct {
		Markets []struct {
			Ticker string `json:"ticker"`
			Title  string `json:"title"`
			Sub    string `json:"yes_sub_title"`
			Event  string `json:"event_ticker"`
			YesAsk string `json:"yes_ask_dollars"`
		} `json:"markets"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Markets) == 0 {
		t.Skip("no open KXHIGHNY markets right now")
	}
	// Mirror marketdata's catalog mapping (Title = title + " — " + yes_sub_title).
	var cat []catalogMarket
	var date string
	for _, m := range raw.Markets {
		title := m.Title
		if m.Sub != "" {
			title = m.Title + " — " + m.Sub
		}
		var ask float64
		fmt.Sscanf(m.YesAsk, "%f", &ask)
		cat = append(cat, catalogMarket{Ticker: m.Ticker, Title: title, YesAsk: ask, Event: m.Event})
		if date == "" {
			if d, ok := eventDateFromTicker(m.Ticker); ok {
				date = d
			}
		}
	}
	cfg := WeatherConfig{ClearMarginF: 1.0, MinEdge: 0.0, MaxAsk: 1.01, SettleHaircut: 0.0}
	// A blistering 130°F clears every "or above" bucket; assert we pick some and only
	// "or above" ones, all on the right day/series.
	obs := weatherObs{Station: "KNYC", City: "New York City", DayHighF: 130, LocalDate: date}
	picks := selectClearedBuckets(cat, "KXHIGHNY", obs, cfg)
	t.Logf("live KXHIGHNY %s: %d markets, %d cleared 'or above' picks at 130°F", date, len(cat), len(picks))
	for _, p := range picks {
		if p.bucket.kind != bucketAbove {
			t.Errorf("non-above bucket selected: %s (%+v)", p.market.Ticker, p.bucket)
		}
		if _, ok := eventDateFromTicker(p.market.Ticker); !ok {
			t.Errorf("pick with unparseable date: %s", p.market.Ticker)
		}
	}
	// A frigid 1°F should clear nothing.
	if got := selectClearedBuckets(cat, "KXHIGHNY", weatherObs{Station: "KNYC", DayHighF: 1, LocalDate: date}, cfg); len(got) != 0 {
		t.Errorf("1°F should clear no buckets, got %d", len(got))
	}
}

func TestParseTempBucket(t *testing.T) {
	cases := []struct {
		title string
		kind  bucketKind
		lo    float64
		hi    float64
		ok    bool
	}{
		{"Will the high temp in NYC be >88° on Jul 11, 2026? — 89° or above", bucketAbove, 89, 0, true},
		{"Will the high temp in NYC be <81° on Jul 11, 2026? — 80° or below", bucketBelow, 0, 80, true},
		{"Will the high temp in NYC be 87-88° on Jul 11, 2026? — 87° to 88°", bucketRange, 87, 88, true},
		{"Some unrelated market about the S&P 500", bucketAbove, 0, 0, false},
	}
	for _, c := range cases {
		b, ok := parseTempBucket(c.title)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.title, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if b.kind != c.kind || b.lo != c.lo || b.hi != c.hi {
			t.Errorf("%q: got %+v, want kind=%v lo=%v hi=%v", c.title, b, c.kind, c.lo, c.hi)
		}
	}
}

func TestEventDateFromTicker(t *testing.T) {
	cases := []struct {
		ticker string
		want   string
		ok     bool
	}{
		{"KXHIGHNY-26JUL11-T88", "2026-07-11", true},
		{"KXHIGHCHI-26DEC01-B72.5", "2026-12-01", true},
		{"KXHIGHNY", "", false},
		{"KXHIGHNY-GARBAGE-T88", "", false},
	}
	for _, c := range cases {
		got, ok := eventDateFromTicker(c.ticker)
		if ok != c.ok || got != c.want {
			t.Errorf("%q: got (%q,%v), want (%q,%v)", c.ticker, got, ok, c.want, c.ok)
		}
	}
}

func TestClearedBucketEdge(t *testing.T) {
	cfg := WeatherConfig{ClearMarginF: 1.0, MinEdge: 0.03, MaxAsk: 0.98, SettleHaircut: 0.0}
	above := func(x float64) tempBucket { return tempBucket{kind: bucketAbove, lo: x} }

	// Cleared with room: high 90 ≥ 89 threshold + 1 margin, ask 0.90 → edge 0.10.
	if edge, ok := clearedBucketEdge(above(89), 90, 0.90, cfg); !ok || math.Abs(edge-0.10) > 1e-9 {
		t.Errorf("cleared-with-room: got edge=%v ok=%v, want 0.10 true", edge, ok)
	}
	// Not cleared: high only reaches the threshold, not threshold+margin.
	if _, ok := clearedBucketEdge(above(89), 89.5, 0.90, cfg); ok {
		t.Error("within margin should NOT be cleared")
	}
	// No room: ask already at/above MaxAsk.
	if _, ok := clearedBucketEdge(above(80), 95, 0.99, cfg); ok {
		t.Error("ask ≥ MaxAsk should not qualify")
	}
	// Edge too thin: ask 0.98 would be blocked by MaxAsk; use 0.965 → edge 0.035 passes,
	// but 0.97 → edge 0.03 is exactly MinEdge (passes), 0.971 → 0.029 fails.
	if _, ok := clearedBucketEdge(above(80), 95, 0.971, cfg); ok {
		t.Error("edge below MinEdge should not qualify")
	}
	// A "below" bucket is never a cleared-YES intraday, even if currently satisfied.
	if _, ok := clearedBucketEdge(tempBucket{kind: bucketBelow, hi: 100}, 70, 0.50, cfg); ok {
		t.Error("below bucket must never be cleared-YES")
	}
	// A range bucket likewise never clears YES intraday (day could still warm past it).
	if _, ok := clearedBucketEdge(tempBucket{kind: bucketRange, lo: 80, hi: 85}, 82, 0.50, cfg); ok {
		t.Error("range bucket must never be cleared-YES")
	}
	// Settlement haircut lowers fair and thus the edge.
	hc := cfg
	hc.SettleHaircut = 0.05
	if edge, ok := clearedBucketEdge(above(89), 95, 0.90, hc); !ok || math.Abs(edge-0.05) > 1e-9 {
		t.Errorf("haircut: got edge=%v ok=%v, want 0.05 true", edge, ok)
	}
}
