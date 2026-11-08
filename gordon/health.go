package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

var healthStartTime = time.Now()

// effectiveStance/RiskTier/Threshold return the persona's LIVE values from the model
// controls (which fold in the Gordons-tab per-gordon config), falling back to the
// compiled persona defaults when controls aren't wired yet.
func effectiveStance(p Persona) string {
	if modelControlsState != nil {
		return modelControlsState.Stance()
	}
	return p.Stance
}

func effectiveRiskTier(p Persona) string {
	if modelControlsState != nil {
		return modelControlsState.RiskTier()
	}
	return p.RiskTier
}

func effectiveThreshold(p Persona) float64 {
	if modelControlsState != nil {
		return modelControlsState.Threshold()
	}
	return p.Threshold
}

// effectiveNudge returns the live personality prompt, falling back to the compiled default.
func effectiveNudge(oc *ProposalClient, p Persona) string {
	if oc != nil {
		return oc.PersonaNudge()
	}
	return p.Nudge
}

// HealthResponse is the common shape every Gekko component returns from /healthz.
type HealthResponse struct {
	Component     string                 `json:"component"`
	Status        string                 `json:"status"`
	UptimeSeconds float64                `json:"uptime_seconds"`
	Stats         map[string]interface{} `json:"stats"`
}

// startHealthServer exposes /healthz reporting Gordon's engine telemetry. Gordon
// is a TUI/headless worker with no other HTTP surface, so it runs on its own port.
func startHealthServer(addr string, tr *TelemetryRegistry, oc *ProposalClient, persona Persona) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		consumed, proposed, lag, avgLatencyUs, uptime := tr.HealthSnapshot()
		llmProposals, llmFallbacks := tr.LLMSourceCounts()
		llmWaits, llmAvgWaitMs, llmFailOpen, llmTimeouts := llmLimiter.Stats()
		stats := map[string]interface{}{
			"persona":             personaID,
			"events_consumed":     consumed,
			"proposals_generated": proposed,
			"kafka_offset_lag":    lag,
			"avg_latency_ms":      float64(avgLatencyUs) / 1000.0,
			// LLM source split — the fallback ratio surfaced on the System tab.
			"llm_proposals": llmProposals,
			"llm_fallbacks": llmFallbacks,
			// Numeric pre-gate: events vetoed before the LLM call (Ollama slots saved).
			"pregate_skips": tr.PregateSkipCount(),
			// Matches moved off the nearest market onto a tradeable neighbour.
			"match_rerouted": matchRerouted(),
			// Recent activity ring buffer (newest first): the per-gordon feed of what
			// it just did — proposals, declines, pre-gate/edge/skew/expiry vetoes,
			// pauses — so the Fleet Scorecard can show live activity, not just counts.
			"recent_activity": tr.RecentLogs(),
			// LLM concurrency limiter: how often generate calls waited for a
			// slot, their mean wait, fail-opens (Redis down), and clean give-ups.
			"llm_slot_waits":       llmWaits,
			"llm_slot_avg_wait_ms": llmAvgWaitMs,
			"llm_slot_fail_open":   llmFailOpen,
			"llm_slot_timeouts":    llmTimeouts,
			// Persona identity + live-editable knobs — always published (not just for
			// hosted backends) so the Gordons scorecard can render each gordon and seed
			// its editor from the EFFECTIVE values, and join against breen's risk metrics
			// (/api/personas) without a separate lookup. Effective (not compiled-default)
			// so live edits from the Gordons tab show up here.
			"stance":          effectiveStance(persona),
			"risk_tier":       effectiveRiskTier(persona),
			"match_threshold": effectiveThreshold(persona),
			"nudge":           effectiveNudge(oc, persona),
		}
		// Proposal backend identity: model name + provider, from whichever backend is
		// actually primary for this persona (ollama or hosted). Always published,
		// unlike the backend_* block below which is hosted-only telemetry.
		if oc != nil && oc.primary != nil {
			stats["backend"] = oc.primary.Name()
			if m, ok := oc.primary.Stats()["model"]; ok {
				stats["model"] = m
			}
		}
		// Hosted-backend telemetry — only present when this persona's PRIMARY backend
		// is a hosted API (not local Ollama), so the frontend can surface it as an
		// external-API card. Backend-scoped keys (backend_*) so the card works for
		// any hosted backend, not just Gemini: the backend's own Stats() provides
		// model/budget, the client the generic primary counters.
		if oc != nil && oc.primary != nil && oc.primary.Name() != ProviderOllama {
			stats["backend_persona"] = personaID
			for k, v := range oc.primary.Stats() {
				stats["backend_"+k] = v
			}
			ok, primaryErr, fallbacks, lastOK := oc.PrimaryCounters()
			stats["backend_primary_ok"] = ok
			stats["backend_primary_err"] = primaryErr
			stats["backend_fallbacks"] = fallbacks
			stats["backend_last_ok_unix"] = lastOK
		}
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Component:     "gordon",
			Status:        "ok",
			UptimeSeconds: uptime,
			Stats:         stats,
		})
	})

	go func() {
		log.Printf("[INFO] Gordon health endpoint listening on %s/healthz", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("[WARNING] Gordon health server stopped: %v", err)
		}
	}()
}

func matchRerouted() int64 {
	if marketMatcher == nil {
		return 0
	}
	return marketMatcher.rerouted.Load()
}
