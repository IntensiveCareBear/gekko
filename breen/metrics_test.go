package main

import (
	"math"
	"testing"
)

func approx(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.01 {
		t.Errorf("%s = %.4f, want %.4f", label, got, want)
	}
}

func TestComputeRiskMetrics(t *testing.T) {
	// 5 trades, all $100 capital: +10, -5, +20, -8, +6.
	pnl := []float64{10, -5, 20, -8, 6}
	cost := []float64{100, 100, 100, 100, 100}

	m := computeRiskMetrics(pnl, cost, 1000, 5)

	approx(t, "expectancy", m.ExpectancyUSD, 4.6) // 23/5
	if m.ProfitFactor == nil {
		t.Fatal("profit factor should be set (there are losing trades)")
	}
	approx(t, "profit factor", *m.ProfitFactor, 2.77) // 36/13
	// Cumulative curve 10,5,25,17,23 → peak profit 25, deepest trough at 17 → DD 8.
	// Percent is DD over peak equity = capital base 1000 + peak profit 25 = 1025.
	approx(t, "max drawdown $", m.MaxDrawdownUSD, 8)
	approx(t, "max drawdown %", m.MaxDrawdownPct, 0.78) // 8/1025*100
	if m.Sharpe == nil || m.Sortino == nil {
		t.Fatal("Sharpe/Sortino should be set at exactly minSamples")
	}
	approx(t, "sharpe", *m.Sharpe, 0.40)
	approx(t, "sortino", *m.Sortino, 1.09)
	if m.Samples != 5 {
		t.Errorf("samples = %d, want 5", m.Samples)
	}
}

func TestComputeRiskMetrics_GatesBelowMinSamples(t *testing.T) {
	pnl := []float64{10, -5, 20, -8, 6}
	cost := []float64{100, 100, 100, 100, 100}

	m := computeRiskMetrics(pnl, cost, 1000, 6) // one more than we have

	if m.Sharpe != nil || m.Sortino != nil {
		t.Error("Sharpe/Sortino must be nil below minSamples")
	}
	// Expectancy, profit factor and drawdown are still valid at low n.
	approx(t, "expectancy", m.ExpectancyUSD, 4.6)
	if m.ProfitFactor == nil {
		t.Error("profit factor should still be reported below minSamples")
	}
	approx(t, "max drawdown $", m.MaxDrawdownUSD, 8)
}

func TestComputeRiskMetrics_NoLosses(t *testing.T) {
	m := computeRiskMetrics([]float64{5, 10, 3}, []float64{50, 50, 50}, 1000, 3)
	if m.ProfitFactor != nil {
		t.Error("profit factor must be nil when there are no losing trades")
	}
	if m.MaxDrawdownUSD != 0 {
		t.Errorf("a monotonically rising curve has no drawdown, got $%.2f", m.MaxDrawdownUSD)
	}
}

// TestComputeRiskMetrics_TinyPeakBigLoss is the regression for the "-150,000%"
// drawdown bug: a persona that poked barely above breakeven ($0.10) before a large
// loss ($150) used to divide by that $0.10 peak profit. Anchoring the % to peak
// equity (capital base + peak profit) keeps it sane.
func TestComputeRiskMetrics_TinyPeakBigLoss(t *testing.T) {
	pnl := []float64{0.10, -150.10}   // up a sliver, then a big loss
	cost := []float64{100, 100}
	m := computeRiskMetrics(pnl, cost, 1000, 0)

	approx(t, "max drawdown $", m.MaxDrawdownUSD, 150.10) // 0.10 → -150.00, drop of 150.10
	// Old (buggy) denominator was peak profit 0.10 → 150,100%. New base is peak
	// equity 1000 + 0.10 = 1000.10 → ~15%.
	approx(t, "max drawdown %", m.MaxDrawdownPct, 15.01)
	if m.MaxDrawdownPct > 100 {
		t.Fatalf("drawdown %% must be bounded by peak equity, got %.2f%%", m.MaxDrawdownPct)
	}
}

// TestComputeRiskMetrics_UnknownBase verifies a 0 capital base suppresses the %
// (reports 0, the "unknown" sentinel) rather than dividing by peak profit — so an
// unknown base can never reintroduce the blowup.
func TestComputeRiskMetrics_UnknownBase(t *testing.T) {
	m := computeRiskMetrics([]float64{0.10, -150.10}, []float64{100, 100}, 0, 0)
	if m.MaxDrawdownPct != 0 {
		t.Errorf("unknown base should suppress drawdown %%, got %.2f", m.MaxDrawdownPct)
	}
	// The dollar drawdown is always correct regardless of base.
	approx(t, "max drawdown $", m.MaxDrawdownUSD, 150.10)
}

func TestComputeBookMetrics_EntryBands(t *testing.T) {
	trades := []ClosedTrade{
		{EntryPrice: 0.10, Size: 100, RealizedPnL: 30, ClosedAtMs: 1},  // <0.20, win
		{EntryPrice: 0.15, Size: 100, RealizedPnL: -10, ClosedAtMs: 2}, // <0.20, loss
		{EntryPrice: 0.55, Size: 100, RealizedPnL: -40, ClosedAtMs: 3}, // 0.50–0.65, loss
		{EntryPrice: 0.60, Size: 100, RealizedPnL: -25, ClosedAtMs: 4}, // 0.50–0.65, loss
		{EntryPrice: 0.90, Size: 100, RealizedPnL: 50, ClosedAtMs: 5},  // ≥0.80, win
	}

	bm := computeBookMetrics(trades, 100000, 20) // above sample gate → Sharpe/Sortino nil

	if bm.Trades != 5 || bm.Wins != 2 || bm.Losses != 3 {
		t.Fatalf("book totals: trades=%d wins=%d losses=%d, want 5/2/3", bm.Trades, bm.Wins, bm.Losses)
	}
	approx(t, "book win rate", bm.WinRate, 40)

	byLabel := map[string]EntryBandStat{}
	for _, b := range bm.EntryBands {
		byLabel[b.Label] = b
	}
	if b := byLabel["<0.20"]; b.Trades != 2 || b.WinRate != 50 {
		t.Errorf("<0.20 band: trades=%d winRate=%.1f, want 2/50", b.Trades, b.WinRate)
	}
	// The historically money-losing band should surface as 0% win, negative P&L.
	if b := byLabel["0.50–0.65"]; b.Trades != 2 || b.WinRate != 0 || b.RealizedPnL != -65 {
		t.Errorf("0.50–0.65 band: trades=%d winRate=%.1f pnl=%.1f, want 2/0/-65", b.Trades, b.WinRate, b.RealizedPnL)
	}
	if bm.Sharpe != nil {
		t.Error("Sharpe should be gated (only 5 trades, minSamples 20)")
	}
}
