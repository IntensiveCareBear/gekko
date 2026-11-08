package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// RiskState is a compact, published mirror of the deterministic, price/size-independent
// parts of riskGate, so Gordon can veto a proposal BEFORE spending an LLM slot on one
// the executive would reject anyway. It carries only what a pre-LLM check needs:
//   - Halted: the drawdown circuit breaker is tripped (all new entries paused).
//   - HeldFamilies: families with an open position. Gordon must NOT pre-gate these — a
//     proposal there could be a valid exit (SELLs always pass riskGate) or a bypass-
//     cooldown add, so those go to the LLM and let breen decide.
//   - CooldownUntilMs / LossLimitedFamilies: per-family re-entry cooldown and cumulative
//     loss-limit state — new entries in these are rejected.
//
// It intentionally omits the checks that need the proposal's price/size (mid-band gate,
// notional/deployed caps) and the category loss-limit (which needs breen's keyword
// table); those stay enforced in riskGate as the backstop.
type RiskState struct {
	AsOfMs              int64            `json:"as_of_ms"`
	Halted              bool             `json:"halted"`
	HaltReason          string           `json:"halt_reason,omitempty"`
	HeldFamilies        []string         `json:"held_families,omitempty"`
	HeldContracts       []string         `json:"held_contracts"`
	CooldownUntilMs     map[string]int64 `json:"cooldown_until_ms,omitempty"`
	LossLimitedFamilies []string         `json:"loss_limited_families,omitempty"`
}

// computeRiskState snapshots the current deterministic risk state. Mirrors the relevant
// branches of riskGate (drawdown halt, family loss limit, re-entry cooldown) so the
// pre-LLM veto and the authoritative gate can't diverge.
func (pt *PositionTracker) computeRiskState() RiskState {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	caps := pt.caps
	nav := pt.lastNAV
	if nav <= 0 {
		nav = pt.startingCapital
	}
	starting := pt.startingCapital

	rs := RiskState{AsOfMs: time.Now().UnixMilli()}

	// Drawdown circuit breaker (riskGate: new entries paused past the halt line).
	if caps.MaxDrawdownHaltPct > 0 && starting > 0 {
		if ddPct := (starting - nav) / starting * 100.0; ddPct >= caps.MaxDrawdownHaltPct {
			rs.Halted = true
			rs.HaltReason = fmt.Sprintf("drawdown %.1f%% ≥ halt %.1f%%", ddPct, caps.MaxDrawdownHaltPct)
		}
	}

	// Per-family open unrealized (cache-marked, like riskGate) + the held set.
	// HeldContracts is always non-nil (marshals as [] not null) so consumers can tell
	// "new snapshot, nothing held" apart from an old blob missing the field.
	familyUnreal := map[string]float64{}
	heldSet := map[string]bool{}
	rs.HeldContracts = []string{}
	for id, p := range pt.positions {
		fam := contractFamily(id)
		heldSet[fam] = true
		rs.HeldContracts = append(rs.HeldContracts, id)
		mark := p.EntryPrice
		if m, ok := pt.markCache[id]; ok {
			mark = m.Price
		}
		familyUnreal[fam] += (mark - p.EntryPrice) * float64(p.Size)
	}
	for fam := range heldSet {
		rs.HeldFamilies = append(rs.HeldFamilies, fam)
	}

	// Per-family cumulative loss limit (realized + open unrealized ≤ -limit), mirroring
	// riskGate. Check every family with either realized history or an open position.
	if caps.PerContractLossLimit > 0 {
		seen := map[string]bool{}
		check := func(fam string) {
			if seen[fam] {
				return
			}
			seen[fam] = true
			if bleed := pt.contractRealizedPnL[fam] + familyUnreal[fam]; bleed <= -caps.PerContractLossLimit {
				rs.LossLimitedFamilies = append(rs.LossLimitedFamilies, fam)
			}
		}
		for fam := range pt.contractRealizedPnL {
			check(fam)
		}
		for fam := range familyUnreal {
			check(fam)
		}
	}

	// Re-entry cooldown expiry per family (riskGate only applies it when the family is
	// not currently held; Gordon also skips gating held families, so publishing all
	// still-active cooldowns is consistent).
	if caps.ExitCooldown > 0 {
		for fam, t := range pt.lastExit {
			if until := t.Add(caps.ExitCooldown); time.Now().Before(until) {
				if rs.CooldownUntilMs == nil {
					rs.CooldownUntilMs = map[string]int64{}
				}
				rs.CooldownUntilMs[fam] = until.UnixMilli()
			}
		}
	}

	return rs
}

// publishRiskState writes the current risk state to Redis for Gordon's pre-LLM veto.
// Called off-lock from syncToRedis (computeRiskState takes its own RLock).
func (pt *PositionTracker) publishRiskState(ctx context.Context) {
	if pt.redisClient == nil {
		return
	}
	if data, err := json.Marshal(pt.computeRiskState()); err == nil {
		_ = pt.redisClient.Set(ctx, riskStateKey, string(data), 0).Err()
	}
}
