package main

import "testing"

func TestResolvePersona(t *testing.T) {
	if p := resolvePersona("bull"); p.Stance != StanceBullish || p.RiskTier != RiskAggressive {
		t.Errorf("bull persona = %+v", p)
	}
	if p := resolvePersona(""); p.ID != "gordon-default" {
		t.Errorf("empty should resolve to default, got %q", p.ID)
	}
	if p := resolvePersona("does-not-exist"); p.ID != "gordon-default" {
		t.Errorf("unknown should fall back to default, got %q", p.ID)
	}
}

func TestIsDefaultPersona(t *testing.T) {
	if !isDefaultPersona("") || !isDefaultPersona("gordon-default") {
		t.Error("empty and gordon-default should be default")
	}
	if isDefaultPersona("bull") {
		t.Error("bull should not be default")
	}
}

func TestStaticControlsFromPersona(t *testing.T) {
	p := resolvePersona("cautious-bull")
	rc := NewStaticControls("", p.Stance, p.RiskTier, p.Threshold)
	if rc.Stance() != StanceBullish {
		t.Errorf("stance = %q", rc.Stance())
	}
	if rc.Threshold() != 0.60 {
		t.Errorf("threshold = %v, want 0.60", rc.Threshold())
	}
	lim, _ := rc.ReadLimits()
	if lim.MaxPositionSize != riskTiers[RiskConservative].MaxSize {
		t.Errorf("limits not from conservative tier: %+v", lim)
	}
	// rdb is nil → run() must be a no-op (never panics, returns immediately).
	if rc.rdb != nil {
		t.Error("static controls should have no redis client")
	}
}
