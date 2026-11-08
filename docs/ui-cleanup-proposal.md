# UI Cleanup Proposal — consolidate knobs and de-duplicate metrics

*2026-07-06 — original proposal. Phase 1 shipped 2026-07-06 (CONTROLS tab,
dead knobs removed, telemetry de-duped, nav supervision badge).*
*2026-07-07 — detailed Phase 2/3 plan appended below, updated for code drift
since Phase 1 (GordonPriority knob on MODELS, Gemini persona provider,
external-API health cards on SYSTEM).*
*2026-07-08 — Phase 2 and Phase 3 shipped (all items except the optional tab
renames). Max slippage is now a real fleet-wide knob in `/api/controls/model`
(static personas track only that field via slipOnly RedisControls);
GordonPriority moved to CONTROLS with read-only weight badges on MODELS;
SYSTEM's gordon card keeps only aggregate persona stats; `PERSONA_LABEL`
extracted to `src/data/personas.ts`. Requires a gordon + liveview rebuild —
until then the old liveview drops the slippage field on POST and everything
falls back to the 3% default (safe).*

## Problem

Controls ("knobs") and metrics are scattered across tabs, some knobs are dead
(they control nothing), and several metrics are rendered in two or three places.

### Where the knobs live today

| Knob | Tab | Backed by |
|---|---|---|
| Market stance / Risk appetite / Selectivity (`ModelControls`) | HOME | `/api/controls/model` (real) |
| Supervision mode (supervised/auto) | SYSTEM | `/api/controls/toggle` (real) |
| WET/DRY toggle | SYSTEM | **nothing — localStorage only** |
| RAG context depth slider | SYSTEM | **nothing — localStorage only** |
| Max slippage slider | SYSTEM | **display-only** (colors slippage text in Strategy Stream; Gordon's real `max_slippage_pct` is a backend config, not wired to this) |
| Theme selector | SYSTEM | localStorage (fine, but buried under ops content) |
| Paper-run reset / CSV exports | SYSTEM | real |
| Approve/deny proposal | STRATEGY STREAM | real (contextual — should stay) |

So: real trading knobs are split between HOME and SYSTEM, and half the
"Executive Controls" section is dead weight that *looks* load-bearing.

### Where the metrics duplicate

- **SystemTelemetry** (latency breakdown, EPS, proposals ingested) is rendered on
  **both** SYSTEM and MODELS.
- **Account snapshot** (NAV, cash, PnL, drawdown, open positions) on SYSTEM
  duplicates the PnL hero on HOME.
- **Persona breakdowns** appear in three places: HOME (`PersonaScorecard`),
  SYSTEM (persona list inside the gordon health card), MODELS (per-persona
  fallback ratios).
- SYSTEM's headline KPI cards ("Proposals Generated") overlap SystemTelemetry's
  "Proposals Ingested".

## Proposed end state

Each tab answers one question; all knobs live in one place.

| Tab | Question it answers | Contents |
|---|---|---|
| HOME | "How is the run doing?" | PnL hero, PersonaScorecard, equity curve, distribution, allocation, closed ledger. **Read-only** — `ModelControls` moves out. |
| STRATEGY STREAM | "What is it doing right now?" | Live feed + approve/deny (contextual actions stay). |
| **CONTROLS** (new) | "Change how it trades / manage the run" | ModelControls (from HOME) · Supervision toggle (from SYSTEM) · Paper-run reset + exports (from SYSTEM) · Appearance/theme (from SYSTEM). One tab, one save model. |
| SYSTEM | "Is the stack healthy?" | Headline pipeline KPIs, per-component health cards, LedgerAudit. Account grid shrinks to a NAV one-liner linking to HOME. |
| MODELS | "Is the LLM pipeline healthy?" | Fallback ratio + charts, concurrency limiter, per-persona fallback, and the **single** SystemTelemetry instance (it's inference latency — it belongs here). |
| DEBUG | unchanged | log console |

A small supervision-mode badge in the top nav (SUPERVISED / AUTO) keeps the one
safety-critical state visible from every tab without duplicating the toggle.

### Dead-knob resolution (decide per knob)

1. **WET/DRY** — remove. It's aspirational (no live-trading path exists); a
   fake-looking production toggle is worse than none. Reintroduce when there's a
   real wet path.
2. **RAG depth** — remove, or wire to Gordon via `/api/controls/model` alongside
   stance/risk/selectivity if it should be real. Removing is the cheap default.
3. **Max slippage** — either (a) relabel as "slippage highlight threshold" and
   move next to Appearance so it stops masquerading as a risk control, or
   (b) wire it to Gordon's `max_slippage_pct` through `/api/controls/model`.
   Option (b) is the honest version if the knob is worth keeping.

## Phasing

**Phase 1 — pure frontend reshuffle (no backend changes, ~1 sitting)**
- Add CONTROLS tab; move ModelControls, supervision toggle, run reset/exports,
  theme there.
- Delete the duplicate SystemTelemetry from SYSTEM (keep MODELS copy).
- Shrink SYSTEM's account section to a one-line NAV summary.
- Remove WET/DRY and RAG-depth knobs (git keeps them if they come back).

**Phase 2 — make remaining knobs honest (small backend touch)**
- Wire max slippage into `/api/controls/model` → Gordon, or demote it to a
  display preference. Update the Strategy Stream coloring to read the same value
  either way.

**Phase 3 — polish (optional)**
- Consolidate persona views: performance (scorecard) stays on HOME, reliability
  (reachability + fallback) stays on MODELS, drop the persona list from the
  SYSTEM gordon card.
- Rename tabs if desired (SYSTEM → HEALTH, MODELS → LLM).

## Notes / alternatives considered

- **Settings drawer instead of a CONTROLS tab** (gear icon, opens over any tab):
  nicer for flipping supervision while watching the stream, but more work and a
  new UI pattern. The nav badge covers the "visible from anywhere" need; a tab
  matches the existing pattern. Recommend the tab; drawer can come later.
- **Merging MODELS into SYSTEM**: rejected — SYSTEM is already the longest page,
  and "stack health" vs "LLM decision quality" are different audiences/moments.

---

# Phase 2/3 detail (2026-07-07 revision)

## Code drift since Phase 1

- **`GordonPriority`** (PR #4): per-gordon promote/demote/pause knobs, backed by
  `/api/controls/priority` → Redis `gekko:gordon_priority`. It was mounted on the
  **MODELS** tab — a real knob on a metrics tab, re-creating the scatter Phase 1
  removed. Phase 3 addresses placement.
- Per-persona fallback on MODELS now uses a rolling window (better; no change).
- Gemini + X/Twitter external-API health cards on SYSTEM — pure metrics,
  correctly placed; no change needed.

## Phase 2 — wire max slippage end-to-end

Today the Controls-tab slider is display-only (colors slippage text in the
Strategy Stream via App state + localStorage), while Gordon's real cap comes
from `PORTFOLIO_MAX_SLIP` (default 0.03) frozen into `RedisControls.maxSlip` at
construction ([controls.go:57](../gordon/controls.go)). The
`gekko:model_controls` blob doesn't carry slippage at all.

**Backend**
1. `gordon/liveview/main.go` `handleModelControls`: accept optional
   `max_slippage_pct` (fraction, validate to [0.001, 0.10]); write it into the
   blob; add `"max_slippage_pct":0.03` to `modelControlsDefault`.
2. `gordon/controls.go`: add `MaxSlippagePct` to the `modelControls` struct;
   `sanitize()` clamps out-of-range/zero back to the env-seeded `rc.maxSlip`;
   `ReadLimits()` returns the live value instead of the frozen one.
3. **Fleet-wide vs default-only (decision)**: non-default personas use
   `NewStaticControls` (no Redis; maxSlip hardcoded 0.03) and deliberately ignore
   the global knob. Slippage is a *risk cap*, not a personality trait, so it
   should be fleet-wide. Recommended middle path: give static personas a Redis
   client that reads **only** `max_slippage_pct` from the blob (stance/risk/
   threshold stay fixed). Fallback option if that's too invasive: knob steers the
   default gordon only, and the UI copy says so.

**Frontend**
4. `ModelControls.tsx`: add "Max slippage" as a 4th control (slider 0.1–10%,
   converting percent ↔ fraction), saved through the existing debounced POST.
5. `Controls.tsx`: delete the display-only slider; Executive Controls shrinks to
   the supervision toggle.
6. `Dashboard.tsx`: its 2.5s poll already hits `/api/controls`; also fetch
   `/api/controls/model` there and color slippage against the backend value.
   Drop the `maxSlippage` prop.
7. `App.tsx`: remove `maxSlippage` state; add `gekko_max_slippage` to the
   one-time localStorage cleanup.

Result: one slippage number, owned by the backend, steering both execution and
display. No migration concerns — the Redis blob is overwritten whole on POST,
and `sanitize()` covers old blobs without the field.

## Phase 3 — persona consolidation + knob placement

1. **Move `GordonPriority` from MODELS to CONTROLS** (below ModelControls) —
   restores "all knobs on one tab". To keep the promote/demote *context* (you
   decide based on fallback rates), add small read-only weight badges
   ("PAUSED" / "DEMOTED" / "PROMOTED") to the per-persona rows on MODELS, read
   from the same `/api/controls/priority` GET.
2. **Drop the persona list from SYSTEM's gordon health card**
   ([SystemStatus.tsx:478](../frontend/src/components/SystemStatus.tsx)) — the
   MODELS per-persona section already shows reachability dots plus windowed
   fallback; SYSTEM keeps only the aggregate gordon stats. Persona *performance*
   stays on HOME (`PersonaScorecard`).
3. **Extract the duplicated `PERSONA_LABEL` map** (currently copy-pasted in
   `PersonaScorecard`, `SystemStatus`, `SystemMetrics`) into a shared
   `src/data/personas.ts` while touching these files.
4. **Tab renames (optional, cosmetic)**: SYSTEM → HEALTH, MODELS → LLM.

Phase 3 is frontend-only except for nothing — `/api/controls/priority` GET
already returns the roster + weights the badges need.
