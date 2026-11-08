# Proposal: First-class LLM backends for Gordon

_High-level design. Turns Gordon's "Ollama, with Gemini bolted on the side" into a clean `ProposalBackend` abstraction where each backend owns its own rate limiting. Companion to the minimal fix already landed on 2026-07-08 (see the end of this doc)._

> **Status: implemented (2026-07-08).** `gordon/backend.go` defines `ProposalBackend` with `ollamaBackend`/`geminiBackend`; `OllamaClient` is now `ProposalClient` holding `primary`/`fallback` backends and generic primary counters. The `provider` branch is gone from `GenerateProposal` — rate/concurrency limiting lives inside each backend (Ollama: fleet slot + rate pacer; Gemini: RPM/RPD only), and pause is cross-cutting via `PriorityControls.Paused()`. `Provider` is a first-class persona field with `GEMINI_PERSONAS` kept as a runtime override (decision 2). The operator pause/weight knob works on any backend, rate stays per-backend (decision 1, the split). Health telemetry is backend-scoped (`backend_*`) end to end: `gordon/health.go` → `liveview/health.go` → `SystemStatus.tsx`. JSON-externalized registry (decision 3) remains a follow-up.

## The idea in one line

Make the proposal LLM a **pluggable backend** — Ollama, Gemini, or anything else — so that each backend carries its own rate/concurrency limiting instead of every persona being throttled by one Ollama-shaped budget.

## Why

The bug that started this: the `bear` persona, running on the Gemini API, was falling back to local rules on ~97% of its calls even though the Gemini key worked. The cause wasn't Gemini — it was that Gordon has **one rate pacer that gates every provider with a budget derived from local-Ollama capacity**.

The deeper reason is structural. `OllamaClient` (`gordon/rag.go`) is a god-object: it holds Ollama's fields (`baseURL`, `model`, `httpClient`) *and* Gemini's (`geminiKey`, `geminiBaseURL`, `geminiLimiter`), plus a `provider` branch inside `GenerateProposal`. Because "which backend" is a runtime `if`, the three rate/concurrency controls got wired inconsistently:

| Control | Where | Ollama | Gemini | Notes |
|---|---|---|---|---|
| `priority` pacer (`PriorityControls`) | `rag.go` §2a | ✅ | ✅ *(bug)* | Budget = `OllamaMaxConcurrent ÷ proposalTimeout`. Gemini inherited an Ollama-capacity rate it has no reason to obey. |
| `genLimiter` concurrency slot (`LLMLimiter`) | `rag.go` §2b | ✅ | ❌ *(correct)* | Already backend-aware — the pattern to copy. |
| `geminiLimiter` (RPM/RPD) | inside `queryGeminiLLM` | ❌ | ✅ | The *right* limiter for Gemini, but bolted on **beside** the pacer, not **instead of** it. |

The name lies, too: "run persona `bear` on Gemini" is really "`bear`'s backend = Gemini," but the code makes it feel like an Ollama client that sometimes phones an API. Adding a third backend (an OpenAI-compatible endpoint, a second local runtime) today means threading another `provider` branch through every one of these controls.

## What "a backend" is

A backend is the thing that turns a prompt into model text, **and** the thing that knows how fast it's allowed to do that. Concretely:

```go
type ProposalBackend interface {
    Generate(ctx context.Context, prompt string) (string, error) // model bound at construction
    Stats() map[string]any                                       // backend-scoped telemetry
    Name() string                                                // "ollama" | "gemini" | ...
}
```

The key property is that **rate limiting lives inside the backend**, not in the shared call path:

- **`ollamaBackend`** owns `baseURL`, `model`, `httpClient`, the fleet-wide concurrency `LLMLimiter`, and the Ollama-capacity rate pacer. Its `Generate` acquires a slot and paces itself, then calls Ollama.
- **`geminiBackend`** owns key/baseURL/model and the `GeminiRateLimiter` (RPM/RPD). No concurrency slot, no Ollama pacer — it self-paces to the API's real limits.

Once this holds, "configurable backend ⇒ configurable rate limiting" falls out for free: a new backend is one new `Generate` implementation with whatever limiter it needs, and `GenerateProposal` never learns its name.

## What Gordon looks like after

`OllamaClient` becomes `ProposalClient`, holding:

- `primary ProposalBackend` — the persona's chosen backend.
- `fallback ProposalBackend` — **always Ollama** (a hosted outage or rate-cap degrades to a real small local model, not keyword rules).
- embed config — **always Ollama** (`nomic-embed-text`); the Qdrant collections are built in that vector space, so embeddings never move regardless of proposal backend.
- persona nudge, timeouts, counters.

`GenerateProposal` loses the `provider` branch entirely:

```
proposal, err = oc.primary.Generate(ctx, systemPrompt)
if err != nil {
    proposal, err = oc.fallback.Generate(ctx, systemPrompt)  // logs the primary error first
}
```

The `geminiPrimaryOK/Err/Fallbacks` atomics become generic `primaryOK/Err/fallbacks` on the client, fed by whatever the primary backend is.

## Configuration & personas

Today the backend choice is a runtime A/B hack: `GEMINI_PERSONAS` (CSV env, resolved in `gordon/main.go` ~242) flips a matching persona's provider to Gemini. `Persona.Provider` exists but is "usually left unset … chosen at runtime."

After the refactor, `Provider` (plus `Model`, and ideally per-backend rate knobs like temperature/RPM) is a **normal per-persona registry field** in `personas.go`, so any persona picks its backend without a separate env toggle. `GEMINI_PERSONAS` either stays as an optional convenience override or is dropped once the registry is the single source of truth (open decision below).

## Telemetry

The health path keys off `provider == gemini` today (`gordon/health.go` emits `gemini_*`; `gordon/liveview/health.go` ~155–249 aggregates them; `SystemStatus.tsx` renders the "External APIs" card). Generalize these to **backend-scoped** keys (`backend`, `backend_calls_today`, `backend_primary_ok`, …) so the card works for any hosted backend, not just Gemini. Keep the same card UX; just stop hardcoding the word "gemini" in the wire format.

## Open decisions

1. **The operator promote/demote/pause knob.** Today it's fused with the rate pacer, so skipping the pacer for a backend also loses the UI control. Cleanest split: keep *pause/weight* as a cross-cutting persona policy (works on any backend) but make the *rate budget* per-backend. Decide whether Gemini personas should stay controllable from the UI (yes ⇒ do the split) or whether that knob is Ollama-only (simpler).
2. **`GEMINI_PERSONAS`** — keep as a convenience override, or drop once `Provider` is a first-class registry field?
3. **JSON-externalized persona registry** — `personas.go` already anticipates this. Pairs naturally with making backend a per-persona field (edit backends without a rebuild), but is a separable follow-up.

## Scope & effort

Roughly one focused day. Files touched:

1. **New `gordon/backend.go`** — the interface + `ollamaBackend`/`geminiBackend`; absorbs `queryOllamaLLM`/`generateWithTimeout` and `queryGeminiLLM`.
2. **`gordon/rag.go`** — strip Gemini fields, rename `OllamaClient`→`ProposalClient`, rewrite `GenerateProposal` §2a/2b/3, generalize counters.
3. **`gordon/gemini.go`** — `queryGeminiLLM` moves into `geminiBackend`; `GeminiRateLimiter`/`readSecret`/`splitCSV` stay.
4. **`gordon/priority.go`** — decouple its default rate from the Ollama estimate; wire it under the Ollama backend (and, per decision 1, keep pause cross-cutting).
5. **`gordon/main.go`** — replace the provider `if/else` (lines ~242–308) with "build the persona's backend from its config."
6. **`gordon/personas.go`** — `Provider`/`Model`/rate knobs as first-class fields.
7. **`gordon/health.go` + `gordon/liveview/health.go`** — `gemini_*` → backend-scoped keys.
8. **`frontend/src/components/SystemStatus.tsx`** — consume the generalized keys.
9. **Tests** — `gemini_test.go` → backend tests; add an `ollamaBackend` test; update `liveview/health_test.go`.

## What already shipped (the minimal fix, 2026-07-08)

Ahead of this refactor, a small unblock landed so the live A/B isn't strangled in the meantime:

- **`gordon/rag.go`** — the `priority.Wait` pacer is now guarded `if oc.provider != ProviderGemini` (mirroring the fleet-slot skip); Gemini self-paces via `geminiLimiter` only.
- **`gordon/rag.go`** — a `[GEMINI WARN] primary call failed: …` log line before the fallback overwrites the error, so a Gemini outage/rate-cap is visible in the logs (a live 429 storm was invisible until curled by hand).
- **`run.sh`** — `GORDON_FLEET_RATE` default `0.2` (the auto-estimate `concurrency ÷ timeout` = `1/90s` assumed every call ate the full 90s deadline; a real `qwen2.5:7b` call measured ~2.9s). Drops the Ollama personas from ~270s to ~15s between LLM calls.

These are tactical patches to exactly the seams this proposal removes. The refactor makes the guard and the per-backend rate unnecessary by construction — the pacer simply won't exist on the Gemini backend.
