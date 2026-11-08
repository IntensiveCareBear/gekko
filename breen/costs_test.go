package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestKalshiFee_PublishedFormula(t *testing.T) {
	cases := []struct {
		name       string
		contractID string
		price      float64
		size       int
		enabled    bool
		want       float64
	}{
		// ceil(0.07 × 100 × 0.5 × 0.5) = ceil($1.75) → $1.75 (already whole cents).
		{"coin flip is the fee peak", "KXINXY-26DEC31", 0.50, 100, true, 1.75},
		// 0.07 × 100 × 0.1 × 0.9 = $0.63 — a tenth of the mid-band fee at the same size.
		{"longshot is cheap", "KXINXY-26DEC31", 0.10, 100, true, 0.63},
		{"symmetric about 0.5", "KXINXY-26DEC31", 0.90, 100, true, 0.63},
		// 0.07 × 1 × 0.5 × 0.5 = $0.0175 → rounds UP to a cent, never down to zero.
		{"single contract rounds up to a cent", "KXINXY-26DEC31", 0.50, 1, true, 0.02},
		{"disabled model charges nothing", "KXINXY-26DEC31", 0.50, 100, false, 0},
		// Polymarket's schedule is different and the run is Kalshi-only.
		{"non-kalshi venue", "POLY-123456789", 0.50, 100, true, 0},
		// Settlement pays a binary, and Kalshi charges no fee on it.
		{"settled to zero", "KXINXY-26DEC31", 0, 100, true, 0},
		{"settled to one", "KXINXY-26DEC31", 1, 100, true, 0},
		{"zero size", "KXINXY-26DEC31", 0.50, 0, true, 0},
	}
	for _, c := range cases {
		got := kalshiFee(c.contractID, c.price, c.size, defaultKalshiFeeRate, c.enabled)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: kalshiFee = %.4f, want %.4f", c.name, got, c.want)
		}
	}
}

func TestHalfSpreadPerContract(t *testing.T) {
	cases := []struct {
		name string
		ob   *OrderBook
		want float64
	}{
		{"penny-wide book", &OrderBook{
			Bids: []PriceLevel{{Price: 0.44}}, Asks: []PriceLevel{{Price: 0.46}}}, 0.01},
		// No book means the fill fell back to the proposal price — nothing was crossed.
		{"nil book", nil, 0},
		{"one-sided book", &OrderBook{Bids: []PriceLevel{{Price: 0.44}}}, 0},
		{"crossed quote", &OrderBook{
			Bids: []PriceLevel{{Price: 0.50}}, Asks: []PriceLevel{{Price: 0.48}}}, 0},
	}
	for _, c := range cases {
		if got := halfSpreadPerContract(c.ob); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: halfSpreadPerContract = %.4f, want %.4f", c.name, got, c.want)
		}
	}
}

// A position closed in slices must not be billed for the same fill's costs twice.
func TestCloseCosts_ApportionsAcrossPartialCloses(t *testing.T) {
	pos := &Position{ContractID: "KXINXY-26DEC31", EntryPrice: 0.50, Size: 100,
		EntrySpreadCostUSD: 1.00, EntryFeeUSD: 1.75}

	// First slice: 40 of 100 contracts → 40% of the entry costs.
	first := closeCosts(pos, 0.55, 40, 0, defaultKalshiFeeRate, true)
	if math.Abs(first.SpreadUSD-0.40) > 1e-9 {
		t.Errorf("first slice spread = %.4f, want 0.40", first.SpreadUSD)
	}
	consumeEntryCosts(pos, 40)
	pos.Size -= 40

	// Second slice takes the remaining 60 contracts and the remaining 60% of costs.
	second := closeCosts(pos, 0.55, 60, 0, defaultKalshiFeeRate, true)
	if math.Abs(second.SpreadUSD-0.60) > 1e-9 {
		t.Errorf("second slice spread = %.4f, want 0.60", second.SpreadUSD)
	}
	if total := first.SpreadUSD + second.SpreadUSD; math.Abs(total-1.00) > 1e-9 {
		t.Errorf("apportioned spread totals %.4f, want the $1.00 actually paid", total)
	}
	consumeEntryCosts(pos, 60)
	if pos.EntrySpreadCostUSD > 1e-9 || pos.EntryFeeUSD > 1e-9 {
		t.Errorf("fully closed position still carries entry costs: spread=%.4f fee=%.4f",
			pos.EntrySpreadCostUSD, pos.EntryFeeUSD)
	}
}

func TestCloseCosts_ExitSpreadOnlyWhenBookFilled(t *testing.T) {
	pos := &Position{ContractID: "KXINXY-26DEC31", EntryPrice: 0.50, Size: 100, EntrySpreadCostUSD: 1.00}

	// Stop-loss/force-exit closes mark at the mid: no exit spread is crossed.
	atMid := closeCosts(pos, 0.45, 100, 0, defaultKalshiFeeRate, true)
	if math.Abs(atMid.SpreadUSD-1.00) > 1e-9 {
		t.Errorf("mid-marked exit spread = %.4f, want the entry side only (1.00)", atMid.SpreadUSD)
	}
	// A signal-driven exit fills at the bid and pays the half-spread again.
	atBid := closeCosts(pos, 0.45, 100, 0.01, defaultKalshiFeeRate, true)
	if math.Abs(atBid.SpreadUSD-2.00) > 1e-9 {
		t.Errorf("book-filled exit spread = %.4f, want 2.00 (both legs)", atBid.SpreadUSD)
	}
}

// Recording the fee and charging it are separate knobs: the default run records the
// drag without moving cash, so its P&L stays comparable with every earlier run.
func TestFeeCharge_RecordedAlwaysChargedOnlyWhenEnabled(t *testing.T) {
	for _, charge := range []bool{false, true} {
		tr := newGuardTracker(t, RiskCaps{})
		tr.feeModel, tr.feeCharge = true, charge

		id := "KXINXY-26DEC31H1600-B5900"
		pos := &Position{ContractID: id, EntryPrice: 0.50, Size: 100, Timestamp: time.Now()}
		tr.positions[id] = pos
		cashBefore, realizedBefore := tr.cash, tr.realizedPnL

		tr.mu.Lock()
		tr.recordClosedTradeLocked(pos, 0.50, pos.Size, "EXIT", "DRY_CLOSED", 0)
		tr.mu.Unlock()

		got := tr.closedTrades[len(tr.closedTrades)-1]
		if got.FeeUSD != 1.75 {
			t.Errorf("charge=%v: FeeUSD = %.2f, want the fee recorded regardless (1.75)", charge, got.FeeUSD)
		}
		cashMoved := math.Abs(tr.cash - cashBefore)
		realizedMoved := math.Abs(tr.realizedPnL - realizedBefore)
		if charge {
			if math.Abs(cashMoved-1.75) > 1e-9 || math.Abs(realizedMoved-1.75) > 1e-9 {
				t.Errorf("charge=true: cash moved %.2f and realized %.2f, want $1.75 debited from both",
					cashMoved, realizedMoved)
			}
		} else if cashMoved > 1e-9 || realizedMoved > 1e-9 {
			t.Errorf("charge=false: fee must be recorded only, but cash moved %.2f / realized %.2f",
				cashMoved, realizedMoved)
		}
	}
}

// Full cycle through the real fill path: open off a live book, mark it around, close
// it back into the book, and check the closed trade carries what actually happened.
// The unit tests above prove the formulas; this proves they are wired to the fills.
func TestExecuteOrder_RoundTripRecordsCostsAndExcursions(t *testing.T) {
	ledger := NewImmutableLedger(filepath.Join(t.TempDir(), "ledger.db"))
	pt := NewPositionTracker("", ledger, NewBrokerClient("./nonexistent.sock", "tok"), true, 100000, time.Hour, 2*time.Hour)
	pt.allowMockBook = true // keyless dev book: deterministic prices, random volumes

	// generateMockBook derives its base from the ticker length: 13 chars → base 0.43,
	// so the top of book is bid 0.42 / ask 0.43 and the half-spread is $0.005.
	id := "KXFED-26DEC31"
	pt.ExecuteOrder(&StrategyProposal{ContractID: id, Action: "BUY", AveragePrice: 0.43, TotalSize: 100,
		Persona: "gemma-bull", Confidence: 0.85})

	pt.mu.RLock()
	pos, held := pt.positions[id]
	pt.mu.RUnlock()
	if !held {
		t.Fatal("expected the BUY to open a position")
	}
	if pos.EntryPrice != 0.43 {
		t.Fatalf("filled at %.4f, want the ask 0.43", pos.EntryPrice)
	}
	// 0.005 × 100 contracts crossed on the way in.
	if math.Abs(pos.EntrySpreadCostUSD-0.50) > 1e-9 {
		t.Errorf("EntrySpreadCostUSD = %.4f, want 0.50", pos.EntrySpreadCostUSD)
	}
	// ceil(0.07 × 100 × 0.43 × 0.57) = ceil($1.7157) → $1.72.
	if math.Abs(pos.EntryFeeUSD-1.72) > 1e-9 {
		t.Errorf("EntryFeeUSD = %.4f, want 1.72", pos.EntryFeeUSD)
	}
	if pos.BestMark != 0.43 || pos.WorstMark != 0.43 {
		t.Errorf("a new position seeds both extremes at its fill, got best=%.4f worst=%.4f",
			pos.BestMark, pos.WorstMark)
	}

	// Two settlement ticks: up to 0.50, then down to 0.38.
	pt.updateExcursions(map[string]float64{id: 0.50})
	pt.updateExcursions(map[string]float64{id: 0.38})

	// Exit back into the book — this one fills at the bid, so it crosses the spread again.
	pt.ExecuteOrder(&StrategyProposal{ContractID: id, Action: "SELL", AveragePrice: 0.42, TotalSize: 100})

	pt.mu.RLock()
	_, stillHeld := pt.positions[id]
	trades := append([]ClosedTrade(nil), pt.closedTrades...)
	pt.mu.RUnlock()
	if stillHeld {
		t.Fatal("expected the SELL to close the position")
	}
	if len(trades) != 1 {
		t.Fatalf("want 1 closed trade, got %d", len(trades))
	}
	tr := trades[0]

	// Both legs of the spread: $0.50 in + $0.50 out.
	if math.Abs(tr.SpreadCostUSD-1.00) > 1e-9 {
		t.Errorf("SpreadCostUSD = %.4f, want 1.00 (both legs crossed)", tr.SpreadCostUSD)
	}
	// $1.72 entry + ceil(0.07 × 100 × 0.42 × 0.58) = $1.71 exit.
	if math.Abs(tr.FeeUSD-3.43) > 1e-9 {
		t.Errorf("FeeUSD = %.4f, want 3.43", tr.FeeUSD)
	}
	// 0.43 → 0.50 is +16.28%; 0.43 → 0.38 is −11.63%. The 0.42 exit beats neither.
	if tr.MaxFavorableExcursionPct != 16.28 {
		t.Errorf("MFE = %.2f%%, want 16.28%%", tr.MaxFavorableExcursionPct)
	}
	if tr.MaxAdverseExcursionPct != -11.63 {
		t.Errorf("MAE = %.2f%%, want -11.63%%", tr.MaxAdverseExcursionPct)
	}
	// The persona's stated conviction has to reach the trade record, or the
	// calibration table has nothing to bin.
	if len(tr.PersonaScores) != 1 || tr.PersonaScores[0].Confidence != 0.85 {
		t.Errorf("want the 0.85 conviction carried to the close, got %+v", tr.PersonaScores)
	}
}

// Spread cost decomposes realized P&L; it is not a second debit. If this ever fails,
// someone has started subtracting the spread that the ask-fill already charged.
func TestBookMetrics_SpreadIsDiagnosticFeesAreTheAdjustment(t *testing.T) {
	trades := []ClosedTrade{
		{ContractID: "A", EntryPrice: 0.50, Size: 100, RealizedPnL: -20, SpreadCostUSD: 1.00, FeeUSD: 1.75,
			MaxFavorableExcursionPct: 10, MaxAdverseExcursionPct: -40, ClosedAtMs: 1},
		{ContractID: "B", EntryPrice: 0.30, Size: 100, RealizedPnL: 50, SpreadCostUSD: 0.50, FeeUSD: 1.47,
			MaxFavorableExcursionPct: 60, MaxAdverseExcursionPct: -10, ClosedAtMs: 2},
		// Legacy trade with no excursion data must not drag the averages toward zero.
		{ContractID: "C", EntryPrice: 0.40, Size: 100, RealizedPnL: 10, ClosedAtMs: 3},
	}
	bm := computeBookMetrics(trades, 100000, 20)

	if bm.RealizedPnL != 40 {
		t.Errorf("RealizedPnL = %.2f, want 40", bm.RealizedPnL)
	}
	if bm.SpreadCostUSD != 1.50 {
		t.Errorf("SpreadCostUSD = %.2f, want 1.50", bm.SpreadCostUSD)
	}
	if bm.NetPnLAfterFees != 40-3.22 {
		t.Errorf("NetPnLAfterFees = %.2f, want %.2f (realized minus fees only)", bm.NetPnLAfterFees, 40-3.22)
	}
	if bm.ExcursionSamples != 2 {
		t.Errorf("ExcursionSamples = %d, want 2 (the legacy trade carries none)", bm.ExcursionSamples)
	}
	if bm.AvgMFEPct != 35 || bm.AvgMAEPct != -25 {
		t.Errorf("excursion averages = (%.2f, %.2f), want (35, -25)", bm.AvgMFEPct, bm.AvgMAEPct)
	}
}

// Truth scoreboard (2026-08-25 desk review fix #1): when the fee is charged it must be
// charged everywhere the system LEARNS from, not just to cash. The allocator reads
// PersonaScore.ReturnPct, Lou and the scorecard read RealizedPnL, and the risk guards
// read the family accrual — leaving any of them gross keeps the learners tuning a
// flattered number.
func TestFeeCharge_NetsIntoEverythingLearnersRead(t *testing.T) {
	for _, charge := range []bool{false, true} {
		tr := newGuardTracker(t, RiskCaps{})
		tr.feeModel, tr.feeCharge = true, charge

		id := "KXINXY-26DEC31H1600-B5900"
		pos := &Position{
			ContractID: id, EntryPrice: 0.50, Size: 100, Timestamp: time.Now(),
			Persona: "consensus", Contributors: []string{"gemma-bull"},
			ContributorProposals: []ContributorProposal{
				{Persona: "gemma-bull", EntryPrice: 0.50, Size: 100, Confidence: 0.8},
			},
		}
		tr.positions[id] = pos

		tr.mu.Lock()
		tr.recordClosedTradeLocked(pos, 0.50, pos.Size, "EXIT", "DRY_CLOSED", 0)
		tr.mu.Unlock()

		got := tr.closedTrades[len(tr.closedTrades)-1]
		fam := tr.contractRealizedPnL[contractFamily(id)]
		// Flat trade: gross P&L is exactly 0, so the fee is the entire signal.
		if charge {
			if got.RealizedPnL != -1.75 {
				t.Errorf("charged: trade RealizedPnL = %.2f, want -1.75 (net of fee)", got.RealizedPnL)
			}
			if !got.FeeCharged {
				t.Error("charged: FeeCharged should be stamped so nothing subtracts the fee twice")
			}
			if len(got.PersonaScores) != 1 || got.PersonaScores[0].RealizedPnL != -1.75 {
				t.Errorf("charged: persona score not netted: %+v", got.PersonaScores)
			}
			if got.PersonaScores[0].ReturnPct >= 0 {
				t.Errorf("charged: the allocator learns from ReturnPct, which must go negative, got %.2f",
					got.PersonaScores[0].ReturnPct)
			}
			if fam != -1.75 {
				t.Errorf("charged: family accrual = %.2f, want -1.75 — the risk guards should see real damage", fam)
			}
		} else {
			if got.RealizedPnL != 0 || got.FeeCharged {
				t.Errorf("uncharged: trade must stay gross, got P&L %.2f charged=%v", got.RealizedPnL, got.FeeCharged)
			}
			if got.FeeUSD != 1.75 {
				t.Errorf("uncharged: the fee must still be RECORDED, got %.2f", got.FeeUSD)
			}
			if fam != 0 {
				t.Errorf("uncharged: family accrual = %.2f, want 0", fam)
			}
		}
	}
}

// NetPnLAfterFees must not subtract a fee that is already inside RealizedPnL.
func TestBookMetrics_DoesNotDoubleSubtractChargedFees(t *testing.T) {
	trades := []ClosedTrade{
		{ContractID: "A", EntryPrice: 0.5, Size: 100, RealizedPnL: -1.75, FeeUSD: 1.75, FeeCharged: true, ClosedAtMs: 1},
		{ContractID: "B", EntryPrice: 0.5, Size: 100, RealizedPnL: 0, FeeUSD: 1.75, FeeCharged: false, ClosedAtMs: 2},
	}
	bm := computeBookMetrics(trades, 100000, 20)
	// A is already net (-1.75); only B's fee still needs subtracting.
	if bm.NetPnLAfterFees != -3.50 {
		t.Errorf("NetPnLAfterFees = %.2f, want -3.50 (A already net, B not)", bm.NetPnLAfterFees)
	}
	if bm.FeeUSD != 3.50 {
		t.Errorf("FeeUSD = %.2f, want 3.50 — total modelled fees are still reported in full", bm.FeeUSD)
	}
}
