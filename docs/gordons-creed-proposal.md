# Gordon's creed: activity-triggered, holdings-aware proposals

_Scoped 2026-07-08 from operator direction after the flatline forensics:_

> **The creed:** "We see activity on the markets. We look through our ingested
> information — or ingest on demand if nothing is found locally. We make a proposal
> and send it to breen." — that is any gordon's creed.

Plus two hard rules from the same session: **sell means sell** (we can't sell what we
don't hold) and **gordons must have insight into our holdings**.

## Where the pipeline is today (and why it's backwards)

The trigger is a **news event**, not market activity. Kahuna pushes every RSS/Twitter/
Finnhub item onto `kahuna.ingest.public`; each gordon's `processEvent` (gordon/main.go)
then runs the whole chain per headline: embed → match to a market → RAG → **LLM call** →
edge/floor/skew gates → proposal. Consequences observed in the 2026-07-08 run:

- The LLM burns a slot on every headline even when no market is moving (overnight, most
  matched books are static — the run went 1h+ with zero passable entries while personas
  kept generating).
- Personas are blind to the portfolio. Bear spent the evening emitting SELLs for
  contracts nobody held; every one died at breen as "exit signal but no active position
  held". Its entire signal stream was decorative.
- Market activity (which marketdata already measures — `momentum:<ticker>` trend/vol
  surge on the 5s rotation) is only a *confidence enrichment* after the fact, never a
  reason to look at a market.

### Shipped ahead of this scope (2026-07-08, uncommitted)

- **Mid-band gate fixed**: judged on strongest raw contributor confidence
  (`midBandConfidence`, breen/tracker.go) instead of the post-weight consensus number,
  and default `MID_BAND_MIN_CONFIDENCE` 0.75 → 0.70. The floor/ceiling gates confine all
  entries to the mid-band, so the old wiring required ≈0.78 raw conviction — zero trades
  passed in a full run.
- **Sell means sell (drop half)**: gordon drops SELL proposals for families with no open
  position, pre-consensus (`[HOLDINGS]` log line, gordon/main.go). Authoritative-only:
  passes through if no risk-state snapshot decoded, so a breen outage can't suppress
  real exits. The *prompt* half (tell the LLM what it may sell) is Phase 1 below.

## Phase 1 — Holdings insight (~1–2h)

Give every gordon the book, so exits become deliberate and SELL hallucinations stop at
the prompt instead of at a gate.

- **Breen publishes holdings**: extend the `gekko:risk_state` blob (breen/riskstate.go)
  — or a sibling `gekko:holdings` key on the same sync cadence — with per-position
  detail breen already computes: contract, entry price, size, freshest mark, unrealized
  PnL %, age, persona contributors. All fields exist in `positions` + `markCache`.
- **Gordon prompt section**: new `YOUR HOLDINGS` block in `buildProposalPrompt`
  (gordon/rag.go), same fail-safe TTL-cache pattern as the lesson book / quant dossier:
  absent key ⇒ no section. Compact, numbers not prose (~1 line per position).
- **Prompt rule**: "SELL is only valid for contracts listed under YOUR HOLDINGS; there
  is no shorting." Belt (prompt) + suspenders (the shipped drop) + backstop (breen).
- Note for later, explicitly **out of scope**: expressing a bearish view on an unheld
  market = buying the NO side. That is a BUY of the opposite contract, not a SELL, and
  needs venue support in marketdata first.

## Phase 2 — Activity as the trigger (~3–4h, the creed inversion) — **built 2026-10-15, on in `relaunch.sh`**

> `marketdata/activity.go` detects the move (|trend| >= `ACTIVITY_TREND_PCT` 8% AND >= `ACTIVITY_MIN_MOVE` 3c, or top-of-book size >= `ACTIVITY_VOL_SURGE` 3x its window average, 10 min per-ticker debounce) and publishes to the Redis channel `gekko:market_activity`. `gordon/activity.go` (`ACTIVITY_TRIGGER=true`) subscribes, admits one evaluation per contract family per `ACTIVITY_FAMILY_COOLDOWN` (10m), and feeds a synthetic event through `processParsedEvent` with the market pre-matched (the embedding match and entity-verify are skipped; the Qdrant lookup of what has already been ingested about the market is the existing RAG step). Queue of `ACTIVITY_QUEUE` (16); a saturated model drops events rather than backlogging. Phase 3 (ingest on demand) and 4 (quiet-market LLM budget) are not built. Measure first: `[ACTIVITY]` log lines show seen/admitted/dropped counts.

- **Activity detector (marketdata)**: the momentum tracker already keeps rolling mids
  and top-of-book volume per ticker. Add threshold crossings — |trend| ≥ X%, vol surge ≥
  Y×, spread collapse/appearance of a book — and publish an `ActivityEvent` (ticker,
  kind, magnitude, book snapshot, ts) to `gekko:market_activity` (Redis stream or
  pub/sub, matching house style).
- **Gordon consumes activity as a first-class trigger** alongside news events: a new
  goroutine feeding the same `processEvent` funnel, with a synthetic event whose
  "content" is the market title + activity description, and the matched ticker already
  known (skips the match step, saves the embed).
- **Look through ingested information first**: on activity, reverse-lookup Qdrant —
  embed the market title (already done once for catalog matching; reuse the vector) and
  search `gordon_events` for recent relevant items. Found ⇒ they become the RAG context
  for the proposal.
- **Per-family evaluation cooldown**: one activity-triggered evaluation per family per N
  minutes (env, default ~10m) so a churning book doesn't burn the LLM budget. Activity
  triggers also pass the existing pre-LLM veto (`[PREGATE]`).

## Phase 3 — Ingest on demand (~2–3h) — **built 2026-10-15, on in `relaunch.sh`**

> Differences from the sketch below. (1) There was no ingested-news store to "look through": the Qdrant collection holds past proposals and trade outcomes, not headlines. Gordon now keeps a rolling buffer (`NEWS_BUFFER`, 500) of every ordinary news event it embeds, and an activity look searches it (`ONDEMAND_MIN_SIM` 0.73, was 0.6 until 2026-10-19, `NEWS_MAX_AGE` 6h). (2) Kahuna replies over Redis (`gekko:ingest_requests` -> list `gekko:ingest_reply:<id>`), not onto the Kafka topic: every item on Kafka would be its own LLM evaluation, the cost this avoids. (3) Fetches are Google News RSS only (keywords built from the market title, <=`ONDEMAND_INGEST_PER_MIN` 6 per minute, 10 min cache), so a request can never name a URL. Headlines are sanitized and appended to the event text as a labelled untrusted block. On a miss gordon skips (`ONDEMAND_ON_MISS=skip`, default) or evaluates without news (`proceed`). The "confidence haircut" for proceed is not built. Files: `kahuna/ingest/ondemand.go`, `gordon/ondemand.go`.

> **News store (added 2026-10-15).** The Phase 3 buffer was memory-only, so a relaunch forgot all news. `gordon/newsstore.go` persists every ordinary headline gordon embeds to a Qdrant collection (`NEWS_COLLECTION`, default `gordon_news`, 768-dim cosine, `ts_ms` integer index, stable per-headline ids so repeats overwrite). Writes are batched (50 / 2s) and best effort; reads merge Qdrant (server-side age and similarity filter) with the memory buffer, and fall back to memory alone if Qdrant errors. Retention `NEWS_RETENTION` 72h, swept hourly. `NEWS_STORE=false` disables it. First live check: 2026-10-15 22:16, an activity event on KXBOZPERMITS fetched one headline on demand in about a second and the model declined it.

### Original sketch

- **Request channel**: gordon publishes `gekko:ingest_requests` (ticker, market title,
  request id, deadline) when the Phase 2 local lookup comes back empty.
- **Kahuna serves it**: subscribe, build a targeted query from the market title (the
  Google News RSS search pattern already exists for the standing queries; Finnhub/FRED
  where the category fits), publish results onto the normal ingest topic tagged with the
  request id — so everything downstream (dedupe, embedding, RAG feedback) works
  unchanged.
- **Bounded wait in gordon**: wait ≤ N seconds (env, ~45s) for tagged items; on timeout
  either proceed flagged "no supporting news found" with a confidence haircut, or skip
  (env choice, default skip — no information, no trade).

## Phase 4 — Quiet-market LLM budget (~1h)

- News events remain a trigger, but gate the LLM call on the matched market being
  *tradeable and awake*: live two-sided book, and either fresh activity or spread inside
  the entry-gate cap. Otherwise log `[QUIET]` + counter in `/healthz` (MODELS tab shows
  calls saved, same as `[PREGATE]`).
- Net effect with Phase 2: LLM calls track *market motion × news*, not raw feed volume.
  Overnight the fleet goes near-silent instead of hallucinating SELLs — which is exactly
  what the flatline should look like when nothing is moving.

## Order & cut line

1 → 2 → 4 → 3. Phase 1 is prompt-plumbing with immediate effect on exit quality; Phase 2
is the creed itself; Phase 4 is cheap once 2's activity signal exists; Phase 3 is the
most new surface (kahuna request path) and stands alone if cut. All new behavior behind
env knobs, defaults preserving current behavior; tests per block (house style).
