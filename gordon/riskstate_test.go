package main

import (
	"context"
	"testing"
	"time"
)

func TestContractFamily(t *testing.T) {
	cases := map[string]string{
		"KXINXY-26DEC31-B8300": "KXINXY-26DEC31", // strike ladder → family drops the strike
		"KXFED-26JUN26":        "KXFED-26JUN26",  // 2 segments → whole id
		"SINGLE":               "SINGLE",
	}
	for in, want := range cases {
		if got := contractFamily(in); got != want {
			t.Errorf("contractFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

// primedReader builds a reader with a preset cache so Evaluate never touches Redis
// (haveOnce + a fresh fetch time short-circuits refresh).
func primedReader(rs riskState) *RiskStateReader {
	held := map[string]bool{}
	for _, f := range rs.HeldFamilies {
		held[f] = true
	}
	loss := map[string]bool{}
	for _, f := range rs.LossLimitedFamilies {
		loss[f] = true
	}
	return &RiskStateReader{
		ttl: time.Minute, haveOnce: true, fetchedAt: time.Now(),
		cached: rs, heldSet: held, lossSet: loss, loaded: true,
	}
}

func TestRiskStateReader_Evaluate(t *testing.T) {
	ctx := context.Background()
	future := time.Now().Add(20 * time.Minute).UnixMilli()
	past := time.Now().Add(-time.Minute).UnixMilli()
	r := primedReader(riskState{
		HeldFamilies:        []string{"KXHELD-26DEC31"},
		LossLimitedFamilies: []string{"KXLOSS-26DEC31"},
		CooldownUntilMs:     map[string]int64{"KXCOOL-26DEC31": future, "KXOLD-26DEC31": past},
	})

	// Held family: never pre-gated (could be an exit / bypass-cooldown add).
	if held, blocked, _ := r.Evaluate(ctx, "KXHELD-26DEC31-B100"); !held || blocked {
		t.Errorf("held family: got held=%v blocked=%v, want true/false", held, blocked)
	}
	// Loss-limited family, not held → blocked.
	if _, blocked, reason := r.Evaluate(ctx, "KXLOSS-26DEC31-B100"); !blocked {
		t.Error("loss-limited family should be blocked")
	} else if reason == "" {
		t.Error("blocked reason should be set")
	}
	// Active cooldown → blocked.
	if _, blocked, _ := r.Evaluate(ctx, "KXCOOL-26DEC31-B100"); !blocked {
		t.Error("family in active cooldown should be blocked")
	}
	// Expired cooldown → not blocked.
	if _, blocked, _ := r.Evaluate(ctx, "KXOLD-26DEC31-B100"); blocked {
		t.Error("expired cooldown must not block")
	}
	// Clean family → allowed.
	if held, blocked, _ := r.Evaluate(ctx, "KXFRESH-26DEC31-B100"); held || blocked {
		t.Errorf("clean family: got held=%v blocked=%v, want false/false", held, blocked)
	}
}

func TestRiskStateReader_HaltOverriddenByHeld(t *testing.T) {
	ctx := context.Background()
	r := primedReader(riskState{Halted: true, HaltReason: "drawdown 12%", HeldFamilies: []string{"KXHELD-26DEC31"}})

	// Halted: a new-entry family is blocked...
	if _, blocked, _ := r.Evaluate(ctx, "KXNEW-26DEC31-B100"); !blocked {
		t.Error("new entry during halt should be blocked")
	}
	// ...but a held family is not, so its exit can still reach the LLM/executive.
	if _, blocked, _ := r.Evaluate(ctx, "KXHELD-26DEC31-B100"); blocked {
		t.Error("held family must not be pre-gated even during a halt (exit path)")
	}
}

func TestRiskStateReader_NilFailOpen(t *testing.T) {
	var r *RiskStateReader // nil reader (no Redis configured)
	if held, blocked, reason := r.Evaluate(context.Background(), "KXANY-26DEC31-B100"); held || blocked || reason != "" {
		t.Errorf("nil reader must fail open: got held=%v blocked=%v reason=%q", held, blocked, reason)
	}
	if held, known := r.HeldFamily(context.Background(), "KXANY-26DEC31-B100"); held || known {
		t.Errorf("nil reader HeldFamily must be unknown: got held=%v known=%v", held, known)
	}
}

func TestRiskStateReader_HeldFamily(t *testing.T) {
	ctx := context.Background()
	r := primedReader(riskState{HeldFamilies: []string{"KXHELD-26DEC31"}})

	// Any strike in a held family is held; the answer is authoritative.
	if held, known := r.HeldFamily(ctx, "KXHELD-26DEC31-B100"); !held || !known {
		t.Errorf("held family: got held=%v known=%v, want true/true", held, known)
	}
	// Unheld family with a loaded snapshot: authoritative "not held" (SELL droppable).
	if held, known := r.HeldFamily(ctx, "KXOTHER-26DEC31-B100"); held || !known {
		t.Errorf("unheld family: got held=%v known=%v, want false/true", held, known)
	}
	// No decoded snapshot (absent key / Redis error): unknown — callers must pass
	// through so a breen outage can't suppress valid exits.
	r.loaded = false
	if held, known := r.HeldFamily(ctx, "KXHELD-26DEC31-B100"); held || known {
		t.Errorf("unloaded state: got held=%v known=%v, want false/false", held, known)
	}
}
