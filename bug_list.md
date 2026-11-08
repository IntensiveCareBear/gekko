# Gekko Systems Bug List

This document consolidates all the bugs and architectural improvements identified across the `gekko-1` codebase during the code review.

## 1. Frontend React Performance & State Bugs (Critical)

> [!WARNING]
> These issues currently cause the frontend `npm run lint` suite to fail with 23 errors. They lead to component remounting and cascading state re-renders.

- **[BUG] Components created inside render functions:**
  - **Location:** `src/components/PortfolioAnalytics.tsx`
  - **Issue:** `DonutTooltip` and `CenterLabel` are defined *inside* the `PortfolioAnalytics` render block. React reconstructs them from scratch on every single render cycle, wiping state and destroying DOM performance.
  - **Fix:** Extract these components to the top level of the file and pass required data via props.

- **[BUG] Synchronous `setState` inside `useEffect`:**
  - **Location:** `PortfolioAnalytics.tsx`, `SystemStatus.tsx`, `SystemTelemetry.tsx`
  - **Issue:** State setters like `fetchStatus()` and `setConnectionStatus('CONNECTING')` are being called synchronously within `useEffect` bodies without an asynchronous wrapper or check, triggering cascading re-renders.
  - **Fix:** Move direct sync calls out of effects or wrap them in proper asynchronous fetch logic to prevent infinite render loops.

## 2. Gordon Quantitative Engine Bugs

- **[BUG] RNG Seeding Concurrency Hazard:**
  - **Location:** `gordon/engine.go` (in `Evaluate` and `generateMockBook`), `gordon/main.go` (in `startSimulatedWorker`)
  - **Issue:** Using `rand.New(rand.NewSource(time.Now().UnixNano()))` locally inside a function call. In a highly concurrent system, if two goroutines hit this line at the exact same nanosecond, they receive identical RNG streams. This causes multiple trade evaluations to produce the exact same Iceberg chunk sizes and timing delays.
  - **Fix:** Use the global `math/rand` instance (e.g. `rand.Intn()`) which is concurrency-safe as of Go 1.20, or initialize a single global RNG with a `sync.Mutex`.

- **[BUG] Silent Data Loss in RAG Vector Upserts:**
  - **Location:** `gordon/rag.go` (in `RunRAGFeedbackConsumer`), `gordon/main.go` (in `processEvent`)
  - **Issue:** When a trade generates a new proposal or when realized RAG feedback is ingested, the engine launches a fire-and-forget goroutine to embed the text and call `UpsertEvent` on Qdrant. If the database is briefly unreachable, the event is permanently discarded without logging or retry, meaning the RAG memory fails to update.
  - **Fix:** Add a short retry loop with exponential backoff before discarding, or push failed embeddings to a Redis Dead Letter Queue (DLQ).

- **[IMPROVEMENT] Naive Headline Sentiment Scoring:**
  - **Location:** `gordon/signals.go` (`headlineSentiment`)
  - **Issue:** The sentiment scorer uses a very small, hardcoded lexicon of strings (e.g. "surge", "crash"). This fails to capture complex contextual phrasing (e.g. "did not surge").
  - **Fix:** Replace the lexicon string matcher with a lightweight local LLM check or a specialized sentiment API if one becomes available.

- **[IMPROVEMENT] API Key Fallback Pre-Check:**
  - **Location:** `gordon/main.go`
  - **Issue:** The check to ensure `GEMINI_API_KEY` exists is done globally for the `provider`, but evaluating whether a Persona *should* use Gemini happens dynamically in a loop over `GeminiPersonas`.
  - **Fix:** Shift the provider selection logic entirely inside `personas.go` where `resolvePersona` is evaluated.
