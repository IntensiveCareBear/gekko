package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPaperAccountBuyAndSettle exercises the capital model + probabilistic
// settlement without Redis/Kafka (redisAddr "" keeps the tracker offline).
func TestPaperAccountBuyAndSettle(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	bc := NewBrokerClient("./nonexistent.sock", "tok")
	// expiry window 0 → positions are immediately eligible to settle.
	pt := NewPositionTracker("", ledger, bc, true, 100000, 0, 0)

	// BUY 100 contracts of a ~$0.43 market.
	pt.ExecuteOrder(&StrategyProposal{ContractID: "FED-RATE-2026", Action: "BUY", AveragePrice: 0.5, TotalSize: 100})

	pt.mu.RLock()
	pos, ok := pt.positions["FED-RATE-2026"]
	cashAfterBuy := pt.cash
	pt.mu.RUnlock()
	if !ok {
		t.Fatal("expected an open position after BUY")
	}
	if cashAfterBuy >= 100000 {
		t.Errorf("expected cash to be debited on BUY, still have $%.2f", cashAfterBuy)
	}
	if pos.Expiry.IsZero() {
		t.Error("expected an expiry to be set on the new position")
	}

	// Settle expired positions.
	pt.settleExpired(context.Background())

	pt.mu.RLock()
	_, stillOpen := pt.positions["FED-RATE-2026"]
	nClosed := len(pt.closedTrades)
	pt.mu.RUnlock()
	if stillOpen {
		t.Error("expected the position to be removed after settlement")
	}
	if nClosed != 1 {
		t.Fatalf("expected exactly 1 closed trade, got %d", nClosed)
	}

	// With no open positions, NAV must equal cash, and realized PnL is reflected.
	acct := pt.snapshotAccount(context.Background())
	if acct.OpenPositions != 0 {
		t.Errorf("expected 0 open positions, got %d", acct.OpenPositions)
	}
	if acct.NAV != acct.Cash {
		t.Errorf("NAV $%.2f should equal cash $%.2f when flat", acct.NAV, acct.Cash)
	}
	// NAV should be starting capital + realized PnL once flat.
	expected := 100000 + acct.RealizedPnL
	if diff := acct.NAV - expected; diff > 0.01 || diff < -0.01 {
		t.Errorf("NAV $%.2f != startingCapital+realizedPnL $%.2f", acct.NAV, expected)
	}
}

// TestBuyingPowerCap ensures orders can't exceed available cash.
func TestBuyingPowerCap(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	bc := NewBrokerClient("./nonexistent.sock", "tok")
	pt := NewPositionTracker("", ledger, bc, true, 50, 0, 0) // tiny $50 account

	// Try to buy 1000 contracts (~$430) — should be scaled down to what $50 affords.
	pt.ExecuteOrder(&StrategyProposal{ContractID: "FED-RATE-2026", Action: "BUY", AveragePrice: 0.5, TotalSize: 1000})

	pt.mu.RLock()
	cash := pt.cash
	pt.mu.RUnlock()
	if cash < 0 {
		t.Errorf("cash went negative ($%.2f) — buying-power cap failed", cash)
	}
}

// TestLiquidateAllPositions verifies every open position is closed at its mark
// (entry price offline — no order books) and realized into the trade history.
func TestLiquidateAllPositions(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	bc := NewBrokerClient("./nonexistent.sock", "tok")
	pt := NewPositionTracker("", ledger, bc, true, 100000, time.Hour, 2*time.Hour)

	pt.ExecuteOrder(&StrategyProposal{ContractID: "FED-RATE-2026", Action: "BUY", AveragePrice: 0.5, TotalSize: 100})
	pt.ExecuteOrder(&StrategyProposal{ContractID: "CPI-JUL-2026", Action: "BUY", AveragePrice: 0.3, TotalSize: 50})

	n := pt.liquidateAllPositions(context.Background())
	if n != 2 {
		t.Fatalf("expected 2 positions liquidated, got %d", n)
	}

	pt.mu.RLock()
	open := len(pt.positions)
	trades := make([]ClosedTrade, len(pt.closedTrades))
	copy(trades, pt.closedTrades)
	pt.mu.RUnlock()
	if open != 0 {
		t.Errorf("expected 0 open positions after liquidation, got %d", open)
	}
	if len(trades) != 2 {
		t.Fatalf("expected 2 closed trades, got %d", len(trades))
	}
	for _, tr := range trades {
		if tr.Status != "LIQUIDATED" {
			t.Errorf("trade %s status = %q, want LIQUIDATED", tr.ContractID, tr.Status)
		}
	}
	// Offline (no live or cached quote) a realization takes the honest-mark haircut:
	// entry × 0.5, never a flat "at entry" fill. Cost basis was $50 + $15 = $65, so
	// liquidation returns $32.50 → cash $99,967.50.
	acct := pt.snapshotAccount(context.Background())
	if diff := acct.Cash - 99967.50; diff > 0.01 || diff < -0.01 {
		t.Errorf("cash $%.2f after no-quote liquidation, want $99967.50 (entry × 0.5 haircut)", acct.Cash)
	}

	// Idempotent on an empty book.
	if n := pt.liquidateAllPositions(context.Background()); n != 0 {
		t.Errorf("second liquidation closed %d positions, want 0", n)
	}
}

// TestResetRunLiquidatesOpenPositions verifies a reset realizes open positions
// into the trade log (visible to the exporter) before wiping the run.
func TestResetRunLiquidatesOpenPositions(t *testing.T) {
	dir := t.TempDir()
	ledger := NewImmutableLedger(filepath.Join(dir, "ledger.db"))
	bc := NewBrokerClient("./nope.sock", "tok")
	pt := NewPositionTracker("", ledger, bc, true, 100000, time.Hour, 2*time.Hour)
	pt.exporter = NewRunExporter(dir)

	pt.ExecuteOrder(&StrategyProposal{ContractID: "FED-RATE-2026", Action: "BUY", AveragePrice: 0.5, TotalSize: 100})
	pt.resetRun(context.Background(), "liquidation test")

	pt.mu.RLock()
	open := len(pt.positions)
	nClosed := len(pt.closedTrades)
	cash := pt.cash
	pt.mu.RUnlock()
	if open != 0 {
		t.Errorf("expected 0 open positions after reset, got %d", open)
	}
	if nClosed != 0 {
		t.Errorf("expected trade history cleared after reset, got %d", nClosed)
	}
	if cash != 100000 {
		t.Errorf("cash $%.2f after reset, want $100000", cash)
	}

	// The liquidation trade must be preserved in the archived run, with the note.
	archives, err := filepath.Glob(filepath.Join(dir, "archive-*"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("expected 1 archive dir, got %v (err=%v)", archives, err)
	}
	tradesCSV, err := os.ReadFile(filepath.Join(archives[0], "trades.csv"))
	if err != nil || !strings.Contains(string(tradesCSV), "LIQUIDATED") {
		t.Errorf("archived trades.csv missing LIQUIDATED close (err=%v):\n%s", err, tradesCSV)
	}
	note, err := os.ReadFile(filepath.Join(archives[0], "run_note.txt"))
	if err != nil || !strings.Contains(string(note), "liquidation test") {
		t.Errorf("archived run_note.txt missing note (err=%v): %q", err, note)
	}
}

func TestRunExporterWritesFiles(t *testing.T) {
	dir := t.TempDir()
	ledger := NewImmutableLedger(filepath.Join(dir, "ledger.db"))
	bc := NewBrokerClient("./nope.sock", "tok")
	pt := NewPositionTracker("", ledger, bc, true, 100000, 0, 0)
	pt.exporter = NewRunExporter(dir)

	pt.ExecuteOrder(&StrategyProposal{ContractID: "FED-RATE-2026", Action: "BUY", AveragePrice: 0.5, TotalSize: 100})
	pt.settleExpired(context.Background())
	pt.recordEquityPoint(context.Background())

	for _, f := range []string{"trades.csv", "trades.jsonl", "equity.csv", "equity.jsonl", "run_summary.txt"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Size() == 0 {
			t.Errorf("expected non-empty export file %s (err=%v)", f, err)
		}
	}
}

// TestTrackSettlementCreditsCash — regression: TrackSettlement used to delete the
// position without crediting the settlement proceeds or realized PnL (silent NAV
// loss on every news-driven settlement), and synced Redis while holding pt.mu.
func TestTrackSettlementCreditsCash(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	bc := NewBrokerClient("./nonexistent.sock", "tok")
	pt := NewPositionTracker("", ledger, bc, true, 100000, time.Hour, 2*time.Hour)

	pt.mu.Lock()
	pt.positions["AAPL-UP-200"] = &Position{ContractID: "AAPL-UP-200", EntryPrice: 0.40, Size: 100, Timestamp: time.Now()}
	pt.cash = 100000 - 0.40*100 // as if the BUY debited $40
	pt.mu.Unlock()

	pt.TrackSettlement("Closed yes AAPL-UP-200 market status update")

	pt.mu.RLock()
	defer pt.mu.RUnlock()
	if _, open := pt.positions["AAPL-UP-200"]; open {
		t.Error("settled position should be removed")
	}
	// YES settlement pays $1.00 × 100 = $100 back: 99960 + 100 = 100060.
	if pt.cash != 100060 {
		t.Errorf("cash = %.2f, want 100060 (settlement proceeds credited)", pt.cash)
	}
	// Realized = (1.00 − 0.40) × 100 = $60.
	if pt.realizedPnL != 60 {
		t.Errorf("realizedPnL = %.2f, want 60", pt.realizedPnL)
	}
	if len(pt.closedTrades) != 1 || pt.closedTrades[0].Status != "SETTLED" {
		t.Errorf("expected one SETTLED closed trade, got %+v", pt.closedTrades)
	}
}

// TestTrackSettlementNoSettlesToZero verifies a NO close credits nothing but still
// books the realized loss and removes the position.
func TestTrackSettlementNoSettlesToZero(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	bc := NewBrokerClient("./nonexistent.sock", "tok")
	pt := NewPositionTracker("", ledger, bc, true, 100000, time.Hour, 2*time.Hour)

	pt.mu.Lock()
	pt.positions["BTC-100K-DEC"] = &Position{ContractID: "BTC-100K-DEC", EntryPrice: 0.40, Size: 100, Timestamp: time.Now()}
	pt.cash = 100000 - 0.40*100
	pt.mu.Unlock()

	pt.TrackSettlement("Closed no BTC-100K-DEC market status update")

	pt.mu.RLock()
	defer pt.mu.RUnlock()
	if pt.cash != 99960 {
		t.Errorf("cash = %.2f, want 99960 (NO pays $0)", pt.cash)
	}
	if pt.realizedPnL != -40 {
		t.Errorf("realizedPnL = %.2f, want -40", pt.realizedPnL)
	}
}

// TestSettleExpired_MarkPriorSupersedesEntry proves the sim-settlement prior uses
// the freshest mark (default SETTLE_PRIOR=mark), not the stale entry price. A
// position bought cheap (0.10) that has since moved to near-certainty (mark 1.0)
// must settle YES from the mark. Mark 1.0 makes the coin flip deterministic
// (rand.Float64() ∈ [0,1) is always < 1.0); under the old entry-price prior (0.10)
// a YES would be near-impossible, so a YES here can only come from the mark.
func TestSettleExpired_MarkPriorSupersedesEntry(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	pt := NewPositionTracker("", ledger, NewBrokerClient("./none.sock", "t"), true, 100000, 0, 0)
	pt.allowMockBook = false // force the mark to come from markCache, not a mock book
	pt.settlePrior = "mark"

	id := "KXBTC-26DEC31-B100000"
	pt.mu.Lock()
	pt.positions[id] = &Position{ContractID: id, EntryPrice: 0.10, Size: 100, Timestamp: time.Now(), Expiry: time.Now().Add(-time.Minute)}
	pt.markCache[id] = markEntry{Price: 1.0, At: time.Now()}
	pt.mu.Unlock()

	pt.settleExpired(context.Background())

	pt.mu.RLock()
	defer pt.mu.RUnlock()
	if _, open := pt.positions[id]; open {
		t.Fatal("expired position should have settled")
	}
	last := pt.closedTrades[len(pt.closedTrades)-1]
	if last.ExitPrice != 1.0 {
		t.Fatalf("mark prior (1.0) should settle YES; got exit %.4f (entry prior would near-always give 0)", last.ExitPrice)
	}
	if last.Status != "RESOLVED" {
		t.Errorf("expected RESOLVED status, got %q", last.Status)
	}
}

// TestSettleExpired_EntryPriorIgnoresMark proves SETTLE_PRIOR=entry restores the
// old behavior: the mark (0.02) is ignored and the entry price is the YES
// probability. Entry 1.0 makes it deterministic; under mark mode the 0.02 mark
// would make a YES near-impossible, so a YES here can only come from the entry.
func TestSettleExpired_EntryPriorIgnoresMark(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	pt := NewPositionTracker("", ledger, NewBrokerClient("./none.sock", "t"), true, 100000, 0, 0)
	pt.allowMockBook = false
	pt.settlePrior = "entry"

	id := "KXETH-26DEC31-B5000"
	pt.mu.Lock()
	pt.positions[id] = &Position{ContractID: id, EntryPrice: 1.0, Size: 100, Timestamp: time.Now(), Expiry: time.Now().Add(-time.Minute)}
	pt.markCache[id] = markEntry{Price: 0.02, At: time.Now()}
	pt.mu.Unlock()

	pt.settleExpired(context.Background())

	pt.mu.RLock()
	defer pt.mu.RUnlock()
	last := pt.closedTrades[len(pt.closedTrades)-1]
	if last.ExitPrice != 1.0 {
		t.Fatalf("entry prior (1.0) should settle YES regardless of the 0.02 mark; got %.4f", last.ExitPrice)
	}
}

// A weather position carrying SettleResolution="YES" must settle YES deterministically
// (the strategy already knows the day's high cleared the bucket), NOT via the sim coin
// flip — even with a tiny entry price and a low mark that would near-always give NO.
func TestSettleExpired_DeterministicResolutionWins(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	pt := NewPositionTracker("", ledger, NewBrokerClient("./none.sock", "t"), true, 100000, 0, 0)
	pt.allowMockBook = false
	pt.settlePrior = "mark"

	id := "KXHIGHNY-26JUL11-T88"
	pt.mu.Lock()
	pt.positions[id] = &Position{ContractID: id, EntryPrice: 0.90, Size: 100, Timestamp: time.Now(),
		Expiry: time.Now().Add(-time.Minute), Persona: "weather", SettleResolution: "YES"}
	pt.markCache[id] = markEntry{Price: 0.02, At: time.Now()} // would near-always settle NO under the sim
	pt.mu.Unlock()

	pt.settleExpired(context.Background())

	pt.mu.RLock()
	defer pt.mu.RUnlock()
	if _, open := pt.positions[id]; open {
		t.Fatal("expired weather position should have settled")
	}
	last := pt.closedTrades[len(pt.closedTrades)-1]
	if last.ExitPrice != 1.0 {
		t.Fatalf("deterministic YES should settle at 1.0 regardless of the 0.02 mark; got %.4f", last.ExitPrice)
	}
	// A cleared 0.90 bucket wins ~$10 (1.0−0.90 × 100), the honest edge the coin flip erased.
	if last.RealizedPnL <= 0 {
		t.Errorf("expected positive realized P/L on a cleared bucket, got %.2f", last.RealizedPnL)
	}
}

// A SettleResolution="NO" hint settles NO deterministically (0.0).
func TestSettleExpired_DeterministicResolutionNo(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	pt := NewPositionTracker("", ledger, NewBrokerClient("./none.sock", "t"), true, 100000, 0, 0)
	pt.allowMockBook = false
	pt.settlePrior = "entry"

	id := "KXHIGHNY-26JUL11-T88"
	pt.mu.Lock()
	pt.positions[id] = &Position{ContractID: id, EntryPrice: 0.99, Size: 100, Timestamp: time.Now(),
		Expiry: time.Now().Add(-time.Minute), SettleResolution: "NO"} // entry 0.99 would near-always give YES under the sim
	pt.mu.Unlock()

	pt.settleExpired(context.Background())

	pt.mu.RLock()
	defer pt.mu.RUnlock()
	last := pt.closedTrades[len(pt.closedTrades)-1]
	if last.ExitPrice != 0.0 {
		t.Fatalf("deterministic NO should settle at 0.0 regardless of the 0.99 entry; got %.4f", last.ExitPrice)
	}
}
