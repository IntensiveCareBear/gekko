package main

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestResolveMark_Tiering(t *testing.T) {
	fresh := markEntry{Price: 0.33, At: time.Now().Add(-5 * time.Minute)}
	stale := markEntry{Price: 0.33, At: time.Now().Add(-45 * time.Minute)}
	maxAge := 30 * time.Minute

	cases := []struct {
		name           string
		live           float64
		quality        quoteQuality
		cached         markEntry
		cachedOK       bool
		forRealization bool
		wantPrice      float64
		wantSource     string
	}{
		{"live wins over everything", 0.41, quoteTwoSided, fresh, true, true, 0.41, "live"},
		{"fresh cached mark used when no live book", 0, quoteNone, fresh, true, true, 0.33, "cached"},
		{"stale cached mark ignored → haircut for realization", 0, quoteNone, stale, true, true, 0.20, "haircut"},
		{"no quote at all → haircut for realization", 0, quoteNone, markEntry{}, false, true, 0.20, "haircut"},
		{"no quote for NAV snapshot → entry (no haircut oscillation)", 0, quoteNone, markEntry{}, false, false, 0.40, "entry"},
		{"fresh cached mark also serves NAV", 0, quoteNone, fresh, true, false, 0.33, "cached"},
		// A one-sided book beats the cache: it is observed now, the cache is a stale mid.
		// This is the 2026-08-30 weather bug — a dying contract kept its flattering mid.
		{"one-sided quote outranks a fresh cached mid", 0.02, quoteOneSided, fresh, true, false, 0.02, "onesided"},
		{"one-sided quote outranks the haircut floor", 0.02, quoteOneSided, stale, true, true, 0.02, "onesided"},
		{"one-sided quote of zero is not a price → falls through", 0, quoteOneSided, markEntry{}, false, false, 0.40, "entry"},
	}
	for _, c := range cases {
		price, source := resolveMark(c.live, c.quality, c.cached, c.cachedOK, 0.40, 0.5, maxAge, c.forRealization)
		if math.Abs(price-c.wantPrice) > 1e-9 || source != c.wantSource {
			t.Errorf("%s: got (%.4f, %s), want (%.4f, %s)", c.name, price, source, c.wantPrice, c.wantSource)
		}
	}
}

// TestQuoteFromBook_Sidedness pins the tiering a book gets graded into. The one-sided
// cases are the 2026-08-30 regression: these used to grade as "no book at all".
func TestQuoteFromBook_Sidedness(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-30 * time.Second).UnixMilli()
	bid := []PriceLevel{{Price: 0.20, Volume: 100}}
	ask := []PriceLevel{{Price: 0.30, Volume: 100}}

	cases := []struct {
		name        string
		ob          *OrderBook
		wantPrice   float64
		wantQuality quoteQuality
	}{
		{"two-sided → mid", &OrderBook{Bids: bid, Asks: ask, AsOfMs: fresh}, 0.25, quoteTwoSided},
		{"bids only → best bid (the price we could sell at)", &OrderBook{Bids: bid, AsOfMs: fresh}, 0.20, quoteOneSided},
		{"asks only → best ask (an upper bound, nobody is bidding)", &OrderBook{Asks: ask, AsOfMs: fresh}, 0.30, quoteOneSided},
		{"empty book → nothing", &OrderBook{AsOfMs: fresh}, 0, quoteNone},
		{"absent book → nothing", nil, 0, quoteNone},
		{"stale book → nothing, even two-sided", &OrderBook{Bids: bid, Asks: ask, AsOfMs: now.Add(-10 * time.Minute).UnixMilli()}, 0, quoteNone},
		{"unstamped book keeps TTL semantics", &OrderBook{Bids: bid, Asks: ask}, 0.25, quoteTwoSided},
	}
	for _, c := range cases {
		price, quality := quoteFromBook(c.ob, 4*time.Minute, now)
		if math.Abs(price-c.wantPrice) > 1e-9 || quality != c.wantQuality {
			t.Errorf("%s: got (%.4f, %v), want (%.4f, %v)", c.name, price, quality, c.wantPrice, c.wantQuality)
		}
	}
}

// TestExcursion_TracksDyingContractOnOneSidedBook is the 2026-08-30 weather bug in
// miniature. KXHIGHCHI-26AUG30-T86 was bought at 0.89, lost its bid as the day decided
// it, resolved at $0.00 — and recorded a max adverse excursion of −0.56%, because a
// one-sided book counted as no observation and the position's WorstMark never moved off
// entry. The excursion record has to follow the price down even when only one side is
// quoted.
func TestExcursion_TracksDyingContractOnOneSidedBook(t *testing.T) {
	pos := &Position{ContractID: "KXHIGHCHI-26AUG30-T86", EntryPrice: 0.89, Size: 96}

	// The book goes ask-only as the contract dies: bids vanish, offers walk to nothing.
	for _, askPrice := range []float64{0.60, 0.30, 0.08, 0.02} {
		ob := &OrderBook{
			Asks:   []PriceLevel{{Price: askPrice, Volume: 50}},
			AsOfMs: time.Now().UnixMilli(),
		}
		price, quality := quoteFromBook(ob, 4*time.Minute, time.Now())
		if quality == quoteNone {
			t.Fatalf("ask-only book at %.2f graded as no quote — the position stops being observed", askPrice)
		}
		pos.observeMark(price)
	}

	_, mae := excursionPct(pos, 0)
	if mae > -90.0 {
		t.Errorf("MAE = %.2f%%, want ≤ -90%%: a position that went 0.89 → 0.02 must record the fall, "+
			"not the -0.56%% the one-sided book used to hide", mae)
	}
}

func TestForceExit_NoQuoteHaircut(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.allowMockBook = false // no live books, no cached marks
	tr.forceExitWindow = time.Hour

	id := "KXDASH-26AUGORDERS-P1020000000"
	tr.positions[id] = &Position{
		ContractID: id, EntryPrice: 0.40, Size: 1000,
		Timestamp: time.Now(), Expiry: time.Now().Add(30 * time.Minute),
	}

	tr.forceExitNearExpiry(context.Background())

	if _, open := tr.positions[id]; open {
		t.Fatal("position within the force-exit window should have been closed")
	}
	last := tr.closedTrades[len(tr.closedTrades)-1]
	if last.Status != "FORCE_EXIT" {
		t.Fatalf("expected FORCE_EXIT close, got %+v", last)
	}
	// No quote → entry × 0.5 haircut, never a flat "at entry" fill.
	if math.Abs(last.ExitPrice-0.20) > 1e-9 {
		t.Errorf("no-quote force-exit should mark at 0.20 (entry 0.40 × 0.5), got %.4f", last.ExitPrice)
	}
	if math.Abs(last.RealizedPnL-(-200)) > 1e-6 {
		t.Errorf("expected -$200 realized on the haircut close, got $%.2f", last.RealizedPnL)
	}
}

func TestForceExit_UsesFreshCachedMark(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.allowMockBook = false
	tr.forceExitWindow = time.Hour

	id := "KXWTIW-26JUL0214-B75.50"
	tr.positions[id] = &Position{
		ContractID: id, EntryPrice: 0.40, Size: 1000,
		Timestamp: time.Now(), Expiry: time.Now().Add(30 * time.Minute),
	}
	// A live mark seen 5 minutes ago outranks the haircut floor.
	tr.markCache[id] = markEntry{Price: 0.31, At: time.Now().Add(-5 * time.Minute)}

	tr.forceExitNearExpiry(context.Background())

	last := tr.closedTrades[len(tr.closedTrades)-1]
	if math.Abs(last.ExitPrice-0.31) > 1e-9 {
		t.Errorf("force-exit should use the fresh cached mark 0.31, got %.4f", last.ExitPrice)
	}
}

func TestSnapshot_NoQuoteKeepsEntryAndCounts(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.allowMockBook = false

	id := "KXFEAR-26JUL02-XGREE"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.50, Size: 100, Timestamp: time.Now()}

	acct := tr.snapshotAccount(context.Background())

	if math.Abs(acct.UnrealizedPnL) > 1e-9 {
		t.Errorf("NAV snapshot with no quote should value at entry (0 unrealized), got $%.2f", acct.UnrealizedPnL)
	}
	if got := tr.noQuoteMarks.Load(); got != 1 {
		t.Errorf("no-quote counter should report 1 position, got %d", got)
	}
	if _, cached := tr.cachedMark(id); cached {
		t.Error("markCache must hold LIVE marks only — entry fallback must not be cached")
	}
}

// TestBidFromBook is the exit-side half of the sidedness question (2026-09-03). Every
// position is long, so an exit crosses the BID and the ask side has no say in whether
// it can happen. The two cases that matter are the two the old two-sided rule conflated.
func TestBidFromBook(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-30 * time.Second).UnixMilli()
	bid := []PriceLevel{{Price: 0.20, Volume: 100}}
	ask := []PriceLevel{{Price: 0.30, Volume: 100}}

	cases := []struct {
		name      string
		ob        *OrderBook
		wantPrice float64
		wantOK    bool
	}{
		{"two-sided → the bid, not the mid", &OrderBook{Bids: bid, Asks: ask, AsOfMs: fresh}, 0.20, true},
		// The 2026-09-03 case: KXHIGHCHI-26SEP03-B92.5 carried 34,616 lots bid at 0.99
		// and zero asks. Maximally exitable, and the old rule refused it.
		{"bids only → exitable", &OrderBook{Bids: []PriceLevel{{Price: 0.99, Volume: 34616}}, AsOfMs: fresh}, 0.99, true},
		{"asks only → nobody is buying, no exit", &OrderBook{Asks: ask, AsOfMs: fresh}, 0, false},
		{"empty book → no exit", &OrderBook{AsOfMs: fresh}, 0, false},
		{"absent book → no exit", nil, 0, false},
		{"stale book → no exit", &OrderBook{Bids: bid, Asks: ask, AsOfMs: now.Add(-10 * time.Minute).UnixMilli()}, 0, false},
		{"unstamped book keeps TTL semantics", &OrderBook{Bids: bid, Asks: ask}, 0.20, true},
		{"zero bid is not a bid", &OrderBook{Bids: []PriceLevel{{Price: 0, Volume: 10}}, AsOfMs: fresh}, 0, false},
	}
	for _, c := range cases {
		price, ok := bidFromBook(c.ob, 4*time.Minute, now)
		if math.Abs(price-c.wantPrice) > 1e-9 || ok != c.wantOK {
			t.Errorf("%s: got (%.4f, %v), want (%.4f, %v)", c.name, price, ok, c.wantPrice, c.wantOK)
		}
	}
}

// Valuation and exit must never disagree about whether a book is live: a position
// valued off a stale book that could still be exited on it would book a fabricated
// fill. Both graders use the same freshness bound; this pins that they stay in step.
func TestBidFromBook_StalenessMatchesQuoteFromBook(t *testing.T) {
	now := time.Now()
	book := func(age time.Duration) *OrderBook {
		return &OrderBook{
			Bids:   []PriceLevel{{Price: 0.40, Volume: 10}},
			Asks:   []PriceLevel{{Price: 0.42, Volume: 10}},
			AsOfMs: now.Add(-age).UnixMilli(),
		}
	}
	for _, age := range []time.Duration{time.Second, 3 * time.Minute, 5 * time.Minute, time.Hour} {
		_, quality := quoteFromBook(book(age), 4*time.Minute, now)
		_, exitable := bidFromBook(book(age), 4*time.Minute, now)
		if (quality == quoteNone) == exitable {
			t.Errorf("age %v: quoteFromBook says live=%v but bidFromBook says exitable=%v",
				age, quality != quoteNone, exitable)
		}
	}
}
