package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// TestShadowProposal_Actionable pins the denominator of the re-activation bar. The
// bearish cases are the 2026-08-30 finding: 33 of the first 55 recorded proposals were
// sizeless signals, and counting them would have let the book "reach 200" without ever
// proposing something that could be scored.
func TestShadowProposal_Actionable(t *testing.T) {
	cases := []struct {
		name string
		sp   ShadowProposal
		want bool
	}{
		{"a sized BUY is a would-be trade", ShadowProposal{Action: "BUY", Price: 0.30, Size: 100}, true},
		{"lower-case action still counts", ShadowProposal{Action: "buy", Price: 0.30, Size: 100}, true},
		{"a bearish signal is not scoreable", ShadowProposal{Action: "SELL", Bearish: true}, false},
		{"a sizeless proposal has no expectancy", ShadowProposal{Action: "BUY", Price: 0.30, Size: 0}, false},
		{"a priceless proposal has no basis", ShadowProposal{Action: "BUY", Price: 0, Size: 100}, false},
		// Belt and braces: a bearish flag wins even if a size somehow rode along.
		{"bearish wins over a stray size", ShadowProposal{Action: "BUY", Bearish: true, Price: 0.3, Size: 10}, false},
	}
	for _, c := range cases {
		if got := c.sp.actionable(); got != c.want {
			t.Errorf("%s: actionable() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScoreShadowProposal_NetOfFees(t *testing.T) {
	sp := ShadowProposal{ContractID: "KXHIGHNY-26AUG30-B82.5", Action: "BUY", Price: 0.40, Size: 100}
	const rate = 0.07

	// Settled YES. Gross = (1.00 - 0.40) × 100 = $60. Kalshi bills no fee at
	// settlement, so only the entry leg is charged.
	entryFee := kalshiFee(sp.ContractID, 0.40, 100, rate, true)
	if entryFee <= 0 {
		t.Fatalf("entry fee should be nonzero, got %.4f", entryFee)
	}
	net, notional := scoreShadowProposal(sp, 1.0, rate, true)
	if math.Abs(notional-40.0) > 1e-9 {
		t.Errorf("notional = %.4f, want 40.00", notional)
	}
	if want := 60.0 - entryFee; math.Abs(net-want) > 1e-9 {
		t.Errorf("settled-YES net = %.4f, want %.4f (gross 60 minus entry fee only)", net, want)
	}

	// Settled NO: the whole stake is gone, plus the entry fee.
	net, _ = scoreShadowProposal(sp, 0.0, rate, true)
	if want := -40.0 - entryFee; math.Abs(net-want) > 1e-9 {
		t.Errorf("settled-NO net = %.4f, want %.4f", net, want)
	}

	// Marked exit mid-life pays BOTH legs.
	net, _ = scoreShadowProposal(sp, 0.50, rate, true)
	exitFee := kalshiFee(sp.ContractID, 0.50, 100, rate, true)
	if want := 10.0 - entryFee - exitFee; math.Abs(net-want) > 1e-9 {
		t.Errorf("marked net = %.4f, want %.4f (both legs charged)", net, want)
	}

	// A flat round trip must score NEGATIVE once fees are charged — the whole point of
	// measuring net-of-fee expectancy rather than gross.
	if net, _ := scoreShadowProposal(sp, 0.40, rate, true); net >= 0 {
		t.Errorf("a flat round trip scored %+.4f; fees must make it a loss", net)
	}
}

// counts builds the per-persona tallies buildShadowScorecard expects.
func counts(m map[string][2]int) map[string]struct{ Actionable, Signals int } {
	out := map[string]struct{ Actionable, Signals int }{}
	for k, v := range m {
		out[k] = struct{ Actionable, Signals int }{v[0], v[1]}
	}
	return out
}

func TestBuildShadowScorecard_BarNeedsBothHalves(t *testing.T) {
	// Profitable, but nowhere near enough evidence.
	rows := []ShadowScore{
		{Personas: []string{"gemma-bull"}, NetPnLUSD: 50, NotionalUSD: 100, Outcome: shadowSettled},
		{Personas: []string{"gemma-bull"}, NetPnLUSD: 10, NotionalUSD: 100, Outcome: shadowMarked},
	}
	sc := buildShadowScorecard(rows, 2, 0, counts(map[string][2]int{"gemma-bull": {2, 0}}), 200, 0)
	if sc.BarMet {
		t.Errorf("bar must not be met on 2 proposals, however profitable: %+v", sc.Verdict)
	}
	if sc.ExpectancyPct <= 0 {
		t.Errorf("expectancy = %.2f%%, want positive", sc.ExpectancyPct)
	}
	if sc.Scored != 2 || sc.SettledCount != 1 || sc.MarkedCount != 1 {
		t.Errorf("scored/settled/marked = %d/%d/%d, want 2/1/1", sc.Scored, sc.SettledCount, sc.MarkedCount)
	}

	// Enough evidence, but it says the book loses money.
	var many []ShadowScore
	for i := 0; i < 200; i++ {
		many = append(many, ShadowScore{Personas: []string{"gemma-bull"}, NetPnLUSD: -1, NotionalUSD: 100, Outcome: shadowSettled})
	}
	sc = buildShadowScorecard(many, 200, 0, counts(map[string][2]int{"gemma-bull": {200, 0}}), 200, 0)
	if sc.BarMet {
		t.Error("bar must not be met by a losing book, however many proposals")
	}

	// Both halves.
	for i := range many {
		many[i].NetPnLUSD = 1
	}
	sc = buildShadowScorecard(many, 200, 0, counts(map[string][2]int{"gemma-bull": {200, 0}}), 200, 0)
	if !sc.BarMet {
		t.Errorf("200 scored proposals at %+.2f%% should meet the bar", sc.ExpectancyPct)
	}
	if !sc.PerPersona["gemma-bull"].BarMet {
		t.Error("the persona that earned it should show bar_met")
	}
}

// TestBuildShadowScorecard_SignalsDoNotAdvanceTheBar is the reason this file exists.
func TestBuildShadowScorecard_SignalsDoNotAdvanceTheBar(t *testing.T) {
	// One scoreable trade, 500 bearish signals. The bar must still be far away.
	rows := []ShadowScore{{Personas: []string{"gemma-bear"}, NetPnLUSD: 5, NotionalUSD: 100, Outcome: shadowSettled}}
	sc := buildShadowScorecard(rows, 501, 500, counts(map[string][2]int{"gemma-bear": {1, 500}}), 200, 0)

	if sc.BarMet {
		t.Error("500 sizeless signals plus one winning trade must not meet a 200-trade bar")
	}
	if sc.Scored != 1 {
		t.Errorf("scored = %d, want 1: only actionable proposals count", sc.Scored)
	}
	if sc.Signals != 500 || sc.Recorded != 501 {
		t.Errorf("signals/recorded = %d/%d, want 500/501: signals are still reported, just not counted",
			sc.Signals, sc.Recorded)
	}
	if p := sc.PerPersona["gemma-bear"]; p.Signals != 500 || p.Actionable != 1 {
		t.Errorf("per-persona signals/actionable = %d/%d, want 500/1", p.Signals, p.Actionable)
	}
}

// A proposal with no observable outcome must not be scored as a zero — that would
// silently drag expectancy toward 0% and let a book with no evidence look neutral.
func TestBuildShadowScorecard_PendingIsNotAZero(t *testing.T) {
	rows := []ShadowScore{
		{Personas: []string{"gemma-neutral"}, NetPnLUSD: 20, NotionalUSD: 100, Outcome: shadowSettled},
		{Personas: []string{"gemma-neutral"}, Outcome: shadowPending},
		{Personas: []string{"gemma-neutral"}, Outcome: shadowPending},
	}
	sc := buildShadowScorecard(rows, 3, 0, counts(map[string][2]int{"gemma-neutral": {3, 0}}), 200, 0)

	if sc.Scored != 1 || sc.PendingCount != 2 {
		t.Errorf("scored/pending = %d/%d, want 1/2", sc.Scored, sc.PendingCount)
	}
	if math.Abs(sc.ExpectancyPct-20.0) > 1e-9 {
		t.Errorf("expectancy = %.2f%%, want 20%%: pending rows must not dilute it", sc.ExpectancyPct)
	}
	if sc.Actionable != 3 {
		t.Errorf("actionable = %d, want 3: pending proposals are still actionable, just unresolved", sc.Actionable)
	}
}

func TestShadowFingerprint_Stability(t *testing.T) {
	a := ShadowProposal{ContractID: "KXCPI-26AUG-T0.3", Personas: []string{"gemma-bull"}, ProposedAt: 1788152175440}
	b := a
	if shadowFingerprint(a) != shadowFingerprint(b) {
		t.Error("the same proposal must fingerprint identically, or settled verdicts re-freeze forever")
	}
	// A later re-proposal of the same contract is a different proposal.
	c := a
	c.ProposedAt = a.ProposedAt + 1
	if shadowFingerprint(a) == shadowFingerprint(c) {
		t.Error("proposals at different times must not collide")
	}
}

// ── breadth and concentration (2026-09-03 forensics) ────────────────────────

func TestShadowSeries(t *testing.T) {
	cases := map[string]string{
		"KXBRENTW-26SEP0417-T84.99": "KXBRENTW",
		"KXBRENTW-26SEP1117-T90":    "KXBRENTW", // a different week is the SAME trade
		"KXHIGHNY-26AUG27-B80.5":    "KXHIGHNY",
		"POLY-6384252906871000571":  "POLY",
		"NODASHES":                  "NODASHES",
		"-leading":                  "-leading",
	}
	for in, want := range cases {
		if got := shadowSeries(in); got != want {
			t.Errorf("shadowSeries(%q) = %q, want %q", in, got, want)
		}
	}
}

// The 2026-09-03 book in miniature: plenty of proposals, plenty of profit, and all of
// it one weekly oil series proposed over and over. The old bar passed this.
func TestBuildShadowScorecard_ConcentratedBookFailsTheBar(t *testing.T) {
	var rows []ShadowScore
	// 150 proposals, but only 5 distinct Brent contracts, carrying every dollar.
	for i := 0; i < 150; i++ {
		rows = append(rows, ShadowScore{
			ContractID: fmt.Sprintf("KXBRENTW-26SEP0417-T%d", 80+i%5),
			Personas:   []string{"gemma-bear"}, NetPnLUSD: 30, NotionalUSD: 100, Outcome: shadowSettled,
		})
	}
	// 60 proposals across everything else, losing money — the honest half of the book.
	for i := 0; i < 60; i++ {
		rows = append(rows, ShadowScore{
			ContractID: fmt.Sprintf("KXU3-26SEP-T%d", i),
			Personas:   []string{"gemma-bear"}, NetPnLUSD: -20, NotionalUSD: 100, Outcome: shadowSettled,
		})
	}
	c := counts(map[string][2]int{"gemma-bear": {210, 0}})

	// Volume and edge alone: the pre-2026-09-03 bar, which this book clears.
	if sc := buildShadowScorecard(rows, 210, 0, c, 200, 0); !sc.BarMet {
		t.Fatalf("precondition: the count-and-edge bar should pass this book (%+.2f%%)", sc.ExpectancyPct)
	}

	sc := buildShadowScorecard(rows, 210, 0, c, 200, 50)
	if sc.ExpectancyPct <= 0 {
		t.Fatalf("precondition: the book should look profitable overall, got %+.2f%%", sc.ExpectancyPct)
	}
	if sc.BarMet {
		t.Errorf("a book carried entirely by one series must not meet the bar: %s", sc.Verdict)
	}
	if sc.TopFamily != "KXBRENTW" {
		t.Errorf("top family = %q, want KXBRENTW", sc.TopFamily)
	}
	if sc.ExpectancyExTop >= 0 {
		t.Errorf("expectancy ex-top = %+.2f%%, want negative — that is the whole finding", sc.ExpectancyExTop)
	}
	if sc.Contracts != 65 { // 5 Brent strikes + 60 KXU3
		t.Errorf("distinct contracts = %d, want 65", sc.Contracts)
	}
	if !strings.Contains(sc.Verdict, "KXBRENTW") {
		t.Errorf("the verdict should name the series carrying the book: %q", sc.Verdict)
	}
	if len(sc.PerFamily) != 2 || sc.PerFamily[0].Family != "KXBRENTW" {
		t.Errorf("per-family should lead with the biggest mover, got %+v", sc.PerFamily)
	}
	if sc.PerFamily[0].Contracts != 5 {
		t.Errorf("KXBRENTW contracts = %d, want 5 — %d proposals on 5 contracts is the point",
			sc.PerFamily[0].Contracts, sc.PerFamily[0].Scored)
	}
}

// Breadth is its own clause: a broadly profitable book still fails if the evidence is
// piled onto too few contracts, even when no single series dominates.
func TestBuildShadowScorecard_BreadthClause(t *testing.T) {
	build := func(distinct int) ShadowScorecard {
		var rows []ShadowScore
		for i := 0; i < 200; i++ {
			rows = append(rows, ShadowScore{
				ContractID: fmt.Sprintf("KXSER%d-26SEP-T1", i%distinct),
				Personas:   []string{"gemma-bull"}, NetPnLUSD: 5, NotionalUSD: 100, Outcome: shadowSettled,
			})
		}
		return buildShadowScorecard(rows, 200, 0, counts(map[string][2]int{"gemma-bull": {200, 0}}), 200, 50)
	}
	if sc := build(10); sc.BarMet {
		t.Errorf("200 proposals over 10 contracts must fail the breadth clause: %s", sc.Verdict)
	} else if !strings.Contains(sc.Verdict, "distinct contracts") {
		t.Errorf("the verdict should explain the breadth failure: %q", sc.Verdict)
	}
	if sc := build(80); !sc.BarMet {
		t.Errorf("200 proposals over 80 profitable contracts should meet the bar: %s", sc.Verdict)
	}
}

// MinContracts=0 must reproduce the old behaviour exactly, so an operator can put the
// bar back where it was without editing code.
func TestBuildShadowScorecard_BreadthDisabled(t *testing.T) {
	var rows []ShadowScore
	for i := 0; i < 200; i++ {
		rows = append(rows, ShadowScore{
			ContractID: "KXONLY-26SEP-T1", Personas: []string{"gemma-bull"},
			NetPnLUSD: 5, NotionalUSD: 100, Outcome: shadowSettled,
		})
	}
	sc := buildShadowScorecard(rows, 200, 0, counts(map[string][2]int{"gemma-bull": {200, 0}}), 200, 0)
	if !sc.BarMet {
		t.Errorf("SHADOW_BAR_MIN_CONTRACTS=0 should restore the count-and-edge bar: %s", sc.Verdict)
	}
	// A single-family book has nothing to strike out, so the robustness clause abstains
	// and breadth is what would have caught it.
	if sc.TopFamily != "" || sc.ExpectancyExTop != 0 {
		t.Errorf("a one-family book should leave the ex-top figures unset, got %q / %+.2f", sc.TopFamily, sc.ExpectancyExTop)
	}
}
