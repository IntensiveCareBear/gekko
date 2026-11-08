package main

import (
	"math"
	"testing"
	"time"
)

func TestTakeProfitReached_PctVsUSDWhicheverFirst(t *testing.T) {
	cases := []struct {
		name        string
		entry, mark float64
		size        int
		pct, usd    float64
		want        bool
	}{
		// $5,000 position up 12%: under the +15% limb but past +$300 → USD trips first.
		{"big position trips USD before pct", 0.50, 0.56, 10000, 15, 300, true},
		// $10 position up 20%: past +15% but only +$2 → pct trips first.
		{"small position trips pct before USD", 0.10, 0.12, 100, 15, 300, true},
		// Up 12% / +$600 with pct-only target → no trip (USD limb disabled).
		{"usd limb disabled", 0.50, 0.56, 10000, 15, 0, false},
		// Up 40% / +$20 with usd-only target → no trip (pct limb disabled).
		{"pct limb disabled", 0.50, 0.70, 100, 0, 300, false},
		{"both limbs disabled", 0.50, 0.99, 10000, 0, 0, false},
		{"loser never trips", 0.50, 0.30, 10000, 15, 300, false},
		// Just past the +15% threshold trips (≥, not >).
		{"at pct threshold trips", 0.50, 0.58, 1000, 15, 0, true},
	}
	for _, c := range cases {
		got, reason := takeProfitReached(c.entry, c.mark, c.size, c.pct, c.usd)
		if got != c.want {
			t.Errorf("%s: takeProfitReached=%v (reason %q), want %v", c.name, got, reason, c.want)
		}
	}
}

func TestApplyStopLosses_BanksTakeProfit(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.takeProfitPct = 25
	tr.stopLossPct = 15 // both armed; the gain must route through take-profit, not the stop

	id := "KXINXY-26DEC31H1600-B5900"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.40, Size: 1000, Timestamp: time.Now()}
	cashBefore := tr.cash

	// Marked at 0.55: +$150 unrealized = +37.5% of the $400 cost → past the +25% target.
	exits := tr.applyStopLosses(map[string]float64{id: 0.55})

	if len(exits) != 1 || exits[0].status != "TAKE_PROFIT" {
		t.Fatalf("expected one TAKE_PROFIT exit, got %+v", exits)
	}
	if _, open := tr.positions[id]; open {
		t.Error("banked position should be removed from open positions")
	}
	last := tr.closedTrades[len(tr.closedTrades)-1]
	if last.Status != "TAKE_PROFIT" || last.ExitPrice != 0.55 {
		t.Errorf("closed trade not recorded as TAKE_PROFIT @ 0.55: %+v", last)
	}
	approx := func(got, want float64) bool { return math.Abs(got-want) < 1e-6 }
	if got := tr.cash - cashBefore; !approx(got, 550) {
		t.Errorf("cash should be credited the $550 exit proceeds, got $%.2f", got)
	}
	if !approx(tr.realizedPnL, 150) {
		t.Errorf("realizedPnL should be +$150, got $%.2f", tr.realizedPnL)
	}
}

func TestTakeProfitDisabledByDefault(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	// takeProfitPct/USD left at 0 (default off): a big winner must ride, not bank.
	id := "KXBTCY-27JAN0100-B62500"
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.20, Size: 1000, Timestamp: time.Now()}

	if exits := tr.applyStopLosses(map[string]float64{id: 0.90}); len(exits) != 0 {
		t.Errorf("with take-profit disabled a +350%% winner must stay open, got %+v", exits)
	}
}
