package main

import "testing"

// outInt reads a counter foldGordon stored natively (int64) in its output stats.
func outInt(t *testing.T, m map[string]interface{}, k string) int64 {
	t.Helper()
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		t.Fatalf("stat %q has unexpected type %T", k, m[k])
		return 0
	}
}

func gordonStat(consumed, proposed int) componentStatus {
	return gordonStatLLM(consumed, proposed, 0, 0)
}

// gordonStatLLM adds the LLM source-split counters (proposals decided by the LLM
// vs rules fallbacks) so the fold's fallback-ratio aggregation can be exercised.
func gordonStatLLM(consumed, proposed, llm, fallback int) componentStatus {
	return componentStatus{
		Component: "gordon", Reachable: true, Status: "ok", UptimeSeconds: 12.5,
		Stats: map[string]interface{}{
			"events_consumed":     float64(consumed), // JSON numbers decode as float64
			"proposals_generated": float64(proposed),
			"kafka_offset_lag":    float64(0),
			"avg_latency_ms":      2.0,
			"llm_proposals":       float64(llm),
			"llm_fallbacks":       float64(fallback),
		},
	}
}

func TestFoldGordon_SumsAcrossPersonas(t *testing.T) {
	cs := foldGordon([]componentStatus{
		gordonStat(100, 24), gordonStat(100, 22), gordonStat(100, 20),
	})
	if !cs.Reachable || cs.Status != "ok" {
		t.Fatalf("expected reachable/ok, got reachable=%v status=%s", cs.Reachable, cs.Status)
	}
	if got := outInt(t, cs.Stats, "proposals_generated"); got != 66 {
		t.Errorf("proposals_generated = %d, want 66 (24+22+20)", got)
	}
	if got := outInt(t, cs.Stats, "events_consumed"); got != 300 {
		t.Errorf("events_consumed = %d, want 300", got)
	}
	if got := outInt(t, cs.Stats, "personas_reachable"); got != 3 {
		t.Errorf("personas_reachable = %d, want 3", got)
	}
}

func TestFoldGordon_SumsLLMFallbackCounts(t *testing.T) {
	// Three personas: 8/2, 6/4, 10/0 (llm/fallback) → totals 24 llm, 6 fallback,
	// i.e. a 20% fleet fallback ratio the frontend can derive from the sums.
	cs := foldGordon([]componentStatus{
		gordonStatLLM(100, 10, 8, 2),
		gordonStatLLM(100, 10, 6, 4),
		gordonStatLLM(100, 10, 10, 0),
	})
	if got := outInt(t, cs.Stats, "llm_proposals"); got != 24 {
		t.Errorf("llm_proposals = %d, want 24 (8+6+10)", got)
	}
	if got := outInt(t, cs.Stats, "llm_fallbacks"); got != 6 {
		t.Errorf("llm_fallbacks = %d, want 6 (2+4+0)", got)
	}
}

func TestFoldGordon_DegradedWhenOneDown(t *testing.T) {
	cs := foldGordon([]componentStatus{
		gordonStat(10, 5),
		{Component: "gordon", Reachable: false, Status: "unreachable"},
	})
	if cs.Status != "degraded" {
		t.Errorf("status = %s, want degraded", cs.Status)
	}
	if got := outInt(t, cs.Stats, "proposals_generated"); got != 5 {
		t.Errorf("proposals_generated = %d, want 5 (only reachable persona counts)", got)
	}
	if got := outInt(t, cs.Stats, "personas_reachable"); got != 1 {
		t.Errorf("personas_reachable = %d, want 1", got)
	}
}

func TestFoldGordon_NoneReachable(t *testing.T) {
	cs := foldGordon([]componentStatus{{Reachable: false}, {Reachable: false}})
	if cs.Reachable || cs.Status != "unreachable" {
		t.Errorf("expected unreachable, got reachable=%v status=%s", cs.Reachable, cs.Status)
	}
}
