package main

import (
	"math"
	"testing"
	"time"
)

// Friction gates (2026-08-25 desk review, fix #4). Both are about the vig rather than
// the view: friction is charged as a percentage of premium, so a tiny clip pays the same
// 15–20% as a large one with no room to overcome it, and a book taking 20 round trips a
// day pays that toll 20 times.

func TestRiskGate_MinClipNotional(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{MinClipNotional: 200})
	tr.lastNAV = 100000

	// The review's median clip: 100 contracts at 23c = $23 of premium.
	small := &StrategyProposal{ContractID: "KXHIGHNY-26AUG25-T77", Action: "BUY",
		AveragePrice: 0.23, TotalSize: 100}
	if ok, reason := tr.riskGate(small); ok {
		t.Error("a $23 clip should be refused as too small to carry its round trip")
	} else if reason == "" {
		t.Error("rejection must explain itself")
	}

	// 1,000 contracts at 23c = $230, over the floor.
	big := &StrategyProposal{ContractID: "KXHIGHNY-26AUG25-T77", Action: "BUY",
		AveragePrice: 0.23, TotalSize: 1000}
	if ok, reason := tr.riskGate(big); !ok {
		t.Errorf("a $230 clip should pass the floor, rejected: %s", reason)
	}

	// Exits are never blocked by an entry gate — refusing them would trap positions.
	tr.positions["KXHIGHNY-26AUG25-T77"] = &Position{
		ContractID: "KXHIGHNY-26AUG25-T77", EntryPrice: 0.23, Size: 100, Timestamp: time.Now()}
	sell := &StrategyProposal{ContractID: "KXHIGHNY-26AUG25-T77", Action: "SELL",
		AveragePrice: 0.23, TotalSize: 100}
	if ok, reason := tr.riskGate(sell); !ok {
		t.Errorf("a SELL must not be blocked by the clip floor, rejected: %s", reason)
	}
}

func TestRiskGate_MinClipNotionalOffByDefault(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{}) // MinClipNotional 0
	tr.lastNAV = 100000
	tiny := &StrategyProposal{ContractID: "KXHIGHNY-26AUG25-T77", Action: "BUY",
		AveragePrice: 0.05, TotalSize: 10} // $0.50
	if ok, reason := tr.riskGate(tiny); !ok {
		t.Errorf("with the floor at 0 the gate must not bite, rejected: %s", reason)
	}
}

func TestRiskGate_DailyEntryBudget(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{MaxEntriesPerDay: 3})
	tr.lastNAV = 100000
	p := func() *StrategyProposal {
		return &StrategyProposal{ContractID: "KXHIGHNY-26AUG25-T77", Action: "BUY",
			AveragePrice: 0.23, TotalSize: 1000}
	}

	// Budget is spent by FILLS, not by proposals: the gate stays open until entries
	// actually land.
	for i := 0; i < 3; i++ {
		if ok, reason := tr.riskGate(p()); !ok {
			t.Fatalf("entry %d should pass with budget remaining, rejected: %s", i+1, reason)
		}
		tr.mu.Lock()
		tr.noteEntryFilledLocked("KXHIGHNY-26AUG25-T77")
		tr.mu.Unlock()
	}
	if ok, _ := tr.riskGate(p()); ok {
		t.Error("the fourth entry should be refused — the day's budget is spent")
	}

	// Exits still pass: the budget throttles round trips being STARTED, and refusing
	// an exit would strand the positions the budget already paid for.
	tr.positions["KXHIGHNY-26AUG25-T77"] = &Position{
		ContractID: "KXHIGHNY-26AUG25-T77", EntryPrice: 0.23, Size: 100, Timestamp: time.Now()}
	sell := &StrategyProposal{ContractID: "KXHIGHNY-26AUG25-T77", Action: "SELL",
		AveragePrice: 0.23, TotalSize: 100}
	if ok, reason := tr.riskGate(sell); !ok {
		t.Errorf("a SELL must not be blocked by the daily entry budget, rejected: %s", reason)
	}
}

// The counter belongs to a calendar day; a stale stamp reads as zero rather than
// carrying yesterday's spend into today.
func TestEntryBudget_RollsOverAtTheDayBoundary(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{MaxEntriesPerDay: 2})

	tr.mu.Lock()
	tr.noteEntryFilledLocked("KXHIGHNY-26AUG25-T77")
	tr.noteEntryFilledLocked("KXHIGHNY-26AUG25-T77")
	spent := tr.entriesTodayLocked()
	tr.mu.Unlock()
	if spent != 2 {
		t.Fatalf("entriesToday = %d, want 2", spent)
	}

	// Pretend the recorded day is yesterday's.
	tr.mu.Lock()
	tr.entriesDay = dayKey(time.Now().AddDate(0, 0, -1))
	rolled := tr.entriesTodayLocked()
	tr.mu.Unlock()
	if rolled != 0 {
		t.Errorf("a stale day stamp must read as 0 spend, got %d", rolled)
	}

	// And the next fill restarts the count rather than incrementing yesterday's.
	tr.mu.Lock()
	tr.noteEntryFilledLocked("KXHIGHNY-26AUG25-T77")
	after := tr.entriesToday
	tr.mu.Unlock()
	if after != 1 {
		t.Errorf("entriesToday after rollover = %d, want 1", after)
	}
}

// Churn cap (desk review fix #5): one round trip per contract FAMILY per day. Family
// keying is the point — it also closes the sequential sibling-strike hole that
// ONE_STRIKE_PER_FAMILY leaves open once the first strike has closed.
func TestRiskGate_OneRoundTripPerFamilyPerDay(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{MaxFamilyEntriesPerDay: 1})
	tr.lastNAV = 100000

	first := &StrategyProposal{ContractID: "KXGDP-26OCT30-T3.5", Action: "BUY",
		AveragePrice: 0.33, TotalSize: 1000}
	if ok, reason := tr.riskGate(first); !ok {
		t.Fatalf("the day's first round trip on a family must pass, rejected: %s", reason)
	}
	tr.mu.Lock()
	tr.noteEntryFilledLocked(first.ContractID)
	tr.mu.Unlock()

	// Same contract again — the KXHIGHLAX-entered-8-times-in-a-day pattern.
	if ok, _ := tr.riskGate(first); ok {
		t.Error("a second round trip on the same contract must be refused")
	}
	// A SIBLING STRIKE of the same family, which is the GDP -$281 pair: two strikes of
	// one event are the same directional bet, and the old cooldown let the second in.
	sibling := &StrategyProposal{ContractID: "KXGDP-26OCT30-T3.0", Action: "BUY",
		AveragePrice: 0.30, TotalSize: 1000}
	if ok, _ := tr.riskGate(sibling); ok {
		t.Error("a sibling strike of a family that already traded today must be refused")
	}
	// An unrelated family is untouched.
	other := &StrategyProposal{ContractID: "KXHIGHNY-26AUG25-T77", Action: "BUY",
		AveragePrice: 0.23, TotalSize: 1000}
	if ok, reason := tr.riskGate(other); !ok {
		t.Errorf("a different family should be unaffected, rejected: %s", reason)
	}
}

func TestFamilyEntries_RollOverWithTheDay(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{MaxFamilyEntriesPerDay: 1})
	id := "KXGDP-26OCT30-T3.5"

	tr.mu.Lock()
	tr.noteEntryFilledLocked(id)
	spent := tr.familyEntriesTodayLocked(contractFamily(id))
	tr.entriesDay = dayKey(time.Now().AddDate(0, 0, -1))
	rolled := tr.familyEntriesTodayLocked(contractFamily(id))
	tr.mu.Unlock()

	if spent != 1 {
		t.Errorf("family count = %d, want 1", spent)
	}
	if rolled != 0 {
		t.Errorf("yesterday's family count must not bind today, got %d", rolled)
	}
}

// Edge-vs-friction gate (desk review fix #4, the half that needed gordon's fair value
// plumbed through). Gordon checks edge against the book it saw seconds ago and prices
// only the spread; this re-checks at the book breen actually fills on, and prices the
// venue fee too.
func TestRoundTripFriction(t *testing.T) {
	// The review's median contract: 23c, penny-wide book. Fee per leg at 0.23 is
	// ceil(0.07 × 1 × 0.23 × 0.77) = ceil($0.0124) = $0.02, so 4c of fee round trip
	// plus 2c of spread.
	got := roundTripFriction("KXHIGHNY-26AUG25-T77", 0.22, 0.24, defaultKalshiFeeRate, true)
	if math.Abs(got-0.06) > 1e-9 {
		t.Errorf("round-trip friction = %.4f, want 0.06 (2c spread + 4c fees)", got)
	}
	// With the fee model off, only the spread counts.
	if got := roundTripFriction("KXHIGHNY-26AUG25-T77", 0.22, 0.24, defaultKalshiFeeRate, false); math.Abs(got-0.02) > 1e-9 {
		t.Errorf("fee-model-off friction = %.4f, want the 0.02 spread alone", got)
	}
	// A one-sided or crossed book has no measurable spread; fees still count.
	if got := roundTripFriction("KXHIGHNY-26AUG25-T77", 0, 0.24, defaultKalshiFeeRate, true); math.Abs(got-0.04) > 1e-9 {
		t.Errorf("no-bid friction = %.4f, want the 0.04 of fees", got)
	}
}

func TestEntryEdgeBlock(t *testing.T) {
	book := func(bid, ask float64) *OrderBook {
		return &OrderBook{Bids: []PriceLevel{{Price: bid}}, Asks: []PriceLevel{{Price: ask}}}
	}
	const id = "KXHIGHNY-26AUG25-T77"
	// friction on a 0.22/0.24 book = 0.06; at 2× the hurdle is 0.12 of edge.
	cases := []struct {
		name    string
		fair    float64
		mult    float64
		ob      *OrderBook
		blocked bool
	}{
		// fair 0.30 vs ask 0.24 → edge 0.06, under the 0.12 hurdle.
		{"thin edge is refused", 0.30, 2, book(0.22, 0.24), true},
		// fair 0.40 → edge 0.16, clears it.
		{"fat edge passes", 0.40, 2, book(0.22, 0.24), false},
		// Exactly at the hurdle passes (0.36 − 0.24 = 0.12).
		{"exactly at the hurdle passes", 0.36, 2, book(0.22, 0.24), false},
		// A negative edge is refused whatever the multiple.
		{"paying above fair value", 0.20, 2, book(0.22, 0.24), true},
		// Off by default.
		{"multiple 0 disables", 0.30, 0, book(0.22, 0.24), false},
		// A proposal with no fair value is never blocked — the gate must not invent one.
		{"no fair value supplied", 0, 2, book(0.22, 0.24), false},
		{"no book to measure against", 0.40, 2, nil, false},
		{"one-sided book", 0.40, 2, &OrderBook{Asks: []PriceLevel{{Price: 0.24}}}, false},
	}
	for _, c := range cases {
		reason := entryEdgeBlock(id, c.fair, c.ob, c.mult, defaultKalshiFeeRate, true)
		if (reason != "") != c.blocked {
			t.Errorf("%s: blocked=%v (%q), want %v", c.name, reason != "", reason, c.blocked)
		}
	}
}

// The review's actual finding, as a test: at a 33c entry the round-trip fee alone is
// ~3.1c against gordon's 5c MIN_EDGE, so a trade that cleared gordon's gate can still
// be a loser once the vig is priced. This is the case the gate exists to catch.
func TestEntryEdgeBlock_CatchesWhatGordonsMinEdgeLetsThrough(t *testing.T) {
	const id = "KXGDP-26OCT30-T3.5"
	ob := &OrderBook{Bids: []PriceLevel{{Price: 0.32}}, Asks: []PriceLevel{{Price: 0.33}}}

	// Gordon required MIN_EDGE (0.05) + spread (0.01) = 0.06 of edge. A fair value of
	// 0.39 gives exactly 0.06 and would have passed it.
	fair := 0.39
	if edge := fair - 0.33; math.Abs(edge-0.06) > 1e-9 {
		t.Fatalf("precondition: edge should be 0.06, got %.4f", edge)
	}
	// Friction: 1c spread + 2 × ceil(0.07 × 0.33 × 0.67) = 1c + 2×$0.02 = 0.05.
	// At 2× that is a 0.10 hurdle, so the 0.06 edge is refused.
	if reason := entryEdgeBlock(id, fair, ob, 2, defaultKalshiFeeRate, true); reason == "" {
		t.Error("a trade clearing gordon's MIN_EDGE but not 2× friction must be refused here")
	}
	// The same trade passes with a 1× multiple, which is the "just break even on the
	// round trip" setting rather than the review's 2×.
	if reason := entryEdgeBlock(id, fair, ob, 1, defaultKalshiFeeRate, true); reason != "" {
		t.Errorf("at 1× the same edge should pass, got %q", reason)
	}
}

// Family blacklist (desk review fix #6): the landing place for a validated "this family
// is a repeat loser" lesson, which Lou may write at the `intervene` autonomy level.
func TestRiskGate_FamilyBlacklist(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.lastNAV = 100000
	// Lou writes names lowercased; contractFamily keeps the ticker's own casing.
	tr.familyBlacklist = parsePersonaSet("kxgdp-26oct30")

	blocked := &StrategyProposal{ContractID: "KXGDP-26OCT30-T3.5", Action: "BUY",
		AveragePrice: 0.33, TotalSize: 1000}
	if ok, _ := tr.riskGate(blocked); ok {
		t.Error("a blacklisted family must be refused despite the casing difference")
	}
	// A sibling strike shares the family, so it is refused too.
	sibling := &StrategyProposal{ContractID: "KXGDP-26OCT30-T3.0", Action: "BUY",
		AveragePrice: 0.30, TotalSize: 1000}
	if ok, _ := tr.riskGate(sibling); ok {
		t.Error("sibling strikes of a blacklisted family must also be refused")
	}
	// A different expiry is a different family, and is untouched.
	other := &StrategyProposal{ContractID: "KXGDP-26NOV30-T3.5", Action: "BUY",
		AveragePrice: 0.33, TotalSize: 1000}
	if ok, reason := tr.riskGate(other); !ok {
		t.Errorf("a different family should be unaffected, rejected: %s", reason)
	}
	// Exits are never blocked — blacklisting must not strand an open position.
	tr.positions["KXGDP-26OCT30-T3.5"] = &Position{
		ContractID: "KXGDP-26OCT30-T3.5", EntryPrice: 0.33, Size: 100, Timestamp: time.Now()}
	sell := &StrategyProposal{ContractID: "KXGDP-26OCT30-T3.5", Action: "SELL",
		AveragePrice: 0.30, TotalSize: 100}
	if ok, reason := tr.riskGate(sell); !ok {
		t.Errorf("a SELL on a blacklisted family must still pass, rejected: %s", reason)
	}
}

// KXBTCD/KXETHD are Kalshi tickers: a substring match on "btc"/"eth" used to label them
// Polymarket and zero their fees (2026-10-19 review: ~$1.5k of fees never booked).
func TestInferExchange_PrefixOnly(t *testing.T) {
	for id, want := range map[string]string{
		"KXBTCD-26OCT1917-T82999.99": "kalshi",
		"KXETHD-26OCT2017-T2439.99":  "kalshi",
		"KXBTCMAXY-26DEC31-99999.99": "kalshi",
		"POLY-5515972276141801":      "polymarket",
		"poly-123":                   "polymarket",
	} {
		if got := inferExchange(id); got != want {
			t.Errorf("inferExchange(%q) = %q, want %q", id, got, want)
		}
	}
	if fee := kalshiFee("KXBTCD-26OCT1917-T82999.99", 0.5, 100, defaultKalshiFeeRate, true); fee != 1.75 {
		t.Errorf("KXBTCD fee = %.2f, want 1.75", fee)
	}
}
