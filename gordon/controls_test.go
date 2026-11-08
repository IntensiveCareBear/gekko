package main

import "testing"

func TestReadLimitsByTier(t *testing.T) {
	rc := NewRedisControls("", 2500, 8000.0, 0.03, 0.5) // no redis; balanced default
	cases := []struct {
		tier     string
		wantSize int
		wantExp  float64
	}{
		{RiskConservative, 1000, 4000.0},
		{RiskBalanced, 2500, 8000.0},
		{RiskAggressive, 5000, 16000.0},
	}
	for _, c := range cases {
		rc.state.RiskTier = c.tier
		lim, err := rc.ReadLimits()
		if err != nil {
			t.Fatalf("%s: %v", c.tier, err)
		}
		if lim.MaxPositionSize != c.wantSize || lim.MaxExposure != c.wantExp {
			t.Errorf("%s: got size=%d exp=%.0f, want %d/%.0f", c.tier, lim.MaxPositionSize, lim.MaxExposure, c.wantSize, c.wantExp)
		}
		if lim.MaxSlippagePct != 0.03 {
			t.Errorf("%s: slippage = %v, want 0.03 (env-seeded default)", c.tier, lim.MaxSlippagePct)
		}
	}
}

func TestStanceDemoteFactor(t *testing.T) {
	cases := []struct {
		stance, action string
		want           float64
	}{
		{StanceNeutral, "BUY", 1.0},
		{StanceNeutral, "SELL", 1.0},
		{StanceBullish, "BUY", 1.0},  // agrees
		{StanceBullish, "SELL", 0.4}, // opposes → demoted
		{StanceBearish, "SELL", 1.0}, // agrees
		{StanceBearish, "BUY", 0.4},  // opposes → demoted
		{StanceBullish, "sell", 0.4}, // case-insensitive
	}
	for _, c := range cases {
		if got := stanceDemoteFactor(c.stance, c.action); got != c.want {
			t.Errorf("stance=%s action=%s: got %.2f, want %.2f", c.stance, c.action, got, c.want)
		}
	}
}

func TestSanitizeCoercesBadValues(t *testing.T) {
	rc := NewRedisControls("", 2500, 8000.0, 0.03, 0.5)
	got := rc.sanitize(modelControls{Stance: "moon", RiskTier: "yolo", MatchThreshold: 9.9, MaxSlippagePct: 0.5})
	if got.Stance != StanceNeutral {
		t.Errorf("stance = %q, want neutral", got.Stance)
	}
	if got.RiskTier != RiskBalanced {
		t.Errorf("risk_tier = %q, want balanced", got.RiskTier)
	}
	if got.MatchThreshold != 0.5 {
		t.Errorf("threshold = %v, want 0.5 (default)", got.MatchThreshold)
	}
	if got.MaxSlippagePct != 0.03 {
		t.Errorf("max_slippage_pct = %v, want 0.03 (default)", got.MaxSlippagePct)
	}
	// A blob written before the slippage field existed unmarshals to zero — it
	// must fall back to the env-seeded cap, not disable the guard.
	got = rc.sanitize(modelControls{Stance: StanceNeutral, RiskTier: RiskBalanced, MatchThreshold: 0.5})
	if got.MaxSlippagePct != 0.03 {
		t.Errorf("legacy blob: max_slippage_pct = %v, want 0.03", got.MaxSlippagePct)
	}
}

func TestStaticControlsTrackSlippageOnly(t *testing.T) {
	rc := NewStaticControls("", StanceBullish, RiskAggressive, 0.6)
	lim, err := rc.ReadLimits()
	if err != nil {
		t.Fatal(err)
	}
	if lim.MaxSlippagePct != 0.03 {
		t.Errorf("seed slippage = %v, want 0.03", lim.MaxSlippagePct)
	}
	// A global-blob update (what refresh() feeds through apply): a slipOnly
	// controls object adopts the new cap but keeps its fixed personality.
	rc.apply(rc.sanitize(modelControls{Stance: StanceBearish, RiskTier: RiskConservative, MatchThreshold: 0.4, MaxSlippagePct: 0.05}))
	if got := rc.Stance(); got != StanceBullish {
		t.Errorf("stance = %q, want bullish (personality must stay fixed)", got)
	}
	lim, _ = rc.ReadLimits()
	if lim.MaxSlippagePct != 0.05 {
		t.Errorf("slippage = %v, want 0.05 (fleet-wide cap tracked)", lim.MaxSlippagePct)
	}
	if lim.MaxPositionSize != riskTiers[RiskAggressive].MaxSize {
		t.Errorf("size cap changed: got %d, want aggressive tier", lim.MaxPositionSize)
	}
}

func TestTierForLimits(t *testing.T) {
	if got := tierForLimits(2500, 8000.0); got != RiskBalanced {
		t.Errorf("got %q, want balanced", got)
	}
	if got := tierForLimits(999, 1.0); got != RiskBalanced {
		t.Errorf("unmatched should fall back to balanced, got %q", got)
	}
}

func TestReadLimitsScaleWithBetSizeMult(t *testing.T) {
	old := betSizeMult
	betSizeMult = 10
	defer func() { betSizeMult = old }()
	rc := NewRedisControls("", 2500, 8000.0, 0.03, 0.5)
	rc.state.RiskTier = RiskAssertive
	lim, _ := rc.ReadLimits()
	if lim.MaxPositionSize != 37500 || lim.MaxExposure != 120000.0 {
		t.Errorf("got size=%d exp=%.0f, want 37500/120000", lim.MaxPositionSize, lim.MaxExposure)
	}
	if lim.MaxSlippagePct != 0.03 {
		t.Errorf("slippage = %v, want unscaled 0.03", lim.MaxSlippagePct)
	}
}
