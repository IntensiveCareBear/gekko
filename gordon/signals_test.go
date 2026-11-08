package main

import (
	"math"
	"testing"
)

func TestHeadlineSentiment(t *testing.T) {
	if s := headlineSentiment("Stocks surge to record high on strong growth"); s <= 0 {
		t.Errorf("positive headline scored %.2f", s)
	}
	if s := headlineSentiment("Markets plunge as recession fears mount and stocks crash"); s >= 0 {
		t.Errorf("negative headline scored %.2f", s)
	}
	if s := headlineSentiment("The committee met on Tuesday"); s != 0 {
		t.Errorf("neutral headline scored %.2f, want 0", s)
	}
}

func TestEntrySkewSizeMult(t *testing.T) {
	const floor = 0.30
	// Cheap entries (rr ≥ 1) keep full size.
	for _, p := range []float64{0.10, 0.30, 0.50} {
		if m := entrySkewSizeMult(p, floor); m != 1.0 {
			t.Errorf("entrySkewSizeMult(%.2f) = %.3f, want 1.0 (cheap entries unscaled)", p, m)
		}
	}
	// Rich entries scale down monotonically.
	m55, m60, m70 := entrySkewSizeMult(0.55, floor), entrySkewSizeMult(0.60, floor), entrySkewSizeMult(0.70, floor)
	if !(m55 < 1.0 && m60 < m55 && m70 < m60) {
		t.Errorf("expected monotonic downscale: 0.55→%.3f 0.60→%.3f 0.70→%.3f", m55, m60, m70)
	}
	// (1-0.6)/0.6 ≈ 0.667.
	if math.Abs(m60-0.6667) > 1e-3 {
		t.Errorf("entrySkewSizeMult(0.60) = %.4f, want ≈0.667", m60)
	}
	// Never below the floor, even at extreme prices.
	if m := entrySkewSizeMult(0.95, floor); m != floor {
		t.Errorf("entrySkewSizeMult(0.95) = %.3f, want floor %.2f", m, floor)
	}
	// Degenerate prices are left unscaled.
	for _, p := range []float64{0, 1, -0.1, 1.5} {
		if m := entrySkewSizeMult(p, floor); m != 1.0 {
			t.Errorf("entrySkewSizeMult(%.2f) = %.3f, want 1.0 (out of range)", p, m)
		}
	}
}

func TestTickerExpiry(t *testing.T) {
	// Parseable Kalshi event dates.
	if exp, ok := tickerExpiry("KXINX-26JUN26"); !ok || exp.Year() != 2026 || exp.Month() != 6 || exp.Day() != 26 {
		t.Errorf("KXINX-26JUN26 → %v ok=%v, want 2026-06-26", exp, ok)
	}
	// Intraday time suffix is honored.
	exp, ok := tickerExpiry("KXINXY-26DEC31H1600-B8300")
	if !ok || exp.Month() != 12 || exp.Day() != 31 || exp.Hour() != 16 || exp.Minute() != 0 {
		t.Errorf("KXINXY-26DEC31H1600 → %v ok=%v, want 2026-12-31 16:00", exp, ok)
	}
	// No intraday time ⇒ end of day.
	if exp, ok := tickerExpiry("KXFDVOPENSEA-27JAN01-4999999999"); !ok || exp.Hour() != 23 || exp.Minute() != 59 {
		t.Errorf("no-time ticker → %v ok=%v, want end-of-day", exp, ok)
	}
	// Bare HHMM intraday (sub-hourly crypto series) — the KXETH15M case that used to
	// default to 23:59 and dodge the near-expiry skip.
	if exp, ok := tickerExpiry("KXETH15M-26JUL090845-45"); !ok || exp.Day() != 9 || exp.Hour() != 8 || exp.Minute() != 45 {
		t.Errorf("KXETH15M-26JUL090845 → %v ok=%v, want 2026-07-09 08:45", exp, ok)
	}
	// Bare HH intraday (hourly crypto series).
	if exp, ok := tickerExpiry("KXBTCD-26JUL0917-T62249.99"); !ok || exp.Day() != 9 || exp.Hour() != 17 || exp.Minute() != 0 {
		t.Errorf("KXBTCD-26JUL0917 → %v ok=%v, want 2026-07-09 17:00", exp, ok)
	}
	// Unparseable / dateless tickers return false so the guard is skipped.
	for _, c := range []string{"JPMCEOCHANGE-27", "POLY-6823816331566743512", "NODASH", "KX-XX"} {
		if _, ok := tickerExpiry(c); ok {
			t.Errorf("tickerExpiry(%q) should not parse", c)
		}
	}
}

func TestCompositeConfidence_MomentumAligns(t *testing.T) {
	base := SignalInputs{MatchScore: 0.6}
	up := base
	up.TrendPct = 12 // strong up move
	confUp, multUp, _ := compositeConfidence(up, "BUY")
	confBase, _, _ := compositeConfidence(base, "BUY")
	if confUp <= confBase {
		t.Errorf("upward momentum should raise BUY confidence: %.3f vs %.3f", confUp, confBase)
	}
	if multUp <= 1.0 {
		t.Errorf("aligned momentum should size up, mult=%.3f", multUp)
	}
}

func TestCompositeConfidence_MomentumOpposesBuy(t *testing.T) {
	in := SignalInputs{MatchScore: 0.6, TrendPct: -12}
	conf, mult, _ := compositeConfidence(in, "BUY")
	if conf >= 0.6 {
		t.Errorf("downward momentum should lower BUY confidence, got %.3f", conf)
	}
	if mult >= 1.0 {
		t.Errorf("opposing momentum should size down, mult=%.3f", mult)
	}
}

func TestCompositeConfidence_CrossVenueEdge(t *testing.T) {
	// Positive edge ⇒ this venue cheaper ⇒ buying here is favorable.
	in := SignalInputs{MatchScore: 0.5, CrossEdge: 0.3}
	conf, _, notes := compositeConfidence(in, "BUY")
	if conf <= 0.5 {
		t.Errorf("favorable cross-venue edge should raise confidence, got %.3f", conf)
	}
	if len(notes) == 0 {
		t.Error("expected a cross-venue note")
	}
}

func TestCompositeConfidence_Clamps(t *testing.T) {
	in := SignalInputs{MatchScore: 0.9, TrendPct: 999, VolSurge: 10, CrossEdge: 1, Sentiment: 1}
	conf, mult, _ := compositeConfidence(in, "BUY")
	if conf < 0.05 || conf > 1.0 {
		t.Errorf("confidence out of [0.05,1.0]: %.3f", conf)
	}
	if mult < 0.6 || mult > 1.5 {
		t.Errorf("size mult out of [0.6,1.5]: %.3f", mult)
	}
}

func TestCompositeConfidence_NoMatchUsesNeutralBase(t *testing.T) {
	// MatchScore 0 (synthetic) ⇒ neutral 0.5 base, so a clean positive signal lifts it.
	in := SignalInputs{MatchScore: 0, Sentiment: 1}
	conf, _, _ := compositeConfidence(in, "BUY")
	if math.Abs(conf-0.5) < 1e-9 || conf < 0.5 {
		t.Errorf("expected lift above neutral 0.5, got %.3f", conf)
	}
}

func TestEdgeVsMarket_Buy(t *testing.T) {
	// Fair 0.60 vs ask 0.50 ⇒ +0.10 edge (market underprices YES).
	edge, gated := edgeVsMarket("BUY", 0.60, 0.45, 0.50)
	if !gated || math.Abs(edge-0.10) > 1e-9 {
		t.Errorf("BUY edge = %.3f gated=%v, want +0.100 true", edge, gated)
	}
	// Fair 0.50 vs ask 0.60 ⇒ negative edge (paying more than fair).
	edge, gated = edgeVsMarket("BUY", 0.50, 0.55, 0.60)
	if !gated || edge >= 0 {
		t.Errorf("overpriced BUY should have negative edge, got %.3f gated=%v", edge, gated)
	}
}

func TestEdgeVsMarket_Sell(t *testing.T) {
	// Fair 0.30 vs bid 0.45 ⇒ +0.15 edge (market overprices YES; sell it).
	edge, gated := edgeVsMarket("sell", 0.30, 0.45, 0.50)
	if !gated || math.Abs(edge-0.15) > 1e-9 {
		t.Errorf("SELL edge = %.3f gated=%v, want +0.150 true", edge, gated)
	}
}

func TestEdgeVsMarket_MissingBookSideNotGated(t *testing.T) {
	if _, gated := edgeVsMarket("BUY", 0.60, 0.45, 0); gated {
		t.Error("BUY with no ask must not be gated")
	}
	if _, gated := edgeVsMarket("SELL", 0.60, 0, 0.50); gated {
		t.Error("SELL with no bid must not be gated")
	}
}

func TestFairImplausible(t *testing.T) {
	// The hallucination pattern: LLM says a 2¢ market is worth 15¢ (7.5× ask).
	if !fairImplausible("BUY", 0.15, 0.02, 3.0) {
		t.Error("fair 7.5× the ask should be implausible")
	}
	// At/under the cap passes: fair 0.06 on ask 0.02 is exactly 3×.
	if fairImplausible("BUY", 0.06, 0.02, 3.0) {
		t.Error("fair at exactly the ratio cap should pass")
	}
	// Never bites non-cheap contracts: ask 0.40 → cap 1.20 > any probability.
	if fairImplausible("BUY", 0.95, 0.40, 3.0) {
		t.Error("high-conviction fair on a mid-priced ask should pass")
	}
	// SELLs, missing asks, and a zero ratio are never implausible.
	if fairImplausible("SELL", 0.15, 0.02, 3.0) {
		t.Error("SELLs are not subject to the cap")
	}
	if fairImplausible("BUY", 0.15, 0, 3.0) {
		t.Error("missing ask must not be implausible (book gate handles it)")
	}
	if fairImplausible("BUY", 0.99, 0.01, 0) {
		t.Error("ratio 0 disables the cap")
	}
}

func TestRequiredEdge(t *testing.T) {
	// Two-sided book: edge must cover base + spread.
	if got := requiredEdge(0.05, 0.40, 0.45); math.Abs(got-0.10) > 1e-9 {
		t.Errorf("requiredEdge = %.3f, want 0.100 (0.05 + 0.05 spread)", got)
	}
	// Missing bid: no spread to add.
	if got := requiredEdge(0.05, 0, 0.45); got != 0.05 {
		t.Errorf("requiredEdge with no bid = %.3f, want 0.05", got)
	}
	// Crossed/zero-width book: no spread to add.
	if got := requiredEdge(0.05, 0.45, 0.45); got != 0.05 {
		t.Errorf("requiredEdge with zero spread = %.3f, want 0.05", got)
	}
}

func TestCompositeConfidenceImbalance(t *testing.T) {
	base := SignalInputs{MatchScore: 0.6}

	// Bid-heavy book (imbalance +) raises a BUY's confidence but not below the deadband.
	small, _, _ := compositeConfidence(SignalInputs{MatchScore: 0.6, Imbalance: 0.10, ImbalanceConf: 1}, "BUY")
	if c, _, _ := compositeConfidence(base, "BUY"); small != c {
		t.Errorf("imbalance below deadband moved confidence: %.4f vs %.4f", small, c)
	}
	bullBuy, _, notes := compositeConfidence(SignalInputs{MatchScore: 0.6, Imbalance: 0.8, ImbalanceConf: 1}, "BUY")
	if bullBuy <= 0.6 {
		t.Errorf("bid-heavy book should raise BUY confidence: %.4f", bullBuy)
	}
	if len(notes) == 0 {
		t.Errorf("expected a book-imbalance note")
	}

	// Same bid-heavy book LOWERS a SELL's conviction (direction flips the sign).
	bullSell, _, _ := compositeConfidence(SignalInputs{MatchScore: 0.6, Imbalance: 0.8, ImbalanceConf: 1}, "SELL")
	if bullSell >= 0.6 {
		t.Errorf("bid-heavy book should lower SELL conviction: %.4f", bullSell)
	}

	// Ask-heavy book raises a SELL/bearish conviction — the bear-capable payoff.
	bearSell, _, _ := compositeConfidence(SignalInputs{MatchScore: 0.6, Imbalance: -0.8, ImbalanceConf: 1}, "SELL")
	if bearSell <= 0.6 {
		t.Errorf("ask-heavy book should raise SELL conviction: %.4f", bearSell)
	}

	// Reliability discount: a low-confidence book moves confidence less than a full one.
	strong, _, _ := compositeConfidence(SignalInputs{MatchScore: 0.6, Imbalance: 0.8, ImbalanceConf: 1.0}, "BUY")
	weak, _, _ := compositeConfidence(SignalInputs{MatchScore: 0.6, Imbalance: 0.8, ImbalanceConf: 0.2}, "BUY")
	if !(weak < strong) {
		t.Errorf("low-reliability book should move confidence less: weak %.4f, strong %.4f", weak, strong)
	}

	// Zero reliability is a no-op regardless of imbalance.
	if c, _, _ := compositeConfidence(SignalInputs{MatchScore: 0.6, Imbalance: 0.9, ImbalanceConf: 0}, "BUY"); c != 0.6 {
		t.Errorf("zero-reliability imbalance moved confidence: %.4f", c)
	}
}

func TestCredibilityScalesSizeAndConfidence(t *testing.T) {
	base := SignalInputs{MatchScore: 0.6}
	c0, m0, _ := compositeConfidence(base, "BUY")
	if c0 != 0.6 || m0 != 1.0 {
		t.Fatalf("unknown credibility must be a no-op, got %v %v", c0, m0)
	}
	lo, hi := base, base
	lo.Credibility, hi.Credibility = 0.30, 0.95
	cl, ml, notes := compositeConfidence(lo, "BUY")
	ch, mh, _ := compositeConfidence(hi, "BUY")
	if !(cl < c0 && c0 < ch && ml < m0 && m0 < mh) || len(notes) == 0 {
		t.Fatalf("expected low<neutral<high: conf %v %v %v size %v %v %v", cl, c0, ch, ml, m0, mh)
	}
	if sourceCredibility("TWITTER", "x") >= sourceCredibility("REUTERS", "x") || sourceCredibility("KALSHI", "x") != 0 {
		t.Fatal("tier table order wrong")
	}
}
