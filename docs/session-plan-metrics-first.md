# Session plan: Metrics-first ("numbers over vibes")

_Consolidated 2026-07-08 from: the open backlog (`gekko_improvement_tasks.md`), `docs/breen-intelligence-proposal.md`, `docs/data-sourcing-and-credibility-proposal.md`, and operator notes ("rely more on numbers, shrink LLM calls, give the decision-maker a plethora of metrics"). Scoped for **one Opus/Sonnet session**, ending in a **fresh $10k dry run**._

## Framing correction (matters for scope)

**Breen makes zero LLM calls** — it is fully mechanical (risk gate, consensus, stops). The only LLM prompt in the system is Gordon's proposal prompt (`gordon/rag.go` ~159–232). So "give breen more metrics" splits into two real workstreams:

1. **Breen (mechanical):** compute/persist the metrics so its gates and future sizing act on measured numbers — and so the *operator* decides from numbers. This is exactly `breen-intelligence-proposal.md` Phase 0 + the "Full per-persona algo-trading metrics" backlog item.
2. **Gordon (LLM):** inject those metrics into the proposal prompt — the "plethora of metrics" note lands here — and add numeric pre-gates so the LLM is called *less*, only when a trade is actually possible.

## The session, in priority order

Each block is independently shippable; if time runs out, everything above the cut still stands alone. All new behavior behind env knobs, defaults = current behavior, tests per block (house style).

### 1. Sim honesty + cost/excursion bookkeeping (breen) — Phase 0 core, ~2h

- **Settlement prior fix**: `settleExpired` resolves YES with `rand < entryPrice` even when the mark has cratered — use the freshest mark (`resolveMarkFor` tiering) instead. Gate: `SETTLE_PRIOR=mark|entry`, default `mark`. One line of honesty that de-noises every metric below, the allocator, Lou, and the RAG feedback.
- **MAE/MFE per trade**: track `worstMark`/`bestMark` in the existing 30s mark loop; stamp `MaxAdverseExcursion`/`MaxFavorableExcursion` (% of entry) on `ClosedTrade`. Free (marks already fetched); prerequisite for any future exit intelligence.
- **Cost accounting**: `SpreadCostUSD` (half-spread × size from the book breen fetched at fill) + `FeeUSD` (Kalshi fee formula, `KALSHI_FEE_MODEL`, default off) on `ClosedTrade`; surface "P&L net of costs".

### 2. Clip-to-headroom sizing (breen) — required for the $10k run, ~1h

`riskGate` is reject-only; gordon sizes (rules fallback 800–1800 contracts) assume a $100k book. At $10k the 2% family cap = **$200**, so nearly every BUY would be rejected and the run would produce no data. Add `RISK_CLIP_TO_CAP` (default **on** for the 10k run, logged as `[SIZE]`): when a BUY exceeds remaining family/category/deployed headroom, **shrink it to fit** (floor: min 1 contract & some notional floor, else reject as today). This is a mini version of breen-intelligence Phase 2's sizing layer — shrink-only, never sizes up.

### 3. Full metric set per persona + book (breen) — backlog item, ~2–3h

- **Expectancy** first (derivable from closed trades now): `avgWin·winRate − avgLoss·lossRate`, capital-weighted, into `PersonaStat` + `gekko:persona_stats` + `PersonaScorecard.tsx`.
- **Per-persona equity series**: periodic per-persona realized-PnL/NAV sample to a capped Redis series (`gekko:persona_equity:<persona>`), rebuilt/backfilled from `ClosedTrade`s on restart → **max drawdown, Sharpe, Sortino** (downside-only dev; rf=0; annualized). Gate behind min sample count, UI shows "—" until then.
- **Book-level (the "breen scorecard")**: same metrics for the whole account — Sharpe/Sortino/expectancy/profit factor/win rate by entry band, alongside the existing `max_drawdown_pct`. The equity curve already exists account-wide, so this is cheap.
- **Calibration table** (Phase 0's display-only piece): bin stated confidence × persona → realized win rate, publish `gekko:calibration` + small System-tab table. Data for the *next* session's calibrated-conviction work.
- Defer **alpha/beta** (needs benchmark regression — SPY series exists, but it's the least actionable; cut first if tight).

### 4. Quant dossier in the proposal prompt (gordon) — the "plethora of metrics" ask, ~2h

New section in `buildProposalPrompt` between LESSONS and CURRENT EVENT, built from Redis snapshots gordon can already reach (same mtime/TTL-cache pattern as `LessonBook`; fail-safe: absent ⇒ no section):

- **Your track record** (per emitting persona): trades, win rate, expectancy, return on capital, Sharpe/Sortino when available.
- **Book state**: NAV, current & max drawdown, deployed %, open position count, distance to drawdown halt.
- **This contract's family**: current exposure vs cap, realized family PnL vs loss limit, cooldown state, recent trade count.
- **Market micro** (extend the existing LIVE MARKET block): spread, required edge (`MIN_EDGE` + spread), hours to expiry.
- Keep it compact (~10 lines, numbers not prose) — it must not blow the token budget that the Ollama queue is sensitive to.

### 5. Pre-LLM numeric veto (gordon) — "shrink LLM calls", ~1–2h

Today gordon burns an Ollama slot generating proposals that breen deterministically rejects. Move the cheap, deterministic checks **before** `GenerateProposal` in `processEvent`:

- near-expiry skip (verify it already runs pre-LLM; if not, hoist),
- no live book / spread over the entry-gate cap (data is in Redis orderbook keys),
- family in cooldown / at loss limit / drawdown halt active — needs breen to publish a tiny risk-state blob (`gekko:risk_state`: halted flag + per-family cooldown-until & loss-limit-breached), refreshed on its existing sync cadence.

Every skip = one fewer LLM call → shorter Ollama queue → lower fallback ratio → the calls that remain are higher quality. Log as `[PREGATE]` with a counter in `/healthz` so the MODELS tab can show calls saved.

**Cut line if the session runs long:** 5 → 4-partial (persona track record only) → 3's alpha/beta already cut. 1 and 2 are non-negotiable (run correctness).

## Fresh $10k dry run

```bash
STARTING_CAPITAL=10000 \
PER_CONTRACT_LOSS_LIMIT=30 \      # was $300 = 0.3% of 100k; keep the ratio, not the dollars
STOP_LOSS_USD=30 \                # same rescale
CATEGORY_LOSS_LIMIT=50 \          # was $500
MAX_TOTAL_DEPLOYED=6000 \         # was $60k (60%)
MAX_CONTRACT_EXPOSURE=1500 \      # was $15k; pct cap ($200) binds anyway
caffeinate -is ./run.sh
```

- Percent-based knobs (`MAX_CONTRACT_EXPOSURE_PCT` 2%, `MAX_CATEGORY_EXPOSURE_PCT` 6%, `MAX_DRAWDOWN_HALT_PCT` 10%) scale automatically — leave them.
- **Do not start the 10k run without block 2 (clip-to-headroom)** — reject-only + 100k-tuned sizes at a $200 family cap ⇒ near-zero fills.
- Reset via the System-tab export-then-reset (bundle the current run first). Lou pass on the archived run before restart; note `runs/lou/overrides.env` already ratchets `MAX_ENTRY_PRICE` to 0.65.
- Rebuild everything — the live stack still runs pre-refactor binaries (backend refactor, UI phases 2–3, persona badges all await a restart).

## Data sources (researched 2026-07-08 — NEXT session, not this one)

**Sequencing rule:** land credibility Phase 1 (`docs/data-sourcing-and-credibility-proposal.md`) in the same session as any new source — today `SourceID` never reaches `compositeConfidence`, so a new noisy source would be one more oracle, exactly the X problem again.

| Source | Cost | Fit |
|---|---|---|
| **Reddit** | **Free** non-commercial tier: 100 queries/min per OAuth client. Commercial: $0.24/1k calls, manual approval, $12k/mo enterprise — irrelevant at our scale/purpose. | Best next source. 100 QPM dwarfs our X budget (25 tweets/~36min). `RedditSource` in kahuna polling /new+/hot of ~6–10 subs (wallstreetbets, stocks, economics, politics, CryptoCurrency, geopolitics) ≈ 10% of quota. Credibility tier 4 (same as X). |
| **Finnhub** | Free: 60 calls/min (news + sentiment endpoints); paid from $80/mo (not needed) | Best free *news API* tier. Tier 2–3 credibility. |
| **FRED / SEC EDGAR / BLS / Treasury** | Free (keyed or keyless) | The credibility-proposal Phase 3 tier-1 official feeds; macro markets are our biggest category. |
| **GDELT** | Free | Global news metadata, 15-min updates; broad but noisy — tier 3, only with corroboration (Phase 2). |
| **Alpha Vantage news / Marketaux** | Free tiers thin (AV ~25 req/day; Marketaux similar) | Skip unless a specific gap appears. |

Recommended next-session bundle: **credibility Phase 1 + RedditSource + FRED/EDGAR** — all $0/mo.

## Explicitly deferred (tracked elsewhere)

Cloud LLM overflow to Haiku (backlog, decisions locked), Gordon rate-limiter, calibrated conviction / Kelly sizing / trailing stops (breen-intelligence Phases 1–3 — consume this session's data), hosted-backend budget persistence, per-contract dashboard, Lou cadence hook + circuit breaker.
