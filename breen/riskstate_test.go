package main

import (
	"testing"
	"time"
)

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestComputeRiskState_FamiliesAndCooldown(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{
		MaxDrawdownHaltPct:   10,
		PerContractLossLimit: 300,
		ExitCooldown:         30 * time.Minute,
	})
	tr.startingCapital = 100000
	tr.lastNAV = 100000 // flat → no halt

	// Held family (open position).
	tr.positions["KXABC-26DEC31-B100"] = &Position{ContractID: "KXABC-26DEC31-B100", EntryPrice: 0.4, Size: 100}
	// Family past its loss limit via realized PnL.
	tr.contractRealizedPnL["KXLOSS-26DEC31"] = -350
	// Family in re-entry cooldown (exited 5m ago, 30m window).
	tr.lastExit["KXCOOL-26DEC31"] = time.Now().Add(-5 * time.Minute)
	// Family whose cooldown has already elapsed → must NOT appear.
	tr.lastExit["KXOLD-26DEC31"] = time.Now().Add(-45 * time.Minute)

	rs := tr.computeRiskState()

	if rs.Halted {
		t.Error("should not be halted at 0% drawdown")
	}
	if !contains(rs.HeldFamilies, "KXABC-26DEC31") {
		t.Errorf("held family missing: %v", rs.HeldFamilies)
	}
	if !contains(rs.LossLimitedFamilies, "KXLOSS-26DEC31") {
		t.Errorf("loss-limited family missing: %v", rs.LossLimitedFamilies)
	}
	if _, ok := rs.CooldownUntilMs["KXCOOL-26DEC31"]; !ok {
		t.Errorf("active cooldown family missing: %v", rs.CooldownUntilMs)
	}
	if _, ok := rs.CooldownUntilMs["KXOLD-26DEC31"]; ok {
		t.Error("elapsed cooldown must not be published")
	}
}

func TestComputeRiskState_DrawdownHalt(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{MaxDrawdownHaltPct: 10})
	tr.startingCapital = 100000
	tr.lastNAV = 85000 // 15% drawdown ≥ 10% halt line

	rs := tr.computeRiskState()

	if !rs.Halted {
		t.Fatal("expected halt at 15% drawdown vs 10% line")
	}
	if rs.HaltReason == "" {
		t.Error("halt reason should be populated")
	}
}

// A family that is both loss-limited AND currently held is still reported as held so
// Gordon lets a potential exit through (SELLs bypass the risk gate).
func TestComputeRiskState_LossLimitedButHeld(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{PerContractLossLimit: 300})
	tr.startingCapital = 100000
	tr.lastNAV = 100000

	tr.positions["KXHELD-26DEC31-B100"] = &Position{ContractID: "KXHELD-26DEC31-B100", EntryPrice: 0.4, Size: 100}
	tr.contractRealizedPnL["KXHELD-26DEC31"] = -350

	rs := tr.computeRiskState()

	if !contains(rs.HeldFamilies, "KXHELD-26DEC31") {
		t.Error("held family must be reported even when loss-limited")
	}
	if !contains(rs.LossLimitedFamilies, "KXHELD-26DEC31") {
		t.Error("loss-limited family should still be listed (Gordon uses held to override)")
	}
}
