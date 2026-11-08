package main

import (
	"context"
	"testing"
	"time"
)

func TestParseSettlementHoldPersonas(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"weather", []string{"weather"}},
		{"weather,gemma-bull", []string{"weather", "gemma-bull"}},
		{" Weather , GEMMA-BULL ", []string{"weather", "gemma-bull"}}, // trimmed + lowercased
		{"weather,,", []string{"weather"}},                            // empty entries dropped
		{"", nil},
		{"  ", nil},
	}
	for _, c := range cases {
		got := parsePersonaSet(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %d entries, want %d (%v)", c.in, len(got), len(c.want), got)
			continue
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%q: missing %q, got %v", c.in, w, got)
			}
		}
	}
}

func TestHoldsToSettlement(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})

	// Policy off: nothing is exempt, whoever the author is.
	weather := &Position{ContractID: "KXHIGHNY-26AUG25-T77", Persona: "weather"}
	if tr.holdsToSettlement(weather) {
		t.Error("an empty hold set must exempt nothing")
	}

	tr.settlementHoldPersonas = parsePersonaSet("weather")

	cases := []struct {
		name string
		pos  *Position
		want bool
	}{
		{"direct weather proposal", &Position{Persona: "weather"}, true},
		// The live shape: consensus stamps itself as the persona and carries the real
		// author in Contributors. Matching on Persona alone would never fire.
		{"consensus order authored by weather",
			&Position{Persona: "consensus", Contributors: []string{"weather"}}, true},
		{"consensus order authored by someone else",
			&Position{Persona: "consensus", Contributors: []string{"gemma-bull"}}, false},
		// A mixed order is not a pure settlement play; keep managing it.
		{"mixed authorship is not exempt",
			&Position{Persona: "consensus", Contributors: []string{"weather", "gemma-bull"}}, false},
		{"unattributed", &Position{}, false},
		{"nil position", nil, false},
		{"case insensitive", &Position{Persona: "Weather"}, true},
	}
	for _, c := range cases {
		if got := tr.holdsToSettlement(c.pos); got != c.want {
			t.Errorf("%s: holdsToSettlement = %v, want %v", c.name, got, c.want)
		}
	}
}

// The core of the 2026-08-25 review's fix #2: a weather position 30% underwater on the
// mark must NOT be stopped out, because the mark is not what the strategy is predicting.
// The same position under the old policy is stopped.
func TestApplyStopLosses_SettlementHoldSkipsPercentExits(t *testing.T) {
	newPos := func() *Position {
		return &Position{
			ContractID: "KXHIGHNY-26AUG25-T77", EntryPrice: 0.23, Size: 100, Timestamp: time.Now(),
			Persona: "consensus", Contributors: []string{"weather"},
			BestMark: 0.35, WorstMark: 0.16,
		}
	}
	id := "KXHIGHNY-26AUG25-T77"

	// Baseline: without the policy, the 15% stop fires at 0.16 (−30% of entry).
	base := newGuardTracker(t, RiskCaps{})
	base.stopLossPct, base.stopLossUSD, base.stopLossMinAbs = 15, 300, 0.02
	base.positions[id] = newPos()
	if exits := base.applyStopLosses(map[string]float64{id: 0.16}); len(exits) != 1 {
		t.Fatalf("precondition: the old policy should stop this position out, got %+v", exits)
	}

	// With the policy on, the same mark leaves it open to reach resolution.
	hold := newGuardTracker(t, RiskCaps{})
	hold.stopLossPct, hold.stopLossUSD, hold.stopLossMinAbs = 15, 300, 0.02
	hold.takeProfitPct = 40
	hold.trailArmPct, hold.trailRetracePct = 15, 10
	hold.settlementHoldPersonas = parsePersonaSet("weather")
	hold.positions[id] = newPos()

	if exits := hold.applyStopLosses(map[string]float64{id: 0.16}); len(exits) != 0 {
		t.Errorf("settlement-hold position was exited on the mark: %+v", exits)
	}
	// Nor on the upside: a 40% take-profit and an armed trail must both stay silent.
	if exits := hold.applyStopLosses(map[string]float64{id: 0.33}); len(exits) != 0 {
		t.Errorf("settlement-hold position was taken profit / trailed out: %+v", exits)
	}
	if _, open := hold.positions[id]; !open {
		t.Error("expected the position to still be open, holding for resolution")
	}
}

// "Risk control in dollars, not in ticks" — the dollar backstop is the one exit that
// still applies, so a settlement position can never bleed without limit.
func TestApplyStopLosses_SettlementHoldKeepsDollarBackstop(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.stopLossPct, tr.stopLossUSD, tr.stopLossMinAbs = 15, 300, 0.02
	tr.settlementHoldPersonas = parsePersonaSet("weather")

	id := "KXHIGHNY-26AUG25-T77"
	// 10,000 contracts at 0.23: a move to 0.19 is −$400, past the −$300 dollar limb.
	tr.positions[id] = &Position{ContractID: id, EntryPrice: 0.23, Size: 10000, Timestamp: time.Now(),
		Persona: "weather", BestMark: 0.23, WorstMark: 0.19}

	exits := tr.applyStopLosses(map[string]float64{id: 0.19})
	if len(exits) != 1 || exits[0].status != "STOP_LOSS" {
		t.Fatalf("the dollar backstop must still fire on a hold position, got %+v", exits)
	}
}

// Force-exit is the other half of the leak: selling the position back at the mark an
// hour before resolution is exactly what the review priced at −$428 vs +$2,950.
func TestForceExitNearExpiry_SkipsSettlementHoldPositions(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.forceExitWindow = time.Hour
	tr.settlementHoldPersonas = parsePersonaSet("weather")

	soon := time.Now().Add(20 * time.Minute)
	held := &Position{ContractID: "KXHIGHNY-26AUG25-T77", EntryPrice: 0.23, Size: 100,
		Timestamp: time.Now(), Expiry: soon, Persona: "weather"}
	other := &Position{ContractID: "KXINXY-26DEC31H1600-B5900", EntryPrice: 0.50, Size: 100,
		Timestamp: time.Now(), Expiry: soon, Persona: "gemma-bull"}
	tr.positions[held.ContractID] = held
	tr.positions[other.ContractID] = other

	tr.forceExitNearExpiry(context.Background())

	if _, open := tr.positions[held.ContractID]; !open {
		t.Error("a settlement-hold position must ride through the force-exit window to resolution")
	}
	if _, open := tr.positions[other.ContractID]; open {
		t.Error("a non-hold position should still be force-exited before expiry")
	}
}

// ── settlement take-profit (2026-09-03 forensics) ───────────────────────────

func TestSettlementTakeProfit(t *testing.T) {
	cases := []struct {
		name             string
		entry, mark, thr float64
		want             bool
	}{
		{"off by default", 0.15, 0.99, 0, false},
		{"below threshold", 0.15, 0.94, 0.95, false},
		{"at threshold", 0.15, 0.95, 0.95, true},
		{"above threshold", 0.15, 0.99, 0.95, true},
		// The six weather zeros this exists for: KXHIGHCHI-26AUG30-T86 was entered
		// expensive at 0.890 and still peaked at 0.985 before settling at $0.00.
		{"expensive entry still clears", 0.890, 0.95, 0.95, true},
		// ...but a position entered ABOVE the threshold must never be "taken profit"
		// at a loss just because its mark is a high number.
		{"entry above threshold is not a profit", 0.97, 0.95, 0.95, false},
		{"mark equal to entry is not a profit", 0.96, 0.96, 0.95, false},
	}
	for _, c := range cases {
		got, reason := settlementTakeProfit(c.entry, c.mark, 100, c.thr)
		if got != c.want {
			t.Errorf("%s: settlementTakeProfit(%.3f, %.2f, %.2f) = %v, want %v", c.name, c.entry, c.mark, c.thr, got, c.want)
		}
		if got && reason == "" {
			t.Errorf("%s: a firing take-profit must carry a reason", c.name)
		}
	}
}

// The whole point of the refinement: the hold policy still ignores every ordinary
// mark, and sells on exactly one — the price at which the market has come over to us.
func TestApplyStopLosses_SettlementHoldTakesProfitAtNearCertainty(t *testing.T) {
	id := "KXHIGHCHI-26SEP03-B92.5"
	newTracker := func() *PositionTracker {
		tr := newGuardTracker(t, RiskCaps{})
		tr.stopLossPct, tr.stopLossUSD, tr.stopLossMinAbs = 15, 300, 0.02
		tr.takeProfitPct = 40
		tr.settlementHoldPersonas = parsePersonaSet("weather")
		tr.settlementHoldTPPrice = 0.95
		tr.positions[id] = &Position{
			ContractID: id, EntryPrice: 0.15, Size: 150, Timestamp: time.Now(),
			Persona: "consensus", Contributors: []string{"weather"},
			BestMark: 0.15, WorstMark: 0.15,
		}
		return tr
	}

	// A mark that has more than doubled is still not a reason to sell — the 40%
	// take-profit stays disabled for a held position, as the desk review intended.
	quiet := newTracker()
	if exits := quiet.applyStopLosses(map[string]float64{id: 0.40}); len(exits) != 0 {
		t.Errorf("a held position must not be banked on an ordinary mark: %+v", exits)
	}

	// At the near-certainty bid it is banked, at that bid, as a TAKE_PROFIT.
	fires := newTracker()
	exits := fires.applyStopLosses(map[string]float64{id: 0.99})
	if len(exits) != 1 || exits[0].status != "TAKE_PROFIT" {
		t.Fatalf("expected one TAKE_PROFIT at the near-certainty bid, got %+v", exits)
	}
	if exits[0].mark != 0.99 {
		t.Errorf("exit booked at %.2f, want the 0.99 bid", exits[0].mark)
	}
	if want := (0.99 - 0.15) * 150; exits[0].yield != want {
		t.Errorf("yield $%.2f, want $%.2f", exits[0].yield, want)
	}
	if _, open := fires.positions[id]; open {
		t.Error("the position should be closed")
	}

	// With the knob off, the same mark rides to resolution — the pre-2026-09-03 policy.
	off := newTracker()
	off.settlementHoldTPPrice = 0
	if exits := off.applyStopLosses(map[string]float64{id: 0.99}); len(exits) != 0 {
		t.Errorf("SETTLEMENT_HOLD_TP_PRICE=0 must hold to resolution: %+v", exits)
	}
}
