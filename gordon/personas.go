package main

import (
	"log"
	"time"
)

// Persona is one Gordon "personality" — a fixed stance/risk/selectivity plus a
// prompt nudge. Multiple Gordon processes run with different personas so the
// executive can compare competing opinions (Phase 2). Reuses the stance/risk/
// threshold semantics from controls.go.
type Persona struct {
	ID        string
	Label     string
	Stance    string  // StanceBullish / StanceNeutral / StanceBearish
	RiskTier  string  // RiskConservative / RiskBalanced / RiskAggressive
	Threshold float64 // match selectivity [0.35, 0.80]
	Nudge     string  // one-line bias injected into the LLM proposal prompt
	// Model optionally overrides OLLAMA_MODEL for this persona, letting a single
	// deliberate persona run a heavier model (e.g. deepseek-r1:7b) while the rest
	// of the fleet stays on the fast default. Empty ⇒ use the configured model.
	Model string
	// Timeout optionally overrides OLLAMA_PROPOSAL_TIMEOUT for this persona. A
	// reasoning model emits long <think> traces and needs a larger deadline than
	// the fast fleet. Zero ⇒ use the configured timeout.
	Timeout time.Duration
	// Provider selects this persona's proposal BACKEND: ProviderOllama (local,
	// the default when empty) or ProviderGemini (hosted API), for A/B-testing a
	// hosted model on the per-persona scorecard. It is a first-class registry field
	// — set it here to give a persona a backend without any env toggle. The
	// GEMINI_PERSONAS env var stays as a convenience override that flips matching
	// personas to Gemini at runtime (see main.go), so an ad-hoc A/B needs no
	// registry edit. Embeddings and the fallback always stay on local Ollama.
	Provider string
}

// personaRegistry is the built-in set. Externalizing to a JSON file can come
// later; a code registry keeps Phase 2 simple and type-safe.
var personaRegistry = map[string]Persona{
	"gordon-default": {
		ID: "gordon-default", Label: "Gordon", Stance: StanceNeutral,
		RiskTier: RiskBalanced, Threshold: 0.50,
		Nudge: "",
	},
	// weather is the dedicated deterministic weather trader (GORDON_WEATHER_MODE=true):
	// it prices Kalshi high-temp buckets from METAR observations and never calls the
	// LLM, so its Nudge/Threshold are unused — the entry only gives it a stable
	// scorecard identity alongside the LLM personas.
	"weather": {
		ID: "weather", Label: "Weather", Stance: StanceNeutral,
		RiskTier: RiskBalanced, Threshold: 0.50,
		Nudge: "",
	},
	"bull": {
		ID: "bull", Label: "Bull", Stance: StanceBullish,
		RiskTier: RiskAggressive, Threshold: 0.45,
		Nudge: "You are aggressively bullish: favor BUY/YES, size up on upside catalysts, and act on thinner conviction.",
	},
	// gemini-bull is an exact clone of "bull" pinned to the hosted Gemini backend, so
	// the same aggressive-bull strategy can be A/B'd across backends (local qwen vs
	// Gemini) on the per-persona scorecard. Distinct id ⇒ its own fleet heartbeat,
	// Kafka group, markets collection, and stats bucket (a second "bull" would collide).
	"gemini-bull": {
		ID: "gemini-bull", Label: "Gemini Bull", Stance: StanceBullish,
		RiskTier: RiskAggressive, Threshold: 0.45,
		Nudge:    "You are aggressively bullish: favor BUY/YES, size up on upside catalysts, and act on thinner conviction.",
		Provider: ProviderGemini,
	},
	// llama-bull is the same aggressive-bull strategy as "bull" but pinned to the
	// local llama3.1:8b model (vs the fleet-default qwen2.5:7b), so the two local
	// models can be A/B'd on the per-persona scorecard. Provider stays empty ⇒ local
	// Ollama; the Model override routes just this persona to llama3.1:8b. Distinct id
	// ⇒ its own fleet heartbeat, Kafka group, markets collection, and stats bucket.
	"llama-bull": {
		ID: "llama-bull", Label: "Llama Bull", Stance: StanceBullish,
		RiskTier: RiskAggressive, Threshold: 0.45,
		Nudge:    "You are aggressively bullish: favor BUY/YES, size up on upside catalysts, and act on thinner conviction.",
		Model:    "llama3.1:8b",
	},
	"cautious-bull": {
		ID: "cautious-bull", Label: "Cautious Bull", Stance: StanceBullish,
		RiskTier: RiskConservative, Threshold: 0.60,
		Nudge: "You are cautiously bullish: lean BUY/YES but only on strong, well-supported signals; keep size modest.",
	},
	"bear": {
		ID: "bear", Label: "Bear", Stance: StanceBearish,
		RiskTier: RiskBalanced, Threshold: 0.55,
		Nudge: "You are bearish: favor SELL/NO and fading hype; demand solid evidence before taking upside.",
	},
	// gemini-bear is the Gemini-backend twin of "bear" (see gemini-bull): same bearish
	// strategy on the hosted backend, so {bull,bear} × {ollama,gemini} is a clean 2×2
	// backend A/B on the scorecard. Distinct id ⇒ its own heartbeat/group/collection/stats.
	"gemini-bear": {
		ID: "gemini-bear", Label: "Gemini Bear", Stance: StanceBearish,
		RiskTier: RiskBalanced, Threshold: 0.55,
		Nudge:    "You are bearish: favor SELL/NO and fading hype; demand solid evidence before taking upside.",
		Provider: ProviderGemini,
	},
	"cautious-bear": {
		ID: "cautious-bear", Label: "Cautious Bear", Stance: StanceBearish,
		RiskTier: RiskConservative, Threshold: 0.65,
		Nudge: "You are cautiously bearish: lean SELL/NO, fade hype, and only take positions on strong, well-supported signals with modest size.",
	},
	"aggressive-bear": {
		ID: "aggressive-bear", Label: "Aggressive Bear", Stance: StanceBearish,
		RiskTier: RiskAggressive, Threshold: 0.45,
		Nudge: "You are aggressively bearish: strongly favor SELL/NO, fade hype quickly, size up on downside catalysts, and act on thinner conviction.",
	},
	"neutral": {
		ID: "neutral", Label: "Neutral", Stance: StanceNeutral,
		RiskTier: RiskBalanced, Threshold: 0.50,
		Nudge: "You are market-neutral: take whichever side the evidence implies, without directional bias.",
	},
	// gemma-neutral is the same market-neutral strategy as "neutral" but pinned to the
	// local gemma3:4b model (vs the fleet-default qwen2.5:7b), for an evening model A/B
	// on the per-persona scorecard. Provider stays empty ⇒ local Ollama; the Model
	// override routes just this persona to gemma3:4b. gemma3:4b is deliberately light
	// (~3.3 GB) so it can share memory with the qwen fleet without the contention that
	// forced llama3.1:8b to run solo. Distinct id ⇒ its own heartbeat/group/collection/stats.
	"gemma-neutral": {
		ID: "gemma-neutral", Label: "Gemma Neutral", Stance: StanceNeutral,
		RiskTier: RiskBalanced, Threshold: 0.50,
		Nudge: "You are market-neutral: take whichever side the evidence implies, without directional bias.",
		Model: "gemma3:4b",
	},
	// gemma-bull is the same aggressive-bull strategy as "bull" but pinned to the local
	// gemma3:4b model (vs the fleet-default qwen2.5:7b), the bull-side twin of gemma-neutral
	// for the same evening model A/B on the per-persona scorecard. Provider stays empty ⇒
	// local Ollama; the Model override routes just this persona to gemma3:4b (~3.3 GB, light
	// enough to share memory with the qwen fleet). Distinct id ⇒ its own heartbeat/group/
	// collection/stats.
	"gemma-bull": {
		ID: "gemma-bull", Label: "Gemma Bull", Stance: StanceBullish,
		RiskTier: RiskAggressive, Threshold: 0.45,
		Nudge: "You are aggressively bullish: favor BUY/YES, size up on upside catalysts, and act on thinner conviction.",
		Model: "gemma3:4b",
	},
	"conservative-neutral": {
		ID: "conservative-neutral", Label: "Conservative Neutral", Stance: StanceNeutral,
		RiskTier: RiskConservative, Threshold: 0.65,
		Nudge: "You are conservative and market-neutral: demand robust, highly-confirming evidence on either side; keep size modest and selectivity high.",
	},
	"aggressive-neutral": {
		ID: "aggressive-neutral", Label: "Aggressive Neutral", Stance: StanceNeutral,
		RiskTier: RiskAggressive, Threshold: 0.40,
		Nudge: "You are aggressive and market-neutral: take substantial positions on either side, moving quickly on thinner conviction whenever a signal emerges.",
	},
}

// resolvePersona returns the persona for an id, falling back to gordon-default.
func resolvePersona(id string) Persona {
	if id == "" {
		id = "gordon-default"
	}
	if p, ok := personaRegistry[id]; ok {
		return p
	}
	log.Printf("[WARN] Unknown GORDON_PERSONA %q — using gordon-default.", id)
	return personaRegistry["gordon-default"]
}

// isDefaultPersona reports whether the id is the default (which keeps polling the
// global gekko:model_controls knob rather than running as a fixed persona).
func isDefaultPersona(id string) bool {
	return id == "" || id == "gordon-default"
}
