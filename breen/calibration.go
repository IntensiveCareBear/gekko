package main

import (
	"math"
	"sort"
	"time"
)

// Confidence calibration (breen-intelligence Phase 0).
//
// `Confidence` gates the mid-band and weights consensus, but nobody has ever checked
// whether a persona's 0.8 actually wins more often than its 0.5. LLM-stated confidence
// is notoriously miscalibrated, and the mid-band gate is only as good as the number it
// reads. This file bins each persona's stated confidence against its realized win rate
// so the miscalibration becomes visible.
//
// Phase 0 is DISPLAY-ONLY: nothing here changes a trading decision. The table exists so
// that, after a run, we can answer "is the fleet miscalibrated enough to be worth
// correcting?" with evidence instead of assumption — Phase 1 (calibratedConfidence
// feeding consensus and the mid-band gate) is only worth building if the bins come out
// non-flat.

const (
	defaultCalibrationBins = 5
	// Bounds on the calibration factor, mirroring personaWeights' [0.5, 1.5] clamp:
	// one bucket's record can at most halve or half-again a persona's stated
	// conviction, however extreme the sample looks.
	defaultCalibrationFactorMin = 0.5
	defaultCalibrationFactorMax = 1.5
	// calibrationN0 is the shrinkage weight: how many trades a bin needs before its own
	// win rate meaningfully outweighs the persona's overall rate. Same trick and same
	// constant as personaWeights, so a 2-trade bin at 100% doesn't read as a signal.
	calibrationN0 = 5.0
)

// CalibrationBin is one persona's realized outcome inside one stated-confidence bucket.
type CalibrationBin struct {
	Lo       float64 `json:"lo"`       // bucket lower edge (inclusive)
	Hi       float64 `json:"hi"`       // bucket upper edge (exclusive; the top bucket includes 1.0)
	Midpoint float64 `json:"midpoint"` // stated confidence at the bucket centre — what the persona claimed
	Trades   int     `json:"trades"`
	Wins     int     `json:"wins"`
	WinRate  float64 `json:"win_rate"` // raw realized win rate in the bucket, percent
	Adjusted float64 `json:"adjusted"` // WinRate shrunk toward the persona's overall rate, percent
	GapPct   float64 `json:"gap_pct"`  // Adjusted − Midpoint×100: negative = over-confident
	Thin     bool    `json:"thin"`     // fewer than minSamples trades — shrinkage is carrying the number
}

// PersonaCalibration is one persona's calibration curve plus the overall rate its thin
// bins are shrunk toward.
type PersonaCalibration struct {
	Persona string           `json:"persona"`
	Trades  int              `json:"trades"`   // scored trades carrying a stated confidence
	WinRate float64          `json:"win_rate"` // overall realized win rate, percent
	Bins    []CalibrationBin `json:"bins"`
}

// CalibrationTable is the published table: one curve per persona, newest-first by
// sample count so the personas with something to say sort to the top.
type CalibrationTable struct {
	Bins        int                  `json:"bins"`
	MinSamples  int                  `json:"min_samples"`
	Enabled     bool                 `json:"enabled"` // false = display-only; true = consensus and the mid-band gate reason on it
	Personas    []PersonaCalibration `json:"personas"`
	UpdatedAtMs int64                `json:"updated_at_ms"`
}

// confidenceCalibrator maps (persona, stated confidence) to the confidence a decision
// should actually be made on. A nil calibrator means "use the stated value" — that is
// the disabled default, so every call site works unchanged when CALIBRATION_ENABLED is
// off.
type confidenceCalibrator func(persona string, stated float64) float64

// apply is the nil-safe call: nil ⇒ stated, unchanged.
func (f confidenceCalibrator) apply(persona string, stated float64) float64 {
	if f == nil {
		return stated
	}
	return f(persona, stated)
}

// calibratedConfidence rescales a persona's stated confidence by how well that
// confidence level has actually DISCRIMINATED for that persona — the ratio of the
// bucket's realized win rate to the persona's own overall win rate.
//
// The ratio form is deliberate, and it is the one real design decision in Phase 1.
// The obvious implementation — replace stated confidence with the bucket's realized
// win rate — is wrong here, and dangerously so: realized win rates in this book run
// 30–50% (asymmetric payoffs make that fine), while MID_BAND_MIN_CONFIDENCE is 0.70.
// Substituting win rate for conviction would push essentially every entry below that
// gate and flatline the funnel — which has already happened twice for adjacent reasons
// (2026-07-08, 2026-07-12). Scaling by the ratio keeps confidence on its original
// scale, so the gates keep meaning what they were tuned to mean, while a persona that
// cries 0.9 and delivers half its own average still gets cut in half.
//
// This also draws a clean line against the allocator, which is the other thing that
// "learns who to trust":
//
//   - personaWeights / AdaptiveAllocator answer "is this persona any good?" (LEVEL)
//   - calibration answers "does this persona's own confidence mean anything?" (SHAPE)
//
// So a persona that never wins is not cut further here — its overall rate is the
// denominator, and cutting it is the allocator's job. Calibration only reads the shape
// of the curve.
//
// Falls back to the stated confidence — explore, same philosophy as the allocator's
// minTrades gate — when the persona has no curve, when its bucket is thinner than
// minSamples, or when it has no wins at all to form a baseline from. Pure.
func calibratedConfidence(table CalibrationTable, persona string, stated float64, minSamples int, factorMin, factorMax float64) float64 {
	var curve *PersonaCalibration
	for i := range table.Personas {
		if table.Personas[i].Persona == persona {
			curve = &table.Personas[i]
			break
		}
	}
	if curve == nil || curve.WinRate <= 0 || len(curve.Bins) == 0 {
		return stated
	}
	idx := calibrationBinIndex(stated, len(curve.Bins))
	if idx < 0 {
		return stated
	}
	// An empty bucket is no evidence, whatever minSamples says: without this floor a
	// CALIBRATION_MIN_SAMPLES of 0 would read a 0-trade bucket's zeroed record as
	// "never wins" and cut conviction to the clamp on no data at all.
	if minSamples < 1 {
		minSamples = 1
	}
	bin := curve.Bins[idx]
	if bin.Trades < minSamples {
		return stated
	}
	factor := bin.Adjusted / curve.WinRate
	factor = math.Max(factorMin, math.Min(factorMax, factor))
	return math.Max(0, math.Min(1, stated*factor))
}

// calibrationBinIndex maps a stated confidence in [0,1] to its bucket. Out-of-range
// values clamp rather than being dropped — a persona stamping 1.2 is still making a
// claim, and silently discarding it would bias the table toward the well-behaved.
func calibrationBinIndex(conf float64, bins int) int {
	if bins <= 0 {
		return -1
	}
	idx := int(conf * float64(bins))
	if idx < 0 {
		idx = 0
	}
	if idx >= bins {
		idx = bins - 1
	}
	return idx
}

// refreshCalibration recomputes the calibration table from the current closed-trade
// history and caches it. Called on every Redis sync — which every close path triggers —
// so the table a decision reads is never more than one settled trade stale. Kept off
// the hot path deliberately: riskGate runs on every proposal and must not walk the
// whole trade history to do it.
func (pt *PositionTracker) refreshCalibration() CalibrationTable {
	pt.mu.RLock()
	trades := make([]ClosedTrade, len(pt.closedTrades))
	copy(trades, pt.closedTrades)
	bins, minSamples := pt.calibrationBins, pt.metricsMinSamples
	enabled := pt.calibrationEnabled
	pt.mu.RUnlock()

	table := computeCalibration(trades, bins, minSamples)
	table.UpdatedAtMs = time.Now().UnixMilli()
	// Stamped so the UI can say whether these numbers are merely being watched or are
	// actually steering the vote — the same table means different things either way.
	table.Enabled = enabled
	pt.calibrationCache.Store(table)
	return table
}

// calibrationSnapshot returns the cached table (zero value before the first refresh).
func (pt *PositionTracker) calibrationSnapshot() CalibrationTable {
	if v, ok := pt.calibrationCache.Load().(CalibrationTable); ok {
		return v
	}
	return CalibrationTable{}
}

// calibrator returns the confidence calibrator the decision paths should use, or nil
// when calibration is disabled — callers treat nil as "reason on stated confidence",
// which is exactly the pre-Phase-1 behavior. The table is captured once here rather
// than per lookup so one consensus flush judges every persona against the same table.
func (pt *PositionTracker) calibrator() confidenceCalibrator {
	pt.mu.RLock()
	enabled := pt.calibrationEnabled
	minSamples := pt.calibrationMinSamples
	lo, hi := pt.calibrationFactorMin, pt.calibrationFactorMax
	pt.mu.RUnlock()
	if !enabled {
		return nil
	}
	table := pt.calibrationSnapshot()
	return func(persona string, stated float64) float64 {
		return calibratedConfidence(table, persona, stated, minSamples, lo, hi)
	}
}

// computeCalibration bins every persona's stated confidence against whether the
// resulting trade actually won, shrinking each bin toward the persona's overall win
// rate so thin buckets read as "no information" instead of as 0% or 100%.
//
// A "win" is that persona's OWN scored P&L on the trade (PersonaScore.RealizedPnL > 0),
// not the trade-level result — on a consensus order each contributor is scored against
// its own proposed entry, so personas differentiate. Scores with no stated confidence
// (legacy trades, proposals that carried none) are skipped: absence of a claim is not a
// claim of zero. Pure.
func computeCalibration(trades []ClosedTrade, bins, minSamples int) CalibrationTable {
	if bins <= 0 {
		bins = defaultCalibrationBins
	}
	type acc struct {
		n, wins int
		binN    []int
		binWins []int
	}
	by := map[string]*acc{}
	for _, t := range trades {
		for _, s := range t.PersonaScores {
			if s.Confidence <= 0 {
				continue
			}
			a := by[s.Persona]
			if a == nil {
				a = &acc{binN: make([]int, bins), binWins: make([]int, bins)}
				by[s.Persona] = a
			}
			idx := calibrationBinIndex(s.Confidence, bins)
			won := s.RealizedPnL > 0
			a.n++
			a.binN[idx]++
			if won {
				a.wins++
				a.binWins[idx]++
			}
		}
	}

	width := 1.0 / float64(bins)
	out := make([]PersonaCalibration, 0, len(by))
	for name, a := range by {
		overall := 0.0
		if a.n > 0 {
			overall = float64(a.wins) / float64(a.n)
		}
		pc := PersonaCalibration{
			Persona: name,
			Trades:  a.n,
			WinRate: round2(overall * 100),
			Bins:    make([]CalibrationBin, bins),
		}
		for i := 0; i < bins; i++ {
			lo, hi := float64(i)*width, float64(i+1)*width
			b := CalibrationBin{
				Lo:       round2(lo),
				Hi:       round2(hi),
				Midpoint: math.Round((lo+hi)/2*1000) / 1000,
				Trades:   a.binN[i],
				Wins:     a.binWins[i],
				Thin:     a.binN[i] < minSamples,
			}
			if b.Trades > 0 {
				raw := float64(b.Wins) / float64(b.Trades)
				w := float64(b.Trades) / (float64(b.Trades) + calibrationN0)
				adjusted := raw*w + overall*(1-w)
				b.WinRate = round2(raw * 100)
				b.Adjusted = round2(adjusted * 100)
				b.GapPct = round2(adjusted*100 - b.Midpoint*100)
			}
			pc.Bins[i] = b
		}
		out = append(out, pc)
	}
	// Most-sampled first, name as the tiebreak so the table doesn't reshuffle between
	// syncs when two personas are level.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Trades != out[j].Trades {
			return out[i].Trades > out[j].Trades
		}
		return out[i].Persona < out[j].Persona
	})
	return CalibrationTable{Bins: bins, MinSamples: minSamples, Personas: out}
}
