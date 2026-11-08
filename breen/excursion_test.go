package main

import (
	"testing"
	"time"
)

// Excursion tracking (breen-intelligence Phase 0): the running best/worst LIVE mark
// on every open position, stamped onto the closed trade as MFE/MAE. This is the
// dataset Phase 3's exit policy is argued from, so the accounting has to be right
// before any knob reads it.

func TestObserveMark_TracksExtremesFromEntryBasis(t *testing.T) {
	pos := &Position{ContractID: "KXTEST", EntryPrice: 0.40, Size: 100}

	// A position that only ever went up must still report the entry as its worst.
	for _, m := range []float64{0.45, 0.52, 0.48} {
		pos.observeMark(m)
	}
	if pos.BestMark != 0.52 {
		t.Errorf("BestMark = %.2f, want 0.52", pos.BestMark)
	}
	if pos.WorstMark != 0.40 {
		t.Errorf("WorstMark = %.2f, want the entry basis 0.40", pos.WorstMark)
	}

	pos.observeMark(0.31)
	if pos.WorstMark != 0.31 {
		t.Errorf("WorstMark = %.2f, want 0.31", pos.WorstMark)
	}
	if pos.BestMark != 0.52 {
		t.Errorf("BestMark moved on an adverse mark: %.2f", pos.BestMark)
	}
}

func TestObserveMark_IgnoresNonPositiveMarks(t *testing.T) {
	pos := &Position{ContractID: "KXTEST", EntryPrice: 0.40, Size: 100}
	pos.observeMark(0.55)
	pos.observeMark(0)
	pos.observeMark(-1)
	if pos.WorstMark != 0.40 || pos.BestMark != 0.55 {
		t.Errorf("a book quoting nothing must not move the excursion record: best=%.2f worst=%.2f",
			pos.BestMark, pos.WorstMark)
	}
}

func TestResetExcursions_OnAddRebasesToNewEntry(t *testing.T) {
	pos := &Position{ContractID: "KXTEST", EntryPrice: 0.40, Size: 100}
	pos.observeMark(0.60)
	pos.observeMark(0.30)

	// An add re-averages the basis to 0.50; the old ±0.20 excursion was measured
	// against a basis that no longer exists.
	pos.EntryPrice = 0.50
	pos.resetExcursions()
	if pos.BestMark != 0.50 || pos.WorstMark != 0.50 {
		t.Errorf("reset should seed both extremes at the new basis, got best=%.2f worst=%.2f",
			pos.BestMark, pos.WorstMark)
	}
	mfe, mae := excursionPct(pos, 0)
	if mfe != 0 || mae != 0 {
		t.Errorf("a freshly rebased position has no excursion yet, got mfe=%.2f mae=%.2f", mfe, mae)
	}
}

func TestExcursionPct(t *testing.T) {
	cases := []struct {
		name             string
		entry            float64
		best, worst      float64
		exit             float64
		wantMFE, wantMAE float64
	}{
		{"ran up then closed flat", 0.50, 0.65, 0.50, 0.50, 30, 0},
		{"ran down then recovered", 0.50, 0.50, 0.40, 0.50, 0, -20},
		{"both directions", 0.50, 0.60, 0.45, 0.55, 20, -10},
		// The exit is a real observed price: a stop that fired below every sampled
		// mark still defines the adverse extreme.
		{"exit beyond the sampled worst", 0.50, 0.55, 0.48, 0.35, 10, -30},
		{"exit beyond the sampled best", 0.50, 0.55, 0.48, 0.70, 40, -4},
		// Never marked (replayed position): the exit alone defines both.
		{"unmarked position", 0.50, 0, 0, 0.60, 20, 0},
		// Binary settlement: caller passes 0 so a $0/$1 payout can't masquerade as a
		// price the position could have been exited at.
		{"settlement excluded", 0.50, 0.58, 0.44, 0, 16, -12},
		{"degenerate entry", 0, 0.5, 0.1, 0.4, 0, 0},
	}
	for _, c := range cases {
		pos := &Position{ContractID: "KXTEST", EntryPrice: c.entry, BestMark: c.best, WorstMark: c.worst, Size: 100}
		mfe, mae := excursionPct(pos, c.exit)
		if mfe != c.wantMFE || mae != c.wantMAE {
			t.Errorf("%s: excursionPct = (%.2f, %.2f), want (%.2f, %.2f)", c.name, mfe, mae, c.wantMFE, c.wantMAE)
		}
	}
}

func TestUpdateExcursions_FoldsTickMarks(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	id := "KXINXY-26DEC31H1600-B5900"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.60, Size: 100, Timestamp: time.Now(),
		BestMark: 0.60, WorstMark: 0.60}

	tr.updateExcursions(map[string]float64{id: 0.72})
	tr.updateExcursions(map[string]float64{id: 0.51})
	tr.updateExcursions(map[string]float64{"KXNOTHELD": 0.99}) // must not panic or create anything

	pos := tr.positions[id]
	if pos.BestMark != 0.72 || pos.WorstMark != 0.51 {
		t.Errorf("extremes = (%.2f, %.2f), want (0.72, 0.51)", pos.BestMark, pos.WorstMark)
	}
	if len(tr.positions) != 1 {
		t.Errorf("a mark for an unheld contract must not create a position (have %d)", len(tr.positions))
	}
}

// The whole point of lifting the mark fetch out of the exit-limb check: excursions
// are measurement, not policy, and must keep accruing on a run with every exit limb
// switched off. Before Phase 0, checkStopLosses returned before ever fetching marks
// in that configuration.
func TestRecordClose_StampsExcursionsWithExitsDisabled(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.stopLossPct, tr.stopLossUSD, tr.takeProfitPct, tr.takeProfitUSD = 0, 0, 0, 0
	tr.perContractLossCloses = false

	id := "KXINXY-26DEC31H1600-B5900"
	pos := &Position{ContractID: id, EntryPrice: 0.50, Size: 100, Timestamp: time.Now()}
	tr.positions[id] = pos

	tr.updateExcursions(map[string]float64{id: 0.66})
	tr.updateExcursions(map[string]float64{id: 0.41})

	tr.mu.Lock()
	tr.recordClosedTradeLocked(pos, 0.55, pos.Size, "EXIT", "DRY_CLOSED", 0)
	tr.mu.Unlock()

	got := tr.closedTrades[len(tr.closedTrades)-1]
	if got.MaxFavorableExcursionPct != 32 {
		t.Errorf("MFE = %.2f%%, want 32%% (0.50 → 0.66)", got.MaxFavorableExcursionPct)
	}
	if got.MaxAdverseExcursionPct != -18 {
		t.Errorf("MAE = %.2f%%, want -18%% (0.50 → 0.41)", got.MaxAdverseExcursionPct)
	}
}

// A binary resolution is a payout, not an exit price: folding it in would report
// MAE = -100% for every settled loser and drown the live-mark signal.
func TestRecordClose_SettlementDoesNotFoldPayoutIntoExcursion(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	id := "KXINXY-26DEC31H1600-B5900"
	pos := &Position{ContractID: id, EntryPrice: 0.50, Size: 100, Timestamp: time.Now(),
		BestMark: 0.58, WorstMark: 0.44}
	tr.positions[id] = pos

	tr.mu.Lock()
	tr.recordClosedTradeLocked(pos, 0, pos.Size, "NO", "RESOLVED", 0)
	tr.mu.Unlock()

	got := tr.closedTrades[len(tr.closedTrades)-1]
	if got.MaxAdverseExcursionPct != -12 {
		t.Errorf("MAE = %.2f%%, want -12%% from the live marks, not -100%% from the payout",
			got.MaxAdverseExcursionPct)
	}
	if got.MaxFavorableExcursionPct != 16 {
		t.Errorf("MFE = %.2f%%, want 16%%", got.MaxFavorableExcursionPct)
	}
}
