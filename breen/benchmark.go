package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const benchmarkKey = "gekko:benchmark"

// defaultBenchmarkQuoteURL is a keyless Yahoo Finance chart endpoint; %s is the
// ticker. Override with BENCHMARK_QUOTE_URL (must contain one %s for the ticker).
const defaultBenchmarkQuoteURL = "https://query1.finance.yahoo.com/v8/finance/chart/%s?range=1d&interval=1d"

// defaultBenchmarkHistoryURL is the keyless Yahoo history endpoint: %s ticker,
// %d period1 (sec), %d period2 (sec), %s interval. Used to backfill the existing
// equity curve so the benchmark line spans the whole run.
const defaultBenchmarkHistoryURL = "https://query1.finance.yahoo.com/v8/finance/chart/%s?period1=%d&period2=%d&interval=%s"

// BenchmarkState is the persisted buy-and-hold anchor: the price and seed capital
// at run start, which fixes the share count for the comparison index (default SPY).
type BenchmarkState struct {
	Ticker      string  `json:"ticker"`
	AnchorPrice float64 `json:"anchor_price"`
	AnchorTsMs  int64   `json:"anchor_ts_ms"`
	Shares      float64 `json:"shares"`
	Capital     float64 `json:"capital"`
	Backfilled  bool    `json:"backfilled"`
}

type pricePoint struct {
	tsMs  int64
	close float64
}

// BenchmarkTracker answers "how would the seed money have done in SPY?": it polls
// the comparison ticker's live price (keyless) and marks a buy-and-hold position
// opened with the run's starting capital at the anchor price. NAV = shares × price.
type BenchmarkTracker struct {
	rdb        *redis.Client
	ticker     string
	quoteURL   string
	historyURL string
	client     *http.Client

	mu        sync.RWMutex
	state     BenchmarkState
	lastPrice float64
}

func NewBenchmarkTracker(rdb *redis.Client, ticker, quoteURL, historyURL string) *BenchmarkTracker {
	if ticker == "" {
		ticker = "SPY"
	}
	if quoteURL == "" {
		quoteURL = defaultBenchmarkQuoteURL
	}
	if historyURL == "" {
		historyURL = defaultBenchmarkHistoryURL
	}
	return &BenchmarkTracker{
		rdb:        rdb,
		ticker:     ticker,
		quoteURL:   quoteURL,
		historyURL: historyURL,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Prime anchors the buy-and-hold position synchronously at startup so the first
// equity sample already carries benchmark data. On a fresh run it anchors at the
// run-start price and backfills the existing equity curve so the line spans the
// whole run; on restart it just refreshes the live price against the saved anchor.
func (b *BenchmarkTracker) Prime(ctx context.Context, capital float64) {
	b.loadState(ctx)
	b.mu.RLock()
	// Treat a non-backfilled anchor (e.g. written by an older build) as not-yet
	// anchored so we backfill the existing curve once.
	anchored := b.state.AnchorPrice > 0 && b.state.Backfilled
	b.mu.RUnlock()
	if anchored {
		if px, err := b.fetchPrice(ctx); err == nil {
			b.mu.Lock()
			b.lastPrice = px
			b.mu.Unlock()
		}
		return
	}
	if b.backfillFromRunStart(ctx, capital) {
		return
	}
	// Fallback: no history (or no existing curve) — anchor at the current price.
	b.tick(ctx, capital)
}

// run refreshes the live price on an interval (intraday moves on the latest
// point). Anchoring/backfill happens once in Prime.
func (b *BenchmarkTracker) run(ctx context.Context, refresh time.Duration, capital float64) {
	t := time.NewTicker(refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.tick(ctx, capital)
		}
	}
}

// backfillFromRunStart anchors at the SPY price as of the run's first equity
// sample and rewrites every existing equity point with its historical benchmark
// value, so the comparison line covers the full run. Returns false (caller falls
// back to a live anchor) if there's no curve or history is unavailable.
func (b *BenchmarkTracker) backfillFromRunStart(ctx context.Context, capital float64) bool {
	if b.rdb == nil {
		return false
	}
	items, err := b.rdb.LRange(ctx, equityCurveKey, 0, -1).Result()
	if err != nil || len(items) == 0 {
		return false
	}
	var first EquityPoint
	if json.Unmarshal([]byte(items[0]), &first) != nil || first.TimestampMs == 0 {
		return false
	}
	nowMs := time.Now().UnixMilli()
	hist, err := b.historicalPrices(ctx, first.TimestampMs, nowMs)
	if err != nil || len(hist) == 0 {
		log.Printf("[BENCHMARK] history fetch failed (%v); will anchor at current price instead.", err)
		return false
	}
	anchorPrice := priceAtOrBefore(hist, first.TimestampMs)
	if anchorPrice <= 0 {
		return false
	}
	shares := capital / anchorPrice

	updated := 0
	for i, raw := range items {
		var p EquityPoint
		if json.Unmarshal([]byte(raw), &p) != nil {
			continue
		}
		px := priceAtOrBefore(hist, p.TimestampMs)
		if px <= 0 {
			px = anchorPrice
		}
		p.BenchmarkNAV = math.Round(shares*px*100) / 100
		if data, err := json.Marshal(p); err == nil {
			if b.rdb.LSet(ctx, equityCurveKey, int64(i), string(data)).Err() == nil {
				updated++
			}
		}
	}

	b.mu.Lock()
	b.lastPrice = hist[len(hist)-1].close
	b.state = BenchmarkState{
		Ticker: b.ticker, AnchorPrice: anchorPrice, AnchorTsMs: first.TimestampMs,
		Shares: shares, Capital: capital, Backfilled: true,
	}
	b.persistLocked(ctx)
	b.mu.Unlock()
	log.Printf("[BENCHMARK] backfilled %d/%d equity points; anchored $%.0f of %s at $%.2f (run start) → %.4f shares.",
		updated, len(items), capital, b.ticker, anchorPrice, shares)
	return true
}

func (b *BenchmarkTracker) tick(ctx context.Context, capital float64) {
	price, err := b.fetchPrice(ctx)
	if err != nil {
		log.Printf("[BENCHMARK] %s quote failed: %v", b.ticker, err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastPrice = price
	if b.state.AnchorPrice <= 0 {
		b.anchorLocked(ctx, price, capital)
	}
}

// anchorLocked fixes the share count from a price + capital. Caller holds b.mu.
func (b *BenchmarkTracker) anchorLocked(ctx context.Context, price, capital float64) {
	if price <= 0 || capital <= 0 {
		return
	}
	b.state = BenchmarkState{
		Ticker:      b.ticker,
		AnchorPrice: price,
		AnchorTsMs:  time.Now().UnixMilli(),
		Shares:      capital / price,
		Capital:     capital,
	}
	b.persistLocked(ctx)
	log.Printf("[BENCHMARK] anchored: $%.2f of %s at $%.2f → %.4f shares (buy-and-hold).",
		capital, b.ticker, price, b.state.Shares)
}

// Reset re-anchors the benchmark to "now" for a fresh run. Uses the last known
// price if available; otherwise clears so the next quote re-anchors.
func (b *BenchmarkTracker) Reset(ctx context.Context, capital float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastPrice > 0 {
		b.anchorLocked(ctx, b.lastPrice, capital)
		return
	}
	b.state = BenchmarkState{Ticker: b.ticker}
	if b.rdb != nil {
		_ = b.rdb.Del(ctx, benchmarkKey).Err()
	}
}

// BenchmarkNAV returns the current buy-and-hold value (shares × latest price) and
// whether it's available (anchored and a price is known).
func (b *BenchmarkTracker) BenchmarkNAV() (float64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.state.AnchorPrice <= 0 || b.lastPrice <= 0 {
		return 0, false
	}
	return b.state.Shares * b.lastPrice, true
}

func (b *BenchmarkTracker) fetchPrice(ctx context.Context) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(b.quoteURL, b.ticker), nil)
	if err != nil {
		return 0, err
	}
	// Yahoo rejects requests without a browser-like User-Agent.
	req.Header.Set("User-Agent", "Mozilla/5.0 (gekko-benchmark)")
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	var yr struct {
		Chart struct {
			Result []struct {
				Meta struct {
					RegularMarketPrice float64 `json:"regularMarketPrice"`
				} `json:"meta"`
			} `json:"result"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(body, &yr); err != nil {
		return 0, err
	}
	if len(yr.Chart.Result) == 0 || yr.Chart.Result[0].Meta.RegularMarketPrice <= 0 {
		return 0, fmt.Errorf("no price in response")
	}
	return yr.Chart.Result[0].Meta.RegularMarketPrice, nil
}

// historicalPrices fetches the ticker's close series over [startMs, endMs]. It
// uses hourly granularity for short spans (so intraday moves show) and daily for
// long ones. Returns ascending-by-time points with nulls dropped.
func (b *BenchmarkTracker) historicalPrices(ctx context.Context, startMs, endMs int64) ([]pricePoint, error) {
	interval := "1d"
	if endMs-startMs <= 60*86_400_000 { // ≤ 60 days → hourly
		interval = "1h"
	}
	// Pad the start back a few days so there's a close at/before the run start
	// (covers weekends/holidays when the market was shut).
	p1 := startMs/1000 - 7*86_400
	p2 := endMs/1000 + 86_400
	url := fmt.Sprintf(b.historyURL, b.ticker, p1, p2, interval)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (gekko-benchmark)")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var yr struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Quote []struct {
						Close []*float64 `json:"close"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(body, &yr); err != nil {
		return nil, err
	}
	if len(yr.Chart.Result) == 0 || len(yr.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, fmt.Errorf("no history in response")
	}
	r := yr.Chart.Result[0]
	closes := r.Indicators.Quote[0].Close
	out := make([]pricePoint, 0, len(r.Timestamp))
	for i, ts := range r.Timestamp {
		if i >= len(closes) || closes[i] == nil || *closes[i] <= 0 {
			continue
		}
		out = append(out, pricePoint{tsMs: ts * 1000, close: *closes[i]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid closes")
	}
	return out, nil
}

// priceAtOrBefore returns the close of the latest history point at/before tsMs
// (the most recent known price as of that moment), or the earliest close when
// every point is newer. Assumes hist is ascending by time.
func priceAtOrBefore(hist []pricePoint, tsMs int64) float64 {
	if len(hist) == 0 {
		return 0
	}
	price := hist[0].close
	for _, p := range hist {
		if p.tsMs <= tsMs {
			price = p.close
		} else {
			break
		}
	}
	return price
}

func (b *BenchmarkTracker) loadState(ctx context.Context) {
	if b.rdb == nil {
		return
	}
	val, err := b.rdb.Get(ctx, benchmarkKey).Result()
	if err != nil {
		return
	}
	var s BenchmarkState
	if json.Unmarshal([]byte(val), &s) == nil && s.AnchorPrice > 0 {
		b.mu.Lock()
		b.state = s
		b.mu.Unlock()
	}
}

// persistLocked writes the anchor to Redis. Caller holds b.mu.
func (b *BenchmarkTracker) persistLocked(ctx context.Context) {
	if b.rdb == nil {
		return
	}
	if data, err := json.Marshal(b.state); err == nil {
		_ = b.rdb.Set(ctx, benchmarkKey, string(data), 0).Err()
	}
}
