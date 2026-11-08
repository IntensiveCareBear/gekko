package main

import (
	"math"
	"sort"
	"testing"
	"time"
)

func TestTradeableStatus(t *testing.T) {
	for _, s := range []string{"", "active", "open", "Active"} {
		if !tradeableStatus(s) {
			t.Errorf("status %q should be tradeable", s)
		}
	}
	for _, s := range []string{"settled", "closed", "finalized", "inactive"} {
		if tradeableStatus(s) {
			t.Errorf("status %q should NOT be tradeable", s)
		}
	}
}

func TestStaleTickers(t *testing.T) {
	embedded := map[string]bool{"KXCPI-26JUL": true, "KXGONE-26JUN": true, "POLY-111": true, "POLY-222": true}
	current := map[string]bool{"KXCPI-26JUL": true, "POLY-111": true}

	// Both catalogs fetched: everything absent from current is stale.
	got := staleTickers(embedded, current, true, true)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "KXGONE-26JUN" || got[1] != "POLY-222" {
		t.Errorf("stale = %v, want [KXGONE-26JUN POLY-222]", got)
	}

	// Polymarket catalog missing this cycle: its tickers must NOT be pruned.
	got = staleTickers(embedded, current, true, false)
	if len(got) != 1 || got[0] != "KXGONE-26JUN" {
		t.Errorf("stale with poly unfetched = %v, want [KXGONE-26JUN]", got)
	}

	// Kalshi catalog missing: only poly tickers prunable.
	got = staleTickers(embedded, current, false, true)
	if len(got) != 1 || got[0] != "POLY-222" {
		t.Errorf("stale with kalshi unfetched = %v, want [POLY-222]", got)
	}
}

func TestVerifyMatch(t *testing.T) {
	tests := []struct {
		name         string
		news, title  string
		wantPenalty  float64
		wantWarnings bool
	}{
		{
			name:  "clean match, no penalty",
			news:  "Bitcoin surges as traders bet BTC tops $100k before the January deadline in 2026.",
			title: "Will BTC be above $100,000 on Jan 31, 2026?",
		},
		{
			name:        "month mismatch (Jan news vs Feb market)",
			news:        "Bitcoin rallies toward $100k with the January cutoff approaching.",
			title:       "Will BTC be above $100k by February?",
			wantPenalty: monthMismatchPenalty,
		},
		{
			name:        "year mismatch",
			news:        "In 2026 the S&P is expected to close above 6000.",
			title:       "Will the S&P 500 close above 6000 in 2027?",
			wantPenalty: yearMismatchPenalty,
		},
		{
			name:        "threshold mismatch (90k title vs 100k news)",
			news:        "Bitcoin pushes toward the $100k milestone this quarter.",
			title:       "Will BTC be above $90,000?",
			wantPenalty: thresholdMismatchPenalty,
		},
		{
			name:  "terse title, no entities — fail open",
			news:  "The Federal Reserve is widely expected to hold rates steady.",
			title: "Fed decision",
		},
		{
			name:  "threshold present on both sides via k-normalization",
			news:  "Ether could clear 4000 dollars by summer.",
			title: "Will ETH be above $4k?",
		},
		{
			name:        "compound mismatch trips the drop threshold",
			news:        "Bitcoin nears $100k with a January 2026 resolution.",
			title:       "Will BTC top $90k by February 2027?",
			wantPenalty: yearMismatchPenalty + monthMismatchPenalty + thresholdMismatchPenalty,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			penalty, warnings := verifyMatch(tc.news, tc.title)
			if math.Abs(penalty-tc.wantPenalty) > 1e-9 {
				t.Errorf("penalty = %.2f, want %.2f (warnings: %v)", penalty, tc.wantPenalty, warnings)
			}
			if (len(warnings) > 0) != (tc.wantPenalty > 0) {
				t.Errorf("warnings=%v but penalty=%.2f", warnings, penalty)
			}
		})
	}
}

func TestExtractThresholds(t *testing.T) {
	got := extractThresholds("BTC above $100k or 100,000 or maybe 5% in 2026")
	// 100k and 100,000 both normalize to 100000; 5% stays 5; 2026 excluded as a year.
	if !got[100000] || !got[5] {
		t.Errorf("extractThresholds missing normalized values: %v", sortedFloats(got))
	}
	if got[2026] {
		t.Errorf("extractThresholds kept a bare year 2026: %v", sortedFloats(got))
	}
}

func TestQuoteTradeable(t *testing.T) {
	defer func(lo, hi float64) { minEntryPrice, maxEntryPrice = lo, hi }(minEntryPrice, maxEntryPrice)
	minEntryPrice, maxEntryPrice = 0.20, 0.55
	for _, c := range []struct {
		bid, ask float64
		want     bool
	}{
		{0.48, 0.49, true},
		{0.27, 0.29000000000000004, true}, // 2c abs tolerance, float noise
		{0.24, 0.27, false},               // the 10-01 Brent book: 12% of mid
		{0.50, 0.54, true},                // 4c but under 10% of mid
		{0, 0.01, false},                  // one-sided
		{0.30, 0, false},
		{0.10, 0.11, false}, // below band
		{0.60, 0.61, false}, // above band
	} {
		if got, why := quoteTradeable(c.bid, c.ask); got != c.want {
			t.Errorf("quoteTradeable(%.2f, %.2f) = %v (%s), want %v", c.bid, c.ask, got, why, c.want)
		}
	}
}

func TestBestQuote(t *testing.T) {
	// Kalshi books arrive with empty/unsorted levels; take the true top.
	ob := &OrderBook{
		Bids: []PriceLevel{{0.20, 10}, {0.31, 5}, {0.40, 0}},
		Asks: []PriceLevel{{0.99, 7}, {0.33, 3}},
	}
	if b, a := bestQuote(ob); b != 0.31 || a != 0.33 {
		t.Errorf("bestQuote = %.2f/%.2f, want 0.31/0.33", b, a)
	}
	if b, a := bestQuote(&OrderBook{Asks: []PriceLevel{{0.01, 5}}}); b != 0 || a != 0.01 {
		t.Errorf("one-sided bestQuote = %.2f/%.2f, want 0/0.01", b, a)
	}
}

func TestContractExpiry(t *testing.T) {
	defer func(m *MarketMatcher) { marketMatcher = m }(marketMatcher)
	closeAt := time.Date(2026, 10, 2, 12, 30, 0, 0, time.UTC)
	marketMatcher = &MarketMatcher{closes: map[string]time.Time{"KXCPI-26SEP-T0.2": closeAt}}

	// Catalog close time covers tickers the parser cannot date.
	if _, ok := tickerExpiry("KXCPI-26SEP-T0.2"); ok {
		t.Fatal("precondition: monthly ticker should not parse")
	}
	if got, ok := contractExpiry("KXCPI-26SEP-T0.2"); !ok || !got.Equal(closeAt) {
		t.Errorf("contractExpiry = %v, %v; want catalog close %v", got, ok, closeAt)
	}
	// No catalog entry: fall back to the ticker.
	want, _ := tickerExpiry("KXBTCD-26JUL0917-T100000")
	if got, ok := contractExpiry("KXBTCD-26JUL0917-T100000"); !ok || !got.Equal(want) {
		t.Errorf("fallback = %v, %v; want %v", got, ok, want)
	}
}
