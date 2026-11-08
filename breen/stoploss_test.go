package main

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestStopLossBreached_PctVsUSDWhicheverFirst(t *testing.T) {
	cases := []struct {
		name             string
		entry, mark      float64
		size             int
		pct, usd, minAbs float64
		want             bool
	}{
		// $5,000 position down 12%: under the -15% limb but past -$300 → USD trips first.
		{"big position trips USD before pct", 0.50, 0.44, 10000, 15, 300, 0, true},
		// $10 position down 20%: past -15% but only -$2 → pct trips first.
		{"small position trips pct before USD", 0.10, 0.08, 100, 15, 300, 0, true},
		// Down 12% / -$600 with pct-only stop → no trip (USD limb disabled).
		{"usd limb disabled", 0.50, 0.44, 10000, 15, 0, 0, false},
		// Down 40% / -$20 with usd-only stop → no trip (pct limb disabled).
		{"pct limb disabled", 0.50, 0.30, 100, 0, 300, 0, false},
		{"both limbs disabled", 0.50, 0.01, 10000, 0, 0, 0, false},
		{"winner never trips", 0.50, 0.70, 10000, 15, 300, 0, false},

		// Absolute floor (STOP_LOSS_MIN_ABS): a sub-floor wiggle can't stop out a
		// cheap contract even when the pct limb is far past its threshold…
		{"one-tick wiggle on longshot held by floor", 0.10, 0.09, 1000, 15, 300, 0.02, false},
		// …nor a big position whose sub-floor move trips the USD limb.
		{"sub-floor move held even past USD limb", 0.50, 0.49, 50000, 15, 300, 0.02, false},
		// A move at/past the floor stops normally.
		{"move at floor trips pct limb", 0.10, 0.08, 1000, 15, 300, 0.02, true},
		{"deep adverse move unaffected by floor", 0.60, 0.40, 1000, 15, 300, 0.02, true},
		// Zero floor = disabled (legacy behavior).
		{"zero floor disabled", 0.10, 0.09, 1000, 5, 300, 0, true},
	}
	for _, c := range cases {
		got, reason := stopLossBreached(c.entry, c.mark, c.size, c.pct, c.usd, c.minAbs)
		if got != c.want {
			t.Errorf("%s: stopLossBreached=%v (reason %q), want %v", c.name, got, reason, c.want)
		}
	}
}

func TestApplyStopLosses_ClosesAndRecords(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.stopLossPct = 15
	tr.stopLossUSD = 300

	id := "KXINXY-26DEC31H1600-B5900"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.60, Size: 1000, Timestamp: time.Now()}
	cashBefore := tr.cash

	// Marked at 0.45: -$150 unrealized = -25% of the $600 cost → past the -15% stop.
	exits := tr.applyStopLosses(map[string]float64{id: 0.45})

	if len(exits) != 1 || exits[0].status != "STOP_LOSS" {
		t.Fatalf("expected one STOP_LOSS exit, got %+v", exits)
	}
	if _, open := tr.positions[id]; open {
		t.Error("stopped-out position should be removed from open positions")
	}
	last := tr.closedTrades[len(tr.closedTrades)-1]
	if last.Status != "STOP_LOSS" || last.ExitPrice != 0.45 {
		t.Errorf("closed trade not recorded as STOP_LOSS @ 0.45: %+v", last)
	}
	approx := func(got, want float64) bool { return math.Abs(got-want) < 1e-6 }
	if got := tr.cash - cashBefore; !approx(got, 450) {
		t.Errorf("cash should be credited the $450 exit proceeds, got $%.2f", got)
	}
	if !approx(tr.realizedPnL, -150) {
		t.Errorf("realizedPnL should be -$150, got $%.2f", tr.realizedPnL)
	}
	fam := contractFamily(id)
	if !approx(tr.contractRealizedPnL[fam], -150) {
		t.Errorf("family loss budget should accrue -$150, got $%.2f", tr.contractRealizedPnL[fam])
	}
	if _, stamped := tr.lastExit[fam]; !stamped {
		t.Error("family lastExit should be stamped so the re-entry cooldown applies")
	}
}

func TestStopLoss_RequiresLiveMark(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.stopLossPct = 15
	tr.stopLossUSD = 300
	// No Redis and no mock books: markPriceLive has nothing live to mark against.
	tr.allowMockBook = false

	id := "KXXRPMAXMON-XRP-26JUL31-180"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.576, Size: 1000, Timestamp: time.Now()}

	tr.checkStopLosses(context.Background())

	if _, open := tr.positions[id]; !open {
		t.Error("with no live mark the stop-loss must not fire — position should stay open")
	}
}

func TestStopLoss_ThenCooldownBlocksReentry(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{ExitCooldown: 30 * time.Minute})
	tr.stopLossPct = 15

	id := "KXBTCY-27JAN0100-B62500"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.50, Size: 1000, Timestamp: time.Now()}
	if exits := tr.applyStopLosses(map[string]float64{id: 0.40}); len(exits) != 1 {
		t.Fatalf("expected the -20%% position stopped out, got %+v", exits)
	}

	reentry := &StrategyProposal{ContractID: id, Action: "BUY", AveragePrice: 0.40, TotalSize: 100}
	if ok, _ := tr.riskGate(reentry); ok {
		t.Error("expected rejection: re-entry into a just-stopped family within the cooldown")
	}
}

func TestFamilyLossLimitCloses(t *testing.T) {
	id := "KXNOTE10Y-26DEC31-T5.14"
	fam := contractFamily(id)

	// Realized -$250 + this strike marked -$100 = -$350 bleed, past the -$300 limit.
	setup := func(closes bool) *PositionTracker {
		tr := newGuardTracker(t, RiskCaps{PerContractLossLimit: 300})
		tr.perContractLossCloses = closes // per-position stop limbs left at 0 to isolate pass 2
		tr.contractRealizedPnL[fam] = -250
		tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.50, Size: 1000, Timestamp: time.Now()}
		return tr
	}

	tr := setup(true)
	exits := tr.applyStopLosses(map[string]float64{id: 0.40})
	if len(exits) != 1 || exits[0].status != "LOSS_LIMIT" {
		t.Fatalf("expected one LOSS_LIMIT exit at -$350 family bleed, got %+v", exits)
	}
	if _, open := tr.positions[id]; open {
		t.Error("family past the loss limit should have its live-marked positions closed")
	}

	tr = setup(false)
	if exits := tr.applyStopLosses(map[string]float64{id: 0.40}); len(exits) != 0 {
		t.Errorf("with PerContractLossCloses=false the position must stay open, got %+v", exits)
	}
	if _, open := tr.positions[id]; !open {
		t.Error("position should remain open when loss-limit closes are disabled")
	}
}
