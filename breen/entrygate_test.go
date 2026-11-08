package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tightBook(bid, ask float64, asOf time.Time) *OrderBook {
	ob := &OrderBook{
		ContractID: "TEST-BOOK-1",
		Bids:       []PriceLevel{{Price: bid, Volume: 500}},
		Asks:       []PriceLevel{{Price: ask, Volume: 500}},
	}
	if !asOf.IsZero() {
		ob.AsOfMs = asOf.UnixMilli()
	}
	return ob
}

func TestEntryBookBlock_MissingBook(t *testing.T) {
	now := time.Now()
	if reason := entryBookBlock(nil, true, 0.10, 0.02, 90*time.Second, now); reason == "" {
		t.Error("expected a missing book to be blocked when requireLive is set")
	}
	if reason := entryBookBlock(nil, false, 0.10, 0.02, 90*time.Second, now); reason != "" {
		t.Errorf("expected a missing book to pass with requireLive off, got %q", reason)
	}
	// One-sided book counts as missing.
	oneSided := &OrderBook{Asks: []PriceLevel{{Price: 0.5, Volume: 100}}}
	if reason := entryBookBlock(oneSided, true, 0.10, 0.02, 90*time.Second, now); reason == "" {
		t.Error("expected a one-sided book to be blocked when requireLive is set")
	}
}

func TestEntryBookBlock_Spread(t *testing.T) {
	now := time.Now()
	// Tight book at mid 0.50: spread 0.02 ≤ 10% of mid → pass.
	if reason := entryBookBlock(tightBook(0.49, 0.51, now), true, 0.10, 0.02, 0, now); reason != "" {
		t.Errorf("expected tight book to pass, got %q", reason)
	}
	// Wide book: bid 0.40 / ask 0.60 → spread 0.20 = 40% of mid, over both caps → block.
	if reason := entryBookBlock(tightBook(0.40, 0.60, now), true, 0.10, 0.02, 0, now); !strings.Contains(reason, "spread") {
		t.Errorf("expected wide spread to be blocked, got %q", reason)
	}
	// Cheap contract with a penny-wide book: bid 0.09 / ask 0.11 is 20% of mid but
	// within the $0.02 absolute tolerance → pass.
	if reason := entryBookBlock(tightBook(0.09, 0.11, now), true, 0.10, 0.02, 0, now); reason != "" {
		t.Errorf("expected penny-wide cheap book to pass via abs tolerance, got %q", reason)
	}
	// Same book with the abs tolerance off → blocked on relative spread.
	if reason := entryBookBlock(tightBook(0.09, 0.11, now), true, 0.10, 0, 0, now); reason == "" {
		t.Error("expected cheap wide-relative book to be blocked with abs tolerance off")
	}
	// Spread limb disabled entirely → pass.
	if reason := entryBookBlock(tightBook(0.40, 0.60, now), true, 0, 0, 0, now); reason != "" {
		t.Errorf("expected wide book to pass with spread cap off, got %q", reason)
	}
}

func TestEntryBookBlock_Staleness(t *testing.T) {
	now := time.Now()
	// Stamped 5 minutes ago against a 90s freshness cap → block.
	stale := tightBook(0.49, 0.51, now.Add(-5*time.Minute))
	if reason := entryBookBlock(stale, true, 0.10, 0.02, 90*time.Second, now); !strings.Contains(reason, "stale") {
		t.Errorf("expected stale book to be blocked, got %q", reason)
	}
	// Unstamped book (as_of_ms 0, e.g. mock/legacy) skips the age check.
	if reason := entryBookBlock(tightBook(0.49, 0.51, time.Time{}), true, 0.10, 0.02, 90*time.Second, now); reason != "" {
		t.Errorf("expected unstamped book to pass the age check, got %q", reason)
	}
	// Age limb disabled → stale stamp passes.
	if reason := entryBookBlock(stale, true, 0.10, 0.02, 0, now); reason != "" {
		t.Errorf("expected stale book to pass with age cap off, got %q", reason)
	}
}

// A dry BUY with the gate on and no live book must be rejected outright — no
// position, no cash movement, no proposal-price fallback fill.
func TestExecuteOrder_BuyRejectedWithoutLiveBook(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	bc := NewBrokerClient("./nonexistent.sock", "tok")
	pt := NewPositionTracker("", ledger, bc, true, 100000, time.Hour, 2*time.Hour)
	pt.entryRequireLiveBook = true // production default (main.go wires from config)

	pt.ExecuteOrder(&StrategyProposal{ContractID: "FED-RATE-2026", Action: "BUY", AveragePrice: 0.5, TotalSize: 100})

	pt.mu.RLock()
	_, held := pt.positions["FED-RATE-2026"]
	cash := pt.cash
	pt.mu.RUnlock()
	if held {
		t.Error("expected no position to open without a live book")
	}
	if cash != 100000 {
		t.Errorf("expected cash untouched on rejected entry, got $%.2f", cash)
	}
	if got := pt.proposalsRejected.Load(); got != 1 {
		t.Errorf("expected proposalsRejected=1, got %d", got)
	}

	// Exits must keep working without a book (they only reduce exposure): seed a
	// position directly, then SELL with no live book — the fallback fill applies.
	pt.mu.Lock()
	pt.positions["CPI-JUL-2026"] = &Position{ContractID: "CPI-JUL-2026", EntryPrice: 0.4, Size: 50, Timestamp: time.Now()}
	pt.mu.Unlock()
	pt.ExecuteOrder(&StrategyProposal{ContractID: "CPI-JUL-2026", Action: "SELL", AveragePrice: 0.45, TotalSize: 50})
	pt.mu.RLock()
	_, stillHeld := pt.positions["CPI-JUL-2026"]
	pt.mu.RUnlock()
	if stillHeld {
		t.Error("expected SELL to close the position despite no live book")
	}
}
