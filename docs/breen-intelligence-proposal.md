# Proposal: Making Breen Smarter

_Design doc, planning only — nothing here is built. Breen is the executive: it owns the account, the risk gate, consensus, stops, and settlement. Today it is a **bouncer** — a stack of static, reactive guards that reject or cap what the Gordons propose. This doc is about upgrading it to a **portfolio manager**: an executive that sizes, calibrates, de-risks, and harvests based on what the book has actually taught it._

## The idea in one line

Every guard breen has was written after a post-mortem found a hole; the next step is a breen that **learns the holes itself** — calibrating persona confidence against realized outcomes, sizing entries by measured expectancy instead of pass/fail caps, de-risking gradually instead of slamming a halt, and managing exits on both sides (harvest winners, not just amputate losers).

## Where breen's brain is today

| Faculty | Mechanism | Character |
|---|---|---|
| Entry veto | `riskGate` (`tracker.go`): family/category exposure caps, loss limits, cooldown, drawdown halt, mid-band conviction gate | Static thresholds from env; binary allow/deny |
| Fill quality | `entryBookBlock`: live two-sided book, freshness, spread cap | Static; no fee/impact model |
| Aggregation | `ConsensusEngine` (`consensus.go`): persona-weight × confidence, 0.5–2.0× agreement multiplier | Trusts stated confidence at face value; BUY-only window |
| Persona learning | `AdaptiveAllocator` (`allocator.go`): EWMA return score → weight, bench kill-switch | Learns *who* to trust, not *when* their confidence means anything |
| Exits | `checkStopLosses` + family `LOSS_LIMIT` closes + `forceExitNearExpiry` (`account.go`) | Entirely defensive — no take-profit, no trailing, winners are never harvested |
| Drawdown | `MaxDrawdownHaltPct`: all entries off below the line | Binary cliff; nothing between 100% risk and 0% |
| Sim settlement | `settleExpired`: `rand.Float64() < pos.EntryPrice` when no real resolution | Prior uses the **entry** price even when the market has moved since — feeds noise into every learning loop downstream |

The pattern: each guard is a hand-written lesson (this file's git history *is* the training log). Lou (`docs/lou-learning-loop-proposal.md`) automated extracting lessons offline; this proposal is about breen **consuming evidence at decision time**.

## The gaps, ranked by expected P&L leverage

1. **Breen never sizes, only vetoes.** A proposal either executes at Gordon/consensus size or is rejected. There is no notion of "this is a good trade, take half," even though `PersonaScore`/`ClosedTrade` history contains everything needed to estimate expectancy per persona × price band.
2. **Confidence is uncalibrated.** `Confidence` gates the mid-band and weights consensus, but nobody checks whether a persona's 0.8 historically wins more than its 0.5. LLM-stated confidence is notoriously miscalibrated; the mid-band gate is only as good as the number it reads.
3. **Exits are one-sided.** The 2026-07-07 forensics showed `DRY_CLOSED` averaging −$4/trade (pure spread cost) while winners either drift to expiry or get force-exited. There is no mechanism to lock in an up-move. We don't even *record* how far each trade ran for/against us (MAE/MFE), so exit policy can't be tuned from evidence.
4. **The drawdown breaker is a cliff.** At `MaxDrawdownHaltPct` everything stops; at one basis point above it, full size. A book bleeding toward the halt keeps betting full-size all the way down.
5. **Costs aren't in the ledger.** No Kalshi fee model, and spread cost (fill at ask, mark at mid) is invisible — it just shows up as instant unrealized loss. Every expectancy estimate downstream (allocator, Lou, this proposal's sizing) is biased by uncounted costs.
6. **Correlation is a keyword table.** `categoryKeywords` is a hand-grown list with known collisions (DASH) and known omissions (LINK, ADA deliberately excluded). Realized mark co-movement is sitting in the equity/mark history unused.
7. **The sim settlement prior is stale.** Resolving YES with probability = *entry* price means a position that cratered from 0.60 to 0.10 still settles YES 60% of the time. This flatters exactly the trades the guards exist to kill, and pollutes the RAG feedback, the allocator, and Lou.

## Proposal, in phases

_Everything defaults **off** (or to current behavior) behind env knobs, same discipline as the risk guards. Each phase is independently shippable and testable; later phases consume the data the earlier ones start recording._

### Phase 0 — measure first (no behavior change, ~half day)

The cheapest intelligence is bookkeeping we're not doing:

- **MAE/MFE per trade.** In the settlement/stop loop (already marking every open position each 30s tick), track per-position `worstMark`/`bestMark`; stamp `MaxAdverseExcursion`/`MaxFavorableExcursion` (as % of entry) onto `ClosedTrade`. This is the dataset every exit-policy question needs ("would a trailing stop at X have helped?") and it's nearly free — the marks are already fetched.
- **Cost accounting.** Add `SpreadCostUSD` (half-spread × size at fill, computed from the book breen already fetched in `ExecuteOrder`) and `FeeUSD` (Kalshi's published fee formula, `KALSHI_FEE_MODEL=true|false`) to `ClosedTrade`. Surface "P&L net of costs" in the scorecard. No gating yet — just make the invisible visible.
- **Calibration table (computed, displayed, unused).** From `PersonaScores` + the `Confidence` each contributor stamped (already captured in `ContributorProposal.Confidence`), bin predicted confidence (e.g. 5 bins) × persona → realized win rate, with the same n₀-shrinkage trick `personaWeights` uses. Publish to Redis (`gekko:calibration`) and a small System-tab table. Watching it for a run tells us how miscalibrated the fleet actually is before we act on it.
- **Settlement prior fix.** `settleExpired`: use the freshest available mark (`resolveMarkFor`, which already tiers live → cached → entry) as the YES probability instead of raw entry price. One line of honesty that de-noises every learning loop. _Arguably a bug fix; still gate it (`SETTLE_PRIOR=mark|entry`, default `mark`) so runs stay comparable._

### Phase 1 — calibrated conviction (~1 day)

- **`calibratedConfidence(persona, statedConf)`** — look up the Phase 0 table; with insufficient samples in a bin, fall back to stated confidence (explore, same philosophy as the allocator's `minTrades` gate).
- Feed it into the two places that consume confidence today:
  - `weightedConviction`/`consolidateProposals` — consensus agreement becomes agreement among *calibrated* beliefs. A persona that cries 0.9 and delivers 0.4 stops dominating votes without waiting for the EWMA to bench it.
  - the **mid-band gate** (`riskGate` + the at-fill re-check) — compares calibrated, not stated, confidence.
- Env: `CALIBRATION_ENABLED` (false), `CALIBRATION_MIN_SAMPLES` (20/bin). Pure functions + table lookup ⇒ easily unit-tested with synthetic trade histories.

### Phase 2 — expectancy-aware sizing + graduated de-risking (~1–2 days)

- **Sizing scalar at the gate.** After `riskGate` passes, breen scales the order instead of rubber-stamping it: `size × riskScalar × edgeScalar`.
  - `edgeScalar` = fractional Kelly, heavily capped: from calibrated win probability `p` and the binary payoff at entry price `c` (win = `(1−c)/c`, lose = 1), Kelly `f* = p − (1−p)·c/(1−c)`; use `KELLY_FRACTION` (0.25) of it, mapped into `[SIZE_SCALAR_MIN, 1.0]` (floor 0.25, never sizes *up* in v1 — consensus already owns upside multiplication). Negative Kelly ⇒ floor, not reject: the veto guards keep that job, and a small position keeps information flowing to the allocator/calibrator.
  - `riskScalar` = **graduated drawdown response**: 1.0 at `DRAWDOWN_EASE_START` (e.g. 4%), linearly down to `DRAWDOWN_EASE_FLOOR` (e.g. 0.25) at the existing halt threshold. The binary halt stays as the backstop; the ramp removes the cliff and the flapping around it.
- **Cost-aware fill gate.** With Phase 0 costs known, extend `entryBookBlock`: expected edge at fill must clear `half-spread + fee + ENTRY_MIN_NET_EDGE`. This is the executive-side twin of gordon's `MIN_EDGE` — checked against the book breen actually fills on, not the one gordon saw seconds earlier.
- Everything logs its scalars into the ledger ref / `[SIZE]` log line so a run's sizing decisions are auditable the way `[RISK]` rejections are.

### Phase 3 — exit intelligence (~1–2 days, gated on Phase 0 data)

- **Trailing stop.** Track `bestMark` (already needed for MFE); exit when price retraces `TRAIL_RETRACE_PCT` from the best mark *after* the position is up at least `TRAIL_ARM_PCT` — unarmed positions keep today's behavior, so this cannot fire on the noise the `STOP_LOSS_MIN_ABS` floor was built to ignore. Live-marks-only, same discipline as `checkStopLosses`.
- **Partial take-profit (optional, second step).** Close `TP_FRACTION` of the position at `TP_TRIGGER_PCT` gain; let the rest ride under the trailing stop. Ship only if the Phase 0 MFE distribution shows winners routinely giving back big excursions — that's an empirical question the data will answer, not a debate.
- **Tuning loop:** both knobs join Lou's auto-tune whitelist (bounded, tighten-only-ish semantics TBD) so exit policy improves from evidence the same way entry policy does.

### Phase 4 — learned correlation (~1 day, lowest priority)

- Record per-family daily mark returns (from the mark cache breen already maintains); compute rolling pairwise correlation; families exceeding `CORR_GROUP_THRESHOLD` (e.g. 0.7 over ≥N observations) get a synthetic shared category for the existing category caps. The keyword table remains as the floor/cold-start. This closes the "cross-series correlation still open" half of post-mortem #5 with data instead of more keywords.

## Guardrails

- **Off by default, bounded, logged.** Same contract as the risk guards: every new behavior behind an env knob with a conservative default, every decision visible in logs/ledger refs, every scalar clamped.
- **Never size up (v1).** The sizing layer only shrinks; upside sizing stays with consensus agreement. Removes the whole class of "runaway confidence" failure modes from the new code.
- **Explore floors everywhere.** Calibration falls back to stated confidence on thin bins; Kelly floors at a small size instead of zero — starving the feedback loops of data is how the allocator's own docs say personas get stuck.
- **Sim-honesty first.** Phase 0's settlement-prior and cost fixes come before any behavior learns from the data, so we don't calibrate against fiction.
- **Lou stays the offline brain.** Breen's new faculties are *online* and *mechanical* (tables, scalars, clamps) — pattern *discovery* remains Lou's job. Where they meet: new knobs join Lou's whitelist; nothing here duplicates Lou's analyzers.

## Tie-ins

- **Lou** (`docs/lou-learning-loop-proposal.md`) — exit knobs and sizing bounds join the auto-tune whitelist; Lou's validated lessons could later seed the expectancy table's priors.
- **Data sourcing & credibility** (`docs/data-sourcing-and-credibility-proposal.md`) — its `Credibility` term shapes confidence upstream in gordon; calibration here corrects whatever residual miscalibration survives, downstream. Complementary, not overlapping.
- **Persona knobs TODO** (`gordon/personas.go`) — per-persona calibration curves are a natural registry field once the registry goes JSON.
- **Live Polymarket execution** (deferred) — the cost model (fees, half-spread) built in Phase 0 is a prerequisite for going live anyway; paper is the right place to burn it in.

## Suggested order & effort

| Phase | What | Effort | Ship condition |
|---|---|---|---|
| 0 | MAE/MFE, cost accounting, calibration table (display-only), settlement prior | ~0.5 day | Tests green; one run of data collected |
| 1 | Calibrated confidence in consensus + mid-band | ~1 day | Calibration table shows real miscalibration worth correcting |
| 2 | Fractional-Kelly downsizing + drawdown ramp + cost-aware fill gate | ~1–2 days | Phase 1 in place (needs calibrated `p`) |
| 3 | Trailing stop (+ optional partial TP) | ~1–2 days | Phase 0 MFE data supports it |
| 4 | Learned correlation groups | ~1 day | Whenever; independent |

Phase 0 is the no-regrets move: it changes no trading behavior, costs half a day, and every later phase — plus Lou, plus the eventual live-mode decision — gets better data out of it.
