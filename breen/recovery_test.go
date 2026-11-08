package main

import (
	"testing"
	"time"
)

func blk(contract, action string, size int, price float64, status string, yield float64) TransactionBlock {
	return TransactionBlock{
		ContractID: contract, Action: action, Size: size, Price: price,
		Status: status, YieldDelta: yield, Timestamp: time.Now(),
	}
}

func TestReplayLedger_OpenFillAndClose(t *testing.T) {
	blocks := []TransactionBlock{
		blk("KXCPI", "BUY", 1000, 0.50, "DRY_OPEN", 0),       // receipt — ignored
		blk("KXCPI", "BUY", 1000, 0.50, "DRY_FILLED", 0),     // open: -$500, pos 1000@0.50
		blk("KXCPI", "BUY", 1000, 0.60, "DRY_FILLED", 0),     // add: -$600, pos 2000@0.55
		blk("KXCPI", "CLOSE", 2000, 1.00, "DRY_CLOSED", 900), // settle YES: +$2000, realized +$900
	}
	st := replayLedger(blocks, 100000)

	if len(st.positions) != 0 {
		t.Errorf("expected flat after close, got %d positions", len(st.positions))
	}
	// cash = 100000 - 500 - 600 + 2000 = 100900
	if st.cash != 100900 {
		t.Errorf("cash = %.2f, want 100900", st.cash)
	}
	if st.realizedPnL != 900 {
		t.Errorf("realized = %.2f, want 900", st.realizedPnL)
	}
	if len(st.closedTrades) != 1 || st.closedTrades[0].EntryPrice != 0.55 {
		t.Errorf("closed trade wrong: %+v", st.closedTrades)
	}
	if st.applied != 3 {
		t.Errorf("applied = %d, want 3 (2 fills + 1 close)", st.applied)
	}
}

func TestReplayLedger_OpenPositionRemains(t *testing.T) {
	blocks := []TransactionBlock{
		blk("KXFED", "BUY", 500, 0.40, "DRY_FILLED", 0),
		blk("POLY-123", "BUY", 200, 0.30, "DRY_FILLED", 0),
		blk("POLY-123", "CLOSE", 100, 0.50, "DRY_CLOSED", 20), // partial close
	}
	st := replayLedger(blocks, 100000)

	if _, ok := st.positions["KXFED"]; !ok {
		t.Error("KXFED should remain open")
	}
	poly, ok := st.positions["POLY-123"]
	if !ok || poly.Size != 100 {
		t.Errorf("POLY-123 should have 100 left, got %+v", poly)
	}
	// cash = 100000 - 200 - 60 + 50 = 99790
	if st.cash != 99790 {
		t.Errorf("cash = %.2f, want 99790", st.cash)
	}
}

func TestReplayLedger_IgnoresReceiptsAndDenials(t *testing.T) {
	blocks := []TransactionBlock{
		blk("X", "BUY", 100, 0.5, "DRY_OPEN", 0),
		blk("X", "BUY", 100, 0.5, "DENIED", 0),
		blk("Y", "BUY", 100, 0.5, "REJECTED", 0),
	}
	st := replayLedger(blocks, 100000)
	if st.applied != 0 || st.cash != 100000 || len(st.positions) != 0 {
		t.Errorf("receipts/denials must not affect state: applied=%d cash=%.2f pos=%d", st.applied, st.cash, len(st.positions))
	}
}

func TestReplayLedger_ResolvedSettlementCreditsCash(t *testing.T) {
	// Regression: expiry settlements write DRY_RESOLVED, which replay used to skip —
	// fills debited cash but settlement credits never landed, so a long-running
	// ledger replayed to deeply negative cash with phantom "open" positions.
	blocks := []TransactionBlock{
		blk("KXCPI", "BUY", 1000, 0.50, "DRY_FILLED", 0),       // -$500
		blk("KXCPI", "CLOSE", 1000, 1.00, "DRY_RESOLVED", 500), // settle YES: +$1000
		blk("KXFED", "BUY", 500, 0.40, "DRY_FILLED", 0),        // -$200
		blk("KXFED", "CLOSE", 500, 0.00, "DRY_RESOLVED", -200), // settle NO: +$0
	}
	st := replayLedger(blocks, 100000)

	if len(st.positions) != 0 {
		t.Errorf("settled positions must not remain open, got %d", len(st.positions))
	}
	// cash = 100000 - 500 + 1000 - 200 + 0 = 100300
	if st.cash != 100300 {
		t.Errorf("cash = %.2f, want 100300", st.cash)
	}
	if st.realizedPnL != 300 {
		t.Errorf("realized = %.2f, want 300", st.realizedPnL)
	}
	if len(st.closedTrades) != 2 {
		t.Errorf("want 2 closed trades, got %d", len(st.closedTrades))
	}
}

func TestReplayLedger_StartsAfterLastRunReset(t *testing.T) {
	// Blocks before the RUN_RESET marker belong to a previous run and must not
	// leak into the rebuilt state (replay-across-resets was the −$283k incident).
	blocks := []TransactionBlock{
		blk("OLD-1", "BUY", 9000, 0.90, "DRY_FILLED", 0), // previous run: -$8100
		blk("OLD-2", "BUY", 9000, 0.90, "DRY_FILLED", 0),
		blk("RUN_RESET", "RESET", 0, 0, "RUN_RESET", 0),
		blk("KXCPI", "BUY", 100, 0.50, "DRY_FILLED", 0), // current run: -$50
	}
	st := replayLedger(blocks, 100000)

	if len(st.positions) != 1 {
		t.Fatalf("want only the post-reset position, got %d", len(st.positions))
	}
	if _, ok := st.positions["KXCPI"]; !ok {
		t.Error("KXCPI (post-reset) should be open")
	}
	if st.cash != 99950 {
		t.Errorf("cash = %.2f, want 99950 (pre-reset fills ignored)", st.cash)
	}
	if st.applied != 1 {
		t.Errorf("applied = %d, want 1", st.applied)
	}
}

func TestReplayLedger_OnlyResetMarkerIsEmpty(t *testing.T) {
	blocks := []TransactionBlock{
		blk("OLD-1", "BUY", 100, 0.50, "DRY_FILLED", 0),
		blk("RUN_RESET", "RESET", 0, 0, "RUN_RESET", 0),
	}
	st := replayLedger(blocks, 100000)
	if st.applied != 0 || st.cash != 100000 || len(st.positions) != 0 {
		t.Errorf("post-reset ledger with no trades must replay empty: applied=%d cash=%.2f pos=%d", st.applied, st.cash, len(st.positions))
	}
}

func TestReplayLedger_CloseWithoutPositionIgnored(t *testing.T) {
	blocks := []TransactionBlock{
		blk("Z", "CLOSE", 100, 1.0, "DRY_CLOSED", 50), // no prior open
	}
	st := replayLedger(blocks, 100000)
	if st.cash != 100000 || st.realizedPnL != 0 || len(st.closedTrades) != 0 {
		t.Errorf("orphan close must be ignored: cash=%.2f realized=%.2f", st.cash, st.realizedPnL)
	}
}
