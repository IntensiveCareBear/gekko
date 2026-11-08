package main

import (
	"testing"
	"time"
)

// trade builds a closed trade carrying one persona score at a stated confidence.
// pnl's sign is what makes it a win, matching computeCalibration's definition.
func calTrade(persona string, conf, pnl float64) ClosedTrade {
	return ClosedTrade{
		ContractID: "KXINXY-26DEC31",
		Size:       100,
		EntryPrice: 0.50,
		PersonaScores: []PersonaScore{
			{Persona: persona, RealizedPnL: pnl, Cost: 50, Confidence: conf},
		},
	}
}

func TestCalibrationBinIndex(t *testing.T) {
	cases := []struct {
		conf float64
		bins int
		want int
	}{
		{0.0, 5, 0},
		{0.19, 5, 0},
		{0.20, 5, 1},
		{0.55, 5, 2},
		{0.99, 5, 4},
		// The top edge belongs to the top bucket, not a sixth one.
		{1.0, 5, 4},
		// Out-of-range claims clamp rather than vanish: a persona stamping 1.2 is
		// still making a claim, and dropping it would bias the table toward the
		// well-behaved.
		{1.4, 5, 4},
		{-0.3, 5, 0},
	}
	for _, c := range cases {
		if got := calibrationBinIndex(c.conf, c.bins); got != c.want {
			t.Errorf("calibrationBinIndex(%.2f, %d) = %d, want %d", c.conf, c.bins, got, c.want)
		}
	}
}

// The headline case: a persona whose 0.9 claims win no more often than its 0.5 ones.
// This is the shape Phase 1 exists to correct — and the shape whose absence means
// Phase 1 isn't worth building.
func TestComputeCalibration_ExposesOverconfidence(t *testing.T) {
	var trades []ClosedTrade
	// 20 high-confidence calls at 0.90, only 5 of which win (25%).
	for i := 0; i < 20; i++ {
		pnl := -10.0
		if i < 5 {
			pnl = 10
		}
		trades = append(trades, calTrade("gemma-bull", 0.90, pnl))
	}
	// 20 mid-confidence calls at 0.50, 10 of which win (50%).
	for i := 0; i < 20; i++ {
		pnl := -10.0
		if i < 10 {
			pnl = 10
		}
		trades = append(trades, calTrade("gemma-bull", 0.50, pnl))
	}

	table := computeCalibration(trades, 5, 20)
	if len(table.Personas) != 1 {
		t.Fatalf("want 1 persona curve, got %d", len(table.Personas))
	}
	pc := table.Personas[0]
	if pc.Trades != 40 {
		t.Errorf("Trades = %d, want 40", pc.Trades)
	}
	if pc.WinRate != 37.5 {
		t.Errorf("overall WinRate = %.2f, want 37.5", pc.WinRate)
	}

	top := pc.Bins[4] // [0.8, 1.0) — the 0.90 claims
	mid := pc.Bins[2] // [0.4, 0.6) — the 0.50 claims
	if top.Trades != 20 || mid.Trades != 20 {
		t.Fatalf("bin populations = top %d, mid %d, want 20 each", top.Trades, mid.Trades)
	}
	if top.WinRate != 25 || mid.WinRate != 50 {
		t.Errorf("raw win rates = top %.1f%%, mid %.1f%%, want 25%% and 50%%", top.WinRate, mid.WinRate)
	}
	// Confidence and outcome run in opposite directions — the miscalibration is real.
	if top.Adjusted >= mid.Adjusted {
		t.Errorf("the 0.9 bucket (%.1f%%) should realize WORSE than the 0.5 bucket (%.1f%%)",
			top.Adjusted, mid.Adjusted)
	}
	// The claim was 0.9; the delivery is ~0.28 after shrinkage → a large negative gap.
	if top.GapPct > -40 {
		t.Errorf("top-bucket GapPct = %.1f, want a large negative gap (claimed 90%%, delivered ~27%%)", top.GapPct)
	}
	if top.Thin || mid.Thin {
		t.Error("20-trade bins should not be flagged thin at minSamples=20")
	}
}

// Shrinkage is what stops a 2-trade bin at 100% from reading as a signal.
func TestComputeCalibration_ShrinksThinBinsTowardOverall(t *testing.T) {
	var trades []ClosedTrade
	// 30 losing calls at 0.30 set the persona's overall rate near zero.
	for i := 0; i < 30; i++ {
		trades = append(trades, calTrade("noisy", 0.30, -10))
	}
	// 2 winning calls at 0.90: raw 100%, but nowhere near enough to believe.
	trades = append(trades, calTrade("noisy", 0.90, 10), calTrade("noisy", 0.90, 10))

	pc := computeCalibration(trades, 5, 20).Personas[0]
	top := pc.Bins[4]
	if top.WinRate != 100 {
		t.Errorf("raw WinRate = %.1f, want the unshrunk 100", top.WinRate)
	}
	if top.Adjusted >= 40 {
		t.Errorf("Adjusted = %.1f, want a 2-trade bin pulled hard toward the %.1f%% overall rate",
			top.Adjusted, pc.WinRate)
	}
	if !top.Thin {
		t.Error("a 2-trade bin must be flagged thin at minSamples=20")
	}
}

// Absence of a stated confidence is not a claim of zero: legacy trades and proposals
// that carried none must not pile into the bottom bucket and invent miscalibration.
func TestComputeCalibration_SkipsScoresWithoutConfidence(t *testing.T) {
	trades := []ClosedTrade{
		calTrade("has-conf", 0.70, 10),
		calTrade("no-conf", 0, -10),
		{ContractID: "legacy", Persona: "legacy-persona", RealizedPnL: -10}, // no PersonaScores at all
	}
	table := computeCalibration(trades, 5, 20)
	if len(table.Personas) != 1 || table.Personas[0].Persona != "has-conf" {
		t.Fatalf("only the persona that actually stated a confidence should appear, got %+v", table.Personas)
	}
	if table.Personas[0].Trades != 1 {
		t.Errorf("Trades = %d, want 1", table.Personas[0].Trades)
	}
}

func TestComputeCalibration_OrdersBySampleCount(t *testing.T) {
	var trades []ClosedTrade
	for i := 0; i < 3; i++ {
		trades = append(trades, calTrade("sparse", 0.60, 10))
	}
	for i := 0; i < 9; i++ {
		trades = append(trades, calTrade("busy", 0.60, 10))
	}
	table := computeCalibration(trades, 5, 20)
	if table.Personas[0].Persona != "busy" {
		t.Errorf("most-sampled persona should sort first, got %q", table.Personas[0].Persona)
	}
	if table.Bins != 5 || table.MinSamples != 20 {
		t.Errorf("table header = %d bins / minSamples %d, want 5 / 20", table.Bins, table.MinSamples)
	}
}

func TestComputeCalibration_EmptyAndDegenerate(t *testing.T) {
	if table := computeCalibration(nil, 5, 20); len(table.Personas) != 0 {
		t.Errorf("no trades should yield no curves, got %d", len(table.Personas))
	}
	// A zero bin count falls back to the default rather than dividing by zero.
	table := computeCalibration([]ClosedTrade{calTrade("p", 0.5, 10)}, 0, 20)
	if table.Bins != defaultCalibrationBins || len(table.Personas[0].Bins) != defaultCalibrationBins {
		t.Errorf("bins = %d, want the %d default", table.Bins, defaultCalibrationBins)
	}
}

// End-to-end: the confidence a persona stamps on its proposal has to survive all the
// way to the closed trade, or there is nothing to calibrate against. It was dropped
// at close before Phase 0.
func TestPersonaScores_CarryConfidenceToClose(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	id := "KXINXY-26DEC31H1600-B5900"
	pos := &Position{
		ContractID: id, EntryPrice: 0.50, Size: 300, Timestamp: time.Now(),
		Persona:      "consensus",
		Contributors: []string{"gemma-bull", "cautious-bull"},
		ContributorProposals: []ContributorProposal{
			{Persona: "gemma-bull", EntryPrice: 0.50, Size: 200, Confidence: 0.90},
			{Persona: "cautious-bull", EntryPrice: 0.50, Size: 100, Confidence: 0.60},
		},
	}
	tr.positions[id] = pos

	tr.mu.Lock()
	tr.recordClosedTradeLocked(pos, 0.60, pos.Size, "EXIT", "TAKE_PROFIT", 0)
	tr.mu.Unlock()

	got := tr.closedTrades[len(tr.closedTrades)-1]
	byPersona := map[string]float64{}
	for _, s := range got.PersonaScores {
		byPersona[s.Persona] = s.Confidence
	}
	if byPersona["gemma-bull"] != 0.90 || byPersona["cautious-bull"] != 0.60 {
		t.Errorf("each persona should carry its OWN stated confidence, got %+v", byPersona)
	}
}

// Two adds from one persona at different convictions score at the size-weighted mean,
// matching how the entry basis itself is averaged.
func TestPersonaScores_ConfidenceIsSizeWeighted(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	id := "KXINXY-26DEC31H1600-B5900"
	pos := &Position{
		ContractID: id, EntryPrice: 0.50, Size: 400, Timestamp: time.Now(),
		Persona: "gemma-bull",
		ContributorProposals: []ContributorProposal{
			{Persona: "gemma-bull", EntryPrice: 0.50, Size: 300, Confidence: 0.80},
			{Persona: "gemma-bull", EntryPrice: 0.50, Size: 100, Confidence: 0.40},
		},
	}
	tr.positions[id] = pos

	tr.mu.Lock()
	tr.recordClosedTradeLocked(pos, 0.55, pos.Size, "EXIT", "DRY_CLOSED", 0)
	tr.mu.Unlock()

	scores := tr.closedTrades[len(tr.closedTrades)-1].PersonaScores
	if len(scores) != 1 {
		t.Fatalf("want 1 aggregated score, got %d", len(scores))
	}
	// (0.80×300 + 0.40×100) / 400 = 0.70
	if scores[0].Confidence != 0.70 {
		t.Errorf("Confidence = %.3f, want the size-weighted 0.70", scores[0].Confidence)
	}
}
