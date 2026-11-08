package main

import (
	"math"
	"sort"
)

// This file computes risk-adjusted performance metrics from closed-trade history.
// Everything here is derived from the ClosedTrade / PersonaScore records the tracker
// already persists (and rebuilds on restart), so no new time-series storage or
// sampling cadence is needed — the numbers reappear after a restart the same way the
// scorecard aggregates do.
//
// A note on Sharpe/Sortino: these are computed on the PER-TRADE return series (each
// trade's realized P&L over the capital it deployed, rf=0), NOT on a periodically
// sampled equity curve, so they are per-trade ratios and are deliberately NOT
// annualized — annualizing a per-trade ratio requires a trades-per-year assumption
// that would manufacture precision we don't have. A time-sampled (annualized) Sharpe
// would need the periodic per-persona NAV series that is intentionally out of scope
// here. Both are gated behind a minimum sample count so a handful of trades can't
// produce a confident-looking ratio.

// RiskMetrics is the risk-adjusted metric block shared by the per-persona scorecard
// and the book-level ("breen") scorecard. Sharpe/Sortino/ProfitFactor are pointers so
// the UI can render "—" when there isn't enough data rather than a misleading 0.
type RiskMetrics struct {
	ExpectancyUSD  float64  `json:"expectancy_usd"`          // mean realized P&L per trade (= winRate·avgWin − lossRate·avgLoss)
	ProfitFactor   *float64 `json:"profit_factor,omitempty"` // gross wins / gross losses; nil when there are no losing trades
	Sharpe         *float64 `json:"sharpe,omitempty"`        // per-trade mean/σ, rf=0, not annualized; nil below MetricsMinSamples
	Sortino        *float64 `json:"sortino,omitempty"`       // per-trade mean/downside-σ, rf=0, not annualized; nil below MetricsMinSamples
	MaxDrawdownUSD float64  `json:"max_drawdown_usd"`        // worst peak-to-trough of the cumulative realized-P&L curve
	MaxDrawdownPct float64  `json:"max_drawdown_pct"`        // same as a % of peak EQUITY (capital base + peak profit); 0 when the base is unknown
	Samples        int      `json:"samples"`                 // number of trades the metrics are computed from
}

// computeRiskMetrics builds a RiskMetrics from time-ordered per-trade P&L and the
// capital deployed on each trade (aligned, same length). Callers must pass the
// slices already sorted by close time so the drawdown curve is chronological.
// capitalBase is the equity the drawdown % is measured against (the run's starting
// capital); pass 0 when unknown to suppress the % rather than divide by peak profit.
func computeRiskMetrics(orderedPnL, orderedCost []float64, capitalBase float64, minSamples int) RiskMetrics {
	n := len(orderedPnL)
	m := RiskMetrics{Samples: n}
	if n == 0 {
		return m
	}

	var sum, grossWin, grossLoss float64
	for _, p := range orderedPnL {
		sum += p
		switch {
		case p > 0:
			grossWin += p
		case p < 0:
			grossLoss += -p
		}
	}
	m.ExpectancyUSD = round2(sum / float64(n))
	if grossLoss > 0 {
		pf := round2(grossWin / grossLoss)
		m.ProfitFactor = &pf
	}

	// Max drawdown of the cumulative realized-P&L curve (starts at 0 at run open).
	var cum, peakProfit, maxDD float64
	for _, p := range orderedPnL {
		cum += p
		if cum > peakProfit {
			peakProfit = cum
		}
		if dd := peakProfit - cum; dd > maxDD {
			maxDD = dd
		}
	}
	m.MaxDrawdownUSD = round2(maxDD)
	// Percent is relative to peak EQUITY (capital base + peak cumulative profit), the
	// standard max-drawdown denominator. Dividing by peak *profit* alone (the old bug)
	// exploded to absurd values — e.g. a persona that poked $0.10 above breakeven then
	// lost $150 reported 150,000%+ — because the denominator was a near-zero sliver.
	if capitalBase > 0 {
		m.MaxDrawdownPct = round2(maxDD / (capitalBase + peakProfit) * 100)
	}

	// Per-trade return series = realized P&L / capital deployed that trade (rf=0).
	rets := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		if orderedCost[i] > 0 {
			rets = append(rets, orderedPnL[i]/orderedCost[i])
		}
	}
	if minSamples > 0 && len(rets) >= minSamples {
		mean := meanOf(rets)
		if sd := sampleStdDev(rets, mean); sd > 0 {
			s := round2(mean / sd)
			m.Sharpe = &s
		}
		if dd := downsideDeviation(rets); dd > 0 {
			so := round2(mean / dd)
			m.Sortino = &so
		}
	}
	return m
}

// EntryBandStat reports outcomes bucketed by the price a position was entered at —
// the lens the post-mortems used to find the money-losing 0.50–0.65 band. Surfacing
// it live lets the operator see that band forming instead of finding it after the run.
type EntryBandStat struct {
	Label       string  `json:"label"`
	Trades      int     `json:"trades"`
	WinRate     float64 `json:"win_rate"` // percentage
	RealizedPnL float64 `json:"realized_pnl"`
}

// BookMetrics is the whole-account scorecard: the same risk-adjusted block as the
// personas, plus win-rate-by-entry-band. This is breen's own performance record,
// complementing the per-persona (Gordon) breakdown.
type BookMetrics struct {
	RiskMetrics
	Trades     int             `json:"trades"`
	Wins       int             `json:"wins"`
	Losses     int             `json:"losses"`
	WinRate    float64         `json:"win_rate"`
	EntryBands []EntryBandStat `json:"entry_bands"`

	// Cost decomposition (breen-intelligence Phase 0). SpreadCostUSD is how much of
	// the realized P&L above was spent crossing spreads — it is already inside
	// RealizedPnL, so it explains the number rather than adjusting it.
	// NetPnLAfterFees is the one genuine adjustment: realized P&L minus the modelled
	// venue fees the paper account never charged.
	SpreadCostUSD   float64 `json:"spread_cost_usd"`
	FeeUSD          float64 `json:"fee_usd"`
	RealizedPnL     float64 `json:"realized_pnl"`
	NetPnLAfterFees float64 `json:"net_pnl_after_fees"`

	// Excursion averages: how far the average trade ran in each direction before it
	// closed. The gap between AvgMFEPct and the average realized return is the size of
	// the prize an exit policy is playing for (Phase 3).
	AvgMFEPct float64 `json:"avg_mfe_pct"`
	AvgMAEPct float64 `json:"avg_mae_pct"`
	// ExcursionSamples is how many trades carried excursion data — zero for a book of
	// only legacy/replayed trades, in which case the averages mean nothing.
	ExcursionSamples int `json:"excursion_samples"`
}

var entryBandEdges = []struct {
	label  string
	lo, hi float64
}{
	{"<0.20", 0, 0.20},
	{"0.20–0.35", 0.20, 0.35},
	{"0.35–0.50", 0.35, 0.50},
	{"0.50–0.65", 0.50, 0.65},
	{"0.65–0.80", 0.65, 0.80},
	{"≥0.80", 0.80, 1.0001},
}

func bandIndex(entry float64) int {
	for i, b := range entryBandEdges {
		if entry >= b.lo && entry < b.hi {
			return i
		}
	}
	return -1
}

// computeBookMetrics folds all closed trades into the account-level scorecard.
// capitalBase is the run's starting capital (the drawdown-% denominator).
func computeBookMetrics(trades []ClosedTrade, capitalBase float64, minSamples int) BookMetrics {
	ordered := make([]ClosedTrade, len(trades))
	copy(ordered, trades)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ClosedAtMs < ordered[j].ClosedAtMs })

	pnl := make([]float64, len(ordered))
	cost := make([]float64, len(ordered))

	bands := make([]EntryBandStat, len(entryBandEdges))
	bandWins := make([]int, len(entryBandEdges))
	for i := range entryBandEdges {
		bands[i].Label = entryBandEdges[i].label
	}

	wins := 0
	var spreadCost, feeCost, unchargedFees, realized, mfeSum, maeSum float64
	excursionSamples := 0
	for i, t := range ordered {
		pnl[i] = t.RealizedPnL
		cost[i] = math.Abs(t.EntryPrice * float64(t.Size))
		realized += t.RealizedPnL
		spreadCost += t.SpreadCostUSD
		feeCost += t.FeeUSD
		// Only fees NOT already netted into RealizedPnL still need subtracting; on a
		// fee-charged trade the P&L above is the net number already.
		if !t.FeeCharged {
			unchargedFees += t.FeeUSD
		}
		// A trade that was never marked carries 0/0; counting it would drag both
		// averages toward zero and make the excursion spread look narrower than it is.
		if t.MaxFavorableExcursionPct != 0 || t.MaxAdverseExcursionPct != 0 {
			mfeSum += t.MaxFavorableExcursionPct
			maeSum += t.MaxAdverseExcursionPct
			excursionSamples++
		}
		if t.RealizedPnL > 0 {
			wins++
		}
		if bi := bandIndex(t.EntryPrice); bi >= 0 {
			bands[bi].Trades++
			bands[bi].RealizedPnL = round2(bands[bi].RealizedPnL + t.RealizedPnL)
			if t.RealizedPnL > 0 {
				bandWins[bi]++
			}
		}
	}
	for i := range bands {
		if bands[i].Trades > 0 {
			bands[i].WinRate = round2(float64(bandWins[i]) / float64(bands[i].Trades) * 100)
		}
	}

	bm := BookMetrics{
		RiskMetrics:      computeRiskMetrics(pnl, cost, capitalBase, minSamples),
		Trades:           len(ordered),
		Wins:             wins,
		Losses:           len(ordered) - wins,
		EntryBands:       bands,
		SpreadCostUSD:    round2(spreadCost),
		FeeUSD:           round2(feeCost),
		RealizedPnL:      round2(realized),
		NetPnLAfterFees:  round2(realized - unchargedFees),
		ExcursionSamples: excursionSamples,
	}
	if bm.Trades > 0 {
		bm.WinRate = round2(float64(wins) / float64(bm.Trades) * 100)
	}
	if excursionSamples > 0 {
		bm.AvgMFEPct = round2(mfeSum / float64(excursionSamples))
		bm.AvgMAEPct = round2(maeSum / float64(excursionSamples))
	}
	return bm
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// sampleStdDev is the n−1 (Bessel-corrected) standard deviation.
func sampleStdDev(xs []float64, mean float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// downsideDeviation is the target semi-deviation against a 0 return: only returns
// below the target contribute, and the sum is divided by the full sample count.
func downsideDeviation(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var ss float64
	for _, x := range xs {
		if x < 0 {
			ss += x * x
		}
	}
	return math.Sqrt(ss / float64(len(xs)))
}
