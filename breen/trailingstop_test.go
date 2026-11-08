package main

import (
	"context"
	"testing"
	"time"
)

// Trailing stop (breen-intelligence Phase 3). Built on the Phase 0 excursion record:
// pos.BestMark is the high-water mark of the LIVE marks seen while the position was
// open, so the trail is measuring a give-back that actually happened rather than
// inferring one from the current quote.

func TestTrailingStopHit(t *testing.T) {
	cases := []struct {
		name              string
		entry, best, mark float64
		size              int
		arm, retrace      float64
		minAbs            float64
		want              bool
	}{
		// Ran 0.20 → 0.40 (+100%, armed at +20%), now 0.34: gave back 0.06 = 30% of
		// entry, past the 10% give-back limit.
		{"armed and retraced", 0.20, 0.40, 0.34, 100, 20, 10, 0, true},
		// Same run, only 0.01 given back = 5% of entry — still riding.
		{"armed but give-back too small", 0.20, 0.40, 0.39, 100, 20, 10, 0, false},
		// Never ran: the trail must not turn a position that was only ever underwater
		// into an early exit. That case belongs to the stop-loss.
		{"never armed, deep loss", 0.20, 0.21, 0.05, 100, 20, 10, 0, false},
		// Ran +15% against a +20% arm — close, but the trail is not live yet.
		{"just short of arming", 0.20, 0.23, 0.20, 100, 20, 10, 0, false},
		// Exactly at the arm threshold arms it (≥, not >).
		{"exactly at arm threshold", 0.20, 0.24, 0.21, 100, 20, 10, 0, true},
		// A new high on this very tick is not a give-back, even though best < mark
		// would otherwise compute a negative retrace.
		{"new high this tick", 0.20, 0.40, 0.44, 100, 20, 10, 0, false},
		// Either limb at zero disables the whole feature (the default).
		{"disabled: no arm", 0.20, 0.40, 0.30, 100, 0, 10, 0, false},
		{"disabled: no retrace", 0.20, 0.40, 0.30, 100, 20, 0, 0, false},
		// A replayed position carries no excursion record; entry stands in as the high,
		// which leaves it unarmed rather than instantly triggering.
		{"unmarked position", 0.20, 0, 0.10, 100, 20, 10, 0, false},
		{"degenerate entry", 0, 0.40, 0.30, 100, 20, 10, 0, false},

		// The tick-noise floor, shared with the stop-loss. On a 3-cent contract that
		// ran to 6 cents, ONE tick down is a 33%-of-entry "give-back" — without the
		// floor the trail would bank every armed longshot on book noise.
		{"one tick on a cheap contract held by the floor", 0.03, 0.06, 0.05, 100, 20, 10, 0.02, false},
		// A real 3-cent give-back on the same contract clears the floor and fires.
		{"real give-back clears the floor", 0.03, 0.06, 0.03, 100, 20, 10, 0.02, true},
		// With the floor disabled the same one-tick wiggle does fire — this is what the
		// floor exists to prevent, pinned so the interaction stays visible.
		{"same wiggle fires with floor off", 0.03, 0.06, 0.05, 100, 20, 10, 0, true},
	}
	for _, c := range cases {
		got, reason := trailingStopHit(c.entry, c.best, c.mark, c.size, c.arm, c.retrace, c.minAbs)
		if got != c.want {
			t.Errorf("%s: trailingStopHit = %v (%q), want %v", c.name, got, reason, c.want)
		}
		if got && reason == "" {
			t.Errorf("%s: a firing trail must explain itself", c.name)
		}
	}
}

func TestApplyStopLosses_TrailStopClosesAndLabels(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.stopLossPct, tr.stopLossUSD = 0, 0 // isolate the trail
	tr.trailArmPct, tr.trailRetracePct = 20, 10

	id := "KXINXY-26DEC31H1600-B5900"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.20, Size: 1000, Timestamp: time.Now(),
		BestMark: 0.40, WorstMark: 0.20}
	cashBefore := tr.cash

	exits := tr.applyStopLosses(map[string]float64{id: 0.34})

	if len(exits) != 1 || exits[0].status != "TRAIL_STOP" {
		t.Fatalf("expected one TRAIL_STOP exit, got %+v", exits)
	}
	if _, stillOpen := tr.positions[id]; stillOpen {
		t.Error("expected the position to be closed")
	}
	// Banked at 0.34 against a 0.20 basis: +$140 on 1000 contracts.
	if got := tr.cash - cashBefore; got != 340 {
		t.Errorf("cash credited %.2f, want 340 (1000 × 0.34)", got)
	}
	if tr.realizedPnL != 140 {
		t.Errorf("realized %.2f, want +140 — the trail banks a WINNER", tr.realizedPnL)
	}
	got := tr.closedTrades[len(tr.closedTrades)-1]
	if got.Status != "TRAIL_STOP" {
		t.Errorf("closed trade status = %q, want TRAIL_STOP", got.Status)
	}
	// The excursion record that armed the trail rides along on the trade.
	if got.MaxFavorableExcursionPct != 100 {
		t.Errorf("MFE = %.2f%%, want 100%% (0.20 → 0.40)", got.MaxFavorableExcursionPct)
	}
}

// A position can be both past its stop and off its high. The stop is the urgent case
// and must win, so the trade is labelled by the thing that actually forced it out.
func TestApplyStopLosses_StopLossBeatsTrail(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.stopLossPct, tr.stopLossMinAbs = 15, 0
	tr.trailArmPct, tr.trailRetracePct = 20, 10

	id := "KXINXY-26DEC31H1600-B5900"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.20, Size: 1000, Timestamp: time.Now(),
		BestMark: 0.40, WorstMark: 0.20}

	// 0.10 is both a -50% loss and a big retrace from the 0.40 high.
	exits := tr.applyStopLosses(map[string]float64{id: 0.10})
	if len(exits) != 1 || exits[0].status != "STOP_LOSS" {
		t.Fatalf("expected STOP_LOSS to win the tie, got %+v", exits)
	}
}

// The trail must be able to run as the ONLY exit limb: before Phase 3 the exit pass
// was skipped entirely unless a stop/take-profit/loss-limit was configured, so a run
// with just the trail set would silently never harvest anything.
func TestCheckStopLosses_TrailAloneEnablesTheExitPass(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.allowMockBook = true // deterministic keyless book
	tr.stopLossPct, tr.stopLossUSD, tr.stopLossMinAbs = 0, 0, 0
	tr.takeProfitPct, tr.takeProfitUSD = 0, 0
	tr.perContractLossCloses = false
	tr.trailArmPct, tr.trailRetracePct = 20, 10

	// generateMockBook: 7-char ticker → base 0.37, so bid 0.36 / ask 0.37, mid 0.365.
	id := "KXTRAIL"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.20, Size: 100, Timestamp: time.Now(),
		BestMark: 0.40, WorstMark: 0.20}

	tr.checkStopLosses(context.Background())

	if _, stillOpen := tr.positions[id]; stillOpen {
		t.Fatal("trail-only configuration must still run the exit pass and close the position")
	}
	if got := tr.closedTrades[len(tr.closedTrades)-1].Status; got != "TRAIL_STOP" {
		t.Errorf("status = %q, want TRAIL_STOP", got)
	}
}

// Excursions must keep accruing while the trail is armed but not yet triggered —
// the high-water mark is the thing the trail is measured against, so a tick that sets
// a new high has to move it.
func TestCheckStopLosses_TrailRidesANewHigh(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.trailArmPct, tr.trailRetracePct = 20, 10

	id := "KXINXY-26DEC31H1600-B5900"
	pos := &Position{ContractID: id, EntryPrice: 0.20, Size: 100, Timestamp: time.Now(),
		BestMark: 0.30, WorstMark: 0.20}
	tr.positions[id] = pos

	tr.updateExcursions(map[string]float64{id: 0.45}) // new high
	if exits := tr.applyStopLosses(map[string]float64{id: 0.45}); len(exits) != 0 {
		t.Fatalf("a new high is not a give-back, got %+v", exits)
	}
	if pos.BestMark != 0.45 {
		t.Errorf("BestMark = %.2f, want the new high 0.45", pos.BestMark)
	}
	// Now it fades from the new high: 0.45 → 0.41 is 0.04 = 20% of entry.
	if exits := tr.applyStopLosses(map[string]float64{id: 0.41}); len(exits) != 1 {
		t.Fatalf("expected the trail to fire off the RAISED high, got %+v", exits)
	}
}
