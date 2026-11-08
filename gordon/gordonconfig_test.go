package main

import "testing"

func testPersona() Persona {
	return Persona{
		ID: "bull", Label: "Bull", Stance: StanceBullish,
		RiskTier: RiskAggressive, Threshold: 0.45, Nudge: "base nudge",
	}
}

// merge with an empty blob keeps the compiled persona defaults (fail-safe).
func TestGordonConfigMergeEmptyKeepsDefaults(t *testing.T) {
	gc := NewGordonConfig("", testPersona())
	got := gc.merge(gordonConfigBlob{})
	if got.Nudge != "base nudge" || got.Stance != StanceBullish ||
		got.RiskTier != RiskAggressive || got.MatchThreshold != 0.45 {
		t.Fatalf("empty merge lost defaults: %+v", got)
	}
}

// A valid override replaces the defaults.
func TestGordonConfigMergeValidOverride(t *testing.T) {
	gc := NewGordonConfig("", testPersona())
	got := gc.merge(gordonConfigBlob{
		Nudge: "custom", Stance: StanceBearish, RiskTier: RiskConservative, MatchThreshold: 0.70,
	})
	if got.Nudge != "custom" || got.Stance != StanceBearish ||
		got.RiskTier != RiskConservative || got.MatchThreshold != 0.70 {
		t.Fatalf("valid override not applied: %+v", got)
	}
}

// Out-of-range / unknown values fall back to the compiled defaults.
func TestGordonConfigMergeSanitizes(t *testing.T) {
	gc := NewGordonConfig("", testPersona())
	got := gc.merge(gordonConfigBlob{Stance: "sideways", RiskTier: "yolo", MatchThreshold: 0.95})
	if got.Stance != StanceBullish {
		t.Errorf("bad stance not sanitized: %q", got.Stance)
	}
	if got.RiskTier != RiskAggressive {
		t.Errorf("bad risk tier not sanitized: %q", got.RiskTier)
	}
	if got.MatchThreshold != 0.45 {
		t.Errorf("out-of-range threshold not sanitized: %v", got.MatchThreshold)
	}
}

// apply installs merged state and fires the nudge sink only on change.
func TestGordonConfigApplyFiresNudgeSink(t *testing.T) {
	gc := NewGordonConfig("", testPersona())
	var got []string
	gc.SetNudgeSink(func(s string) { got = append(got, s) })
	if len(got) != 1 || got[0] != "base nudge" {
		t.Fatalf("sink not seeded with default nudge: %v", got)
	}
	gc.apply(gc.merge(gordonConfigBlob{Nudge: "changed", Stance: StanceBearish}))
	if gc.Stance() != StanceBearish {
		t.Errorf("stance getter = %q, want bearish", gc.Stance())
	}
	if len(got) != 2 || got[1] != "changed" {
		t.Fatalf("sink not fired on nudge change: %v", got)
	}
	// Re-applying the same nudge must not re-fire the sink.
	gc.apply(gc.merge(gordonConfigBlob{Nudge: "changed", Stance: StanceBearish}))
	if len(got) != 2 {
		t.Errorf("sink fired without a nudge change: %v", got)
	}
}

// A RedisControls with an attached config prefers its live values.
func TestRedisControlsPrefersGordonConfig(t *testing.T) {
	rc := NewStaticControls("", StanceBullish, RiskAggressive, 0.45)
	gc := NewGordonConfig("", testPersona())
	gc.apply(gc.merge(gordonConfigBlob{Stance: StanceBearish, RiskTier: RiskConservative, MatchThreshold: 0.70}))
	rc.SetGordonConfig(gc)

	if rc.Stance() != StanceBearish {
		t.Errorf("Stance() = %q, want bearish (from config)", rc.Stance())
	}
	if rc.RiskTier() != RiskConservative {
		t.Errorf("RiskTier() = %q, want conservative (from config)", rc.RiskTier())
	}
	if rc.Threshold() != 0.70 {
		t.Errorf("Threshold() = %v, want 0.70 (from config)", rc.Threshold())
	}
	lim, _ := rc.ReadLimits()
	if lim.MaxPositionSize != riskTiers[RiskConservative].MaxSize {
		t.Errorf("ReadLimits size = %d, want conservative tier", lim.MaxPositionSize)
	}
}
