package main

import (
	"context"
	"math"
	"testing"
)

// curve builds a one-persona calibration table with explicit bucket records, so the
// rescaling logic is tested against exactly the shape intended rather than whatever
// computeCalibration happens to produce.
func curve(persona string, overall float64, bins ...CalibrationBin) CalibrationTable {
	return CalibrationTable{
		Bins:     len(bins),
		Personas: []PersonaCalibration{{Persona: persona, Trades: 100, WinRate: overall, Bins: bins}},
	}
}

func bin(lo, hi, adjusted float64, trades int) CalibrationBin {
	return CalibrationBin{Lo: lo, Hi: hi, Midpoint: (lo + hi) / 2, Trades: trades, Adjusted: adjusted}
}

func TestConfidenceCalibrator_NilIsStated(t *testing.T) {
	var cal confidenceCalibrator
	if got := cal.apply("anyone", 0.83); got != 0.83 {
		t.Errorf("a nil calibrator must pass the stated value through, got %.3f", got)
	}
}

func TestCalibratedConfidence_RescalesByDiscrimination(t *testing.T) {
	// A persona winning 40% overall, whose top bucket delivers only 20% — it claims
	// its best and delivers half its own average.
	table := curve("gemma-bull", 40,
		bin(0.0, 0.2, 40, 50), bin(0.2, 0.4, 40, 50), bin(0.4, 0.6, 40, 50),
		bin(0.6, 0.8, 40, 50), bin(0.8, 1.0, 20, 50))

	got := calibratedConfidence(table, "gemma-bull", 0.90, 20, 0.5, 1.5)
	if math.Abs(got-0.45) > 1e-9 {
		t.Errorf("a bucket delivering half the persona's average should halve 0.90 → 0.45, got %.3f", got)
	}

	// A bucket that delivers exactly the persona's average is perfectly ordinary and
	// must pass through untouched — this is what keeps the gates meaning what they meant.
	if got := calibratedConfidence(table, "gemma-bull", 0.50, 20, 0.5, 1.5); math.Abs(got-0.50) > 1e-9 {
		t.Errorf("an average-performing bucket must not move the confidence, got %.3f", got)
	}
}

// The regression that matters most: calibration must not quietly convert conviction
// into win rate. Realized win rates run 30–50% here while MID_BAND_MIN_CONFIDENCE is
// 0.70 — substituting one for the other would reject every mid-band entry and flatline
// the funnel for the third time (2026-07-08, 2026-07-12).
func TestCalibratedConfidence_DoesNotCollapseToWinRate(t *testing.T) {
	// A well-behaved persona: 38% overall, every bucket performing at its average.
	table := curve("cautious-bull", 38,
		bin(0.0, 0.2, 38, 40), bin(0.2, 0.4, 38, 40), bin(0.4, 0.6, 38, 40),
		bin(0.6, 0.8, 38, 40), bin(0.8, 1.0, 38, 40))

	const midBandGate = 0.70
	for _, stated := range []float64{0.72, 0.80, 0.95} {
		got := calibratedConfidence(table, "cautious-bull", stated, 20, 0.5, 1.5)
		if got < midBandGate {
			t.Errorf("stated %.2f calibrated to %.3f — below the %.2f mid-band gate purely because "+
				"the persona's win rate is under it; calibration must rescale, not substitute",
				stated, got, midBandGate)
		}
	}
}

func TestCalibratedConfidence_ClampsAndFallsBack(t *testing.T) {
	cases := []struct {
		name       string
		table      CalibrationTable
		persona    string
		stated     float64
		minSamples int
		want       float64
	}{
		{
			// No curve for this persona: explore, don't guess.
			name:    "unknown persona falls back to stated",
			table:   curve("someone-else", 40, bin(0.8, 1.0, 10, 50)),
			persona: "gemma-bull", stated: 0.90, minSamples: 20, want: 0.90,
		},
		{
			// The bucket is real but too thin to believe — same philosophy as the
			// allocator's minTrades gate.
			name:    "thin bucket falls back to stated",
			table:   curve("gemma-bull", 40, bin(0.0, 0.5, 40, 50), bin(0.5, 1.0, 5, 3)),
			persona: "gemma-bull", stated: 0.90, minSamples: 20, want: 0.90,
		},
		{
			// A persona that has never won has no baseline to form a ratio against.
			// Cutting it further is the ALLOCATOR's job (level); calibration only
			// reads the shape of the curve.
			name:    "no wins at all falls back to stated",
			table:   curve("hopeless", 0, bin(0.0, 0.5, 0, 50), bin(0.5, 1.0, 0, 50)),
			persona: "hopeless", stated: 0.80, minSamples: 20, want: 0.80,
		},
		{
			// Factor would be 10/40 = 0.25; the clamp holds it at 0.5.
			name:    "extreme underperformance clamps at factorMin",
			table:   curve("gemma-bull", 40, bin(0.0, 0.5, 40, 50), bin(0.5, 1.0, 10, 50)),
			persona: "gemma-bull", stated: 0.80, minSamples: 20, want: 0.40,
		},
		{
			// Factor would be 90/30 = 3.0; the clamp holds it at 1.5.
			name:    "extreme outperformance clamps at factorMax",
			table:   curve("sharp", 30, bin(0.0, 0.5, 30, 50), bin(0.5, 1.0, 90, 50)),
			persona: "sharp", stated: 0.60, minSamples: 20, want: 0.90,
		},
		{
			// 0.90 × 1.5 = 1.35 → confidence is a probability-like scale, clamp to 1.
			name:    "result clamps to 1.0",
			table:   curve("sharp", 30, bin(0.0, 0.5, 30, 50), bin(0.5, 1.0, 90, 50)),
			persona: "sharp", stated: 0.90, minSamples: 20, want: 1.0,
		},
	}
	for _, c := range cases {
		got := calibratedConfidence(c.table, c.persona, c.stated, c.minSamples, 0.5, 1.5)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %.3f, want %.3f", c.name, got, c.want)
		}
	}
}

// The vote is where calibration is supposed to bite: a loud persona with a bad record
// at that volume should stop out-voting a quieter one that delivers.
func TestWeightedConviction_OverconfidentPersonaLosesTheVote(t *testing.T) {
	table := CalibrationTable{Bins: 5, Personas: []PersonaCalibration{
		// Claims 0.9, delivers half its own average there.
		{Persona: "loud", Trades: 100, WinRate: 40, Bins: []CalibrationBin{
			bin(0.0, 0.2, 40, 50), bin(0.2, 0.4, 40, 50), bin(0.4, 0.6, 40, 50),
			bin(0.6, 0.8, 40, 50), bin(0.8, 1.0, 20, 50)}},
		// Claims 0.7, delivers better than its average there.
		{Persona: "quiet", Trades: 100, WinRate: 40, Bins: []CalibrationBin{
			bin(0.0, 0.2, 40, 50), bin(0.2, 0.4, 40, 50), bin(0.4, 0.6, 40, 50),
			bin(0.6, 0.8, 56, 50), bin(0.8, 1.0, 40, 50)}},
	}}
	cal := confidenceCalibrator(func(persona string, stated float64) float64 {
		return calibratedConfidence(table, persona, stated, 20, 0.5, 1.5)
	})

	loud := []StrategyProposal{{ContractID: "A", Persona: "loud", Confidence: 0.90}}
	quiet := []StrategyProposal{{ContractID: "B", Persona: "quiet", Confidence: 0.70}}
	weights := map[string]float64{"loud": 1.0, "quiet": 1.0}

	// Uncalibrated, the louder claim wins on volume alone.
	if weightedConviction(loud, weights, nil) <= weightedConviction(quiet, weights, nil) {
		t.Fatal("precondition: stated 0.90 should out-vote stated 0.70 without calibration")
	}
	// Calibrated: 0.90×0.5 = 0.45 vs 0.70×1.4 = 0.98.
	if weightedConviction(loud, weights, cal) >= weightedConviction(quiet, weights, cal) {
		t.Errorf("calibrated, the persona that delivers should out-vote the one that claims: loud=%.3f quiet=%.3f",
			weightedConviction(loud, weights, cal), weightedConviction(quiet, weights, cal))
	}

	// And it decides which contract the event actually trades.
	both := append(append([]StrategyProposal{}, loud...), quiet...)
	if winner, _, _ := pickWinningContract(both, weights, cal); winner != "B" {
		t.Errorf("winning contract = %q, want B (the calibrated persona's pick)", winner)
	}
}

// Calibration must never write back into the record it learns from. ContributorProposal
// .Confidence is what PersonaScore.Confidence — and therefore the calibration table —
// is built from; stamping a calibrated value there would make the table calibrate its
// own output and drift a little further every run.
func TestConsolidateProposals_ContributorConfidenceStaysRaw(t *testing.T) {
	halve := confidenceCalibrator(func(_ string, stated float64) float64 { return stated * 0.5 })
	props := []StrategyProposal{
		{ContractID: "A", Action: "BUY", AveragePrice: 0.50, TotalSize: 100, Persona: "gemma-bull", Confidence: 0.80},
	}

	out, _, ok := consolidateProposals(props, map[string]float64{}, 0, halve)
	if !ok {
		t.Fatal("expected consolidation to succeed")
	}
	if len(out.ContributorProposals) != 1 {
		t.Fatalf("want 1 contributor proposal, got %d", len(out.ContributorProposals))
	}
	if got := out.ContributorProposals[0].Confidence; got != 0.80 {
		t.Errorf("recorded confidence = %.3f, want the RAW 0.80 — the calibration table is "+
			"built from this field and must not be fed its own output", got)
	}
}

func TestMidBandConfidence_JudgesCalibratedContributors(t *testing.T) {
	// "loud" gets halved, "quiet" passes through.
	cal := confidenceCalibrator(func(persona string, stated float64) float64 {
		if persona == "loud" {
			return stated * 0.5
		}
		return stated
	})
	p := &StrategyProposal{
		Persona:    "consensus",
		Confidence: 0.55,
		ContributorProposals: []ContributorProposal{
			{Persona: "loud", Confidence: 0.95},  // calibrates to 0.475
			{Persona: "quiet", Confidence: 0.72}, // stays 0.72
		},
	}

	if got := midBandConfidence(p, nil); got != 0.95 {
		t.Errorf("uncalibrated gate reads the loudest raw claim 0.95, got %.3f", got)
	}
	// Calibrating each contributor and THEN taking the max is the point: the loud
	// persona no longer carries the gate on a claim its record doesn't support.
	if got := midBandConfidence(p, cal); math.Abs(got-0.72) > 1e-9 {
		t.Errorf("calibrated gate = %.3f, want 0.72 (the quiet persona's honest claim)", got)
	}
}

// Wiring: the tracker hands out a nil calibrator while disabled, so every decision path
// keeps its pre-Phase-1 behavior until an operator turns it on.
func TestTrackerCalibrator_NilUntilEnabled(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	if tr.calibrator() != nil {
		t.Error("calibration is off by default — calibrator() must be nil")
	}

	tr.calibrationEnabled = true
	tr.calibrationMinSamples = 1
	tr.calibrationFactorMin, tr.calibrationFactorMax = 0.5, 1.5
	tr.calibrationCache.Store(curve("gemma-bull", 40,
		bin(0.0, 0.2, 40, 50), bin(0.2, 0.4, 40, 50), bin(0.4, 0.6, 40, 50),
		bin(0.6, 0.8, 40, 50), bin(0.8, 1.0, 20, 50)))

	cal := tr.calibrator()
	if cal == nil {
		t.Fatal("enabled tracker must hand out a calibrator")
	}
	if got := cal.apply("gemma-bull", 0.90); math.Abs(got-0.45) > 1e-9 {
		t.Errorf("calibrator = %.3f, want 0.45", got)
	}
	if got := cal.apply("never-seen", 0.90); got != 0.90 {
		t.Errorf("unknown persona = %.3f, want the stated 0.90", got)
	}
}

// refreshCalibration must populate the cache without Redis: from Phase 1 the table
// feeds decisions, so an offline/keyless run cannot be left reading an empty one.
func TestRefreshCalibration_PopulatesCacheOffline(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{}) // redisAddr "" → offline
	tr.metricsMinSamples = 1

	tr.mu.Lock()
	for i := 0; i < 6; i++ {
		pnl := -10.0
		if i < 3 {
			pnl = 10
		}
		tr.closedTrades = append(tr.closedTrades, ClosedTrade{
			ContractID:    "KXINXY-26DEC31",
			PersonaScores: []PersonaScore{{Persona: "gemma-bull", RealizedPnL: pnl, Confidence: 0.90}},
		})
	}
	tr.mu.Unlock()

	tr.syncAccountToRedis(context.Background()) // no Redis client: must still refresh the cache

	table := tr.calibrationSnapshot()
	if len(table.Personas) != 1 || table.Personas[0].Persona != "gemma-bull" {
		t.Fatalf("cache not populated offline: %+v", table.Personas)
	}
	if table.UpdatedAtMs == 0 {
		t.Error("refreshed table should carry an update stamp")
	}
}
