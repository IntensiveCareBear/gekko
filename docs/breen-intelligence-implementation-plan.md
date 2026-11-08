# Breen Intelligence — Implementation Plan

_Execution plan for `docs/breen-intelligence-proposal.md` (2026-07-08), re-based on the
tree as of 2026-08-20. The proposal is the "why"; this is the "what to type", with the
file/function landing sites, exact env knobs, test targets, and the places where the
original design needs a concrete decision it didn't make._

## 1. What already shipped since the proposal

| Proposal item | Status | Where |
|---|---|---|
| Phase 0 — settlement prior fix | **Done** | `SETTLE_PRIOR=mark\|entry` — [config.go:177](breen/config.go:177), [account.go:863](breen/account.go:863), tests [account_test.go:246](breen/account_test.go:246) |
| Phase 3 — take-profit | **Partly done, static** | `takeProfitReached`, wired in `applyStopLosses` pass 1; both limbs default **0 (off)**. No partial. |
| Phase 3 — trailing stop | **Built 2026-08-21, off** | `trailingStopHit` in [account.go](breen/account.go); `TRAIL_ARM_PCT`/`TRAIL_RETRACE_PCT` both 0 — see §5 |
| Phase 0 — MAE/MFE | **Done 2026-08-20** | `Position.BestMark/WorstMark` + `updateExcursions`; stamped as `mfe_pct`/`mae_pct` |
| Phase 0 — cost accounting | **Done 2026-08-20** | [costs.go](breen/costs.go); `KALSHI_FEE_MODEL` on, `KALSHI_FEE_CHARGE` **on** (set by `scripts/relaunch.sh`; the code default stays `false`) |
| Phase 0 — calibration table | **Done 2026-08-20** | [calibration.go](breen/calibration.go) → `gekko:calibration` → `/api/calibration` |
| Phase 1 — calibrated confidence | **On since 2026-10-13** | `CALIBRATION_ENABLED` defaults true in `run.sh`; no effect until a persona bucket has 20 trades — see §4.1 |
| Phase 2a — drawdown ramp | **Built and on 2026-10-15** | [sizing.go](breen/sizing.go) `drawdownScalar`; fills x1.0 at 4% drawdown to x0.25 at the 10% halt; `SIZING_ENABLED` (on in `scripts/relaunch.sh`) |
| Phase 2b — Kelly scalar | Not started | needs calibrated win probabilities (Phase 1 data) |
| Phase 2 — cost-aware fill gate | **Built 2026-08-25, off** | `entryEdgeBlock` in [costs.go](breen/costs.go); needed the `FairValue` plumbing from gordon. `ENTRY_EDGE_FRICTION_MULT=0`. 2026-10-19: still off; turn on at 2× once breen's `inferExchange` stops zeroing crypto fees (`gekko_improvement_tasks.md` 10-19 review) |
| Phase 4 — learned correlation | Not started | `contractCategory` keyword table [tracker.go:395](breen/tracker.go:395) |

Two things changed underneath the proposal and shape the plan:

- **Risk tiers** (`1bb0abb`) put a 5-step size/exposure knob in the UI
  (`conservative → aggressive`, `frontend/src/components/ModelControls.tsx`). The
  Phase 2 sizing scalar must apply **after** the tier caps, never as a substitute —
  the tier sets the ceiling, the scalar shrinks inside it.
- **Gordon's `requiredEdge`** ([gordon/signals.go:215](gordon/signals.go:215)) already
  adds the top-of-book spread to `MIN_EDGE`. Breen's Phase 2 fill gate is therefore a
  *re-check at the real fill*, not a new idea — and it needs a field gordon doesn't
  currently publish (see §4.3).

## 2. Ordering and effort

**Phase 0 (0a/0b/0c) is built as of 2026-08-20** — uncommitted, needs a rebuild and
`./run.sh` restart. It changes no trading behaviour. Every new field is additive: the
198 trades in the live run unmarshal into the new `ClosedTrade` unchanged, and they
carry no excursion or confidence data, so the calibration card stays hidden and the
excursion tiles read "—" until trades opened under the new build close. §6.1 records
what to check once a run has accumulated.

| Step | Scope | Effort | Ship gate |
|---|---|---|---|
| 0a | MAE/MFE excursion tracking | 0.5 day | ✅ Tests green; excursions non-zero on a live run |
| 0b | Cost accounting (spread decomposition + Kalshi fee model) | 0.5 day | ✅ Fee totals appear in scorecard |
| 0c | Calibration table (compute + publish + display, unused) | 1 day | ✅ Table populated after ≥1 run |
| 1 | `calibratedConfidence` into consensus + mid-band | 1 day | 0c shows miscalibration worth correcting |
| 2a | Graduated drawdown ramp | 0.5 day | Independent of 1 — can ship early |
| 2b | Fractional-Kelly downsizing | 1 day | Needs 1 (calibrated `p`) |
| 2c | Cost-aware fill gate (needs gordon `FairValue` plumbing) | 0.5 day | Needs 0b |
| 3 | Trailing stop, then optional partial TP | 1–2 days | Needs 0a data to justify the knobs |
| 4 | Learned correlation groups | 1 day | Anytime; independent |

0a/0b/0c are behaviour-neutral and can all land in one branch. **2a is the cheapest
real P&L change in the whole plan** and does not depend on the calibration chain — if
only one thing ships, ship 2a.

---

## 3. Phase 0 — measurement

### 3.1 MAE/MFE (step 0a)

**The catch the proposal missed:** `checkStopLosses` returns *before* fetching marks
when no stop/TP limb is enabled ([account.go:590-598](breen/account.go:590)). Excursion
tracking must not inherit that gate.

Changes:

1. `Position` ([tracker.go:18](breen/tracker.go:18)) gains
   `BestMark float64` / `WorstMark float64` (`json:"best_mark,omitempty"` etc.).
   Zero means "never marked" — readers substitute `EntryPrice`, which also handles
   positions recovered by `replayLedger` ([recovery.go](breen/recovery.go)) and by the
   Redis snapshot path.
2. Split `checkStopLosses` into: fetch marks (unconditional when positions exist) →
   `updateExcursionsLocked(marks)` → exit passes (still behind the `enabled` check).
   New pure-ish helper next to `applyStopLosses`:
   ```go
   // updateExcursionsLocked stamps each position's running best/worst LIVE mark.
   // Caller holds pt.mu.
   func (pt *PositionTracker) updateExcursionsLocked(marks map[string]float64)
   ```
   Adds no Redis traffic in the default config (stops are on: `STOP_LOSS_PCT=15`), and
   one mark-fetch per 30s tick if an operator disables every exit limb.
3. On an **add** to an existing position ([tracker.go:776](breen/tracker.go:776)) the
   entry price is re-averaged; reset `BestMark`/`WorstMark` to the new average entry so
   the excursion stays measured against the basis it will be scored on.
4. `ClosedTrade` ([tracker.go:188](breen/tracker.go:188)) gains
   `MaxFavorableExcursionPct` / `MaxAdverseExcursionPct`, stamped in
   `recordClosedTradeLocked` ([account.go:142](breen/account.go:142)) as
   `(best−entry)/entry×100` and `(worst−entry)/entry×100`, each folded with the actual
   exit price so a trade that closed at its extreme is never understated.

Tests (`breen/excursion_test.go`): monotone up path, monotone down path, add-to-position
reset, unmarked position closes with MFE=MAE=exit return, and a regression test that
excursions still update with all exit limbs disabled.

### 3.2 Cost accounting (step 0b)

**Design decision the proposal glosses over:** in dry mode, half-spread is *already*
paid — the BUY fills at the ask ([tracker.go:723](breen/tracker.go:723)) and the exit at
the bid. Subtracting a "spread cost" from realized P&L would double-count it. Fees are
the opposite: never charged to cash anywhere today.

So the two get different treatment:

- **`SpreadCostUSD` — diagnostic only.** Computed at fill from the book breen already
  fetched: `(ask − mid) × filledSize` on entry, `(mid − bid) × size` on exit; summed
  onto `Position.SpreadCostUSD` across adds, carried to `ClosedTrade`. It *decomposes*
  realized P&L ("how much of this loss was just the round trip"); it never debits cash.
  Document this in the field comment or someone will "fix" it later.
- **`FeeUSD` — a real, currently-missing debit.** New `breen/costs.go`:
  ```go
  // kalshiFee returns the published Kalshi trading fee: ceil(0.07 × C × P × (1−P))
  // in cents, per side. Zero for non-Kalshi contracts (inferExchange) or when the
  // model is disabled.
  func kalshiFee(price float64, size int) float64
  ```
  Two knobs, deliberately separate: `KALSHI_FEE_MODEL` (default `true` — *record* the
  fee) and `KALSHI_FEE_CHARGE` (default `false` — *debit cash* for it). Recording first
  lets us see the drag on the scorecard without breaking comparability with prior runs;
  flipping the charge knob later is the honest-accounting move ahead of any live mode.
- Scorecard: `computePersonaStats` / `computeBookMetrics`
  ([metrics.go](breen/metrics.go)) gain `NetPnLAfterFees` and `SpreadCostUSD` as
  *additional* fields. `RealizedPnL` keeps its meaning — the allocator, Lou, the RAG
  feedback and the ledger all read it.

Tests (`breen/costs_test.go`): fee at P=0.5 (max) and at the tails, cent-rounding
direction, non-Kalshi ⇒ 0, disabled ⇒ 0, and an `ExecuteOrder` test asserting cash is
untouched with `KALSHI_FEE_CHARGE=false` and debited with it on.

### 3.3 Calibration table (step 0c)

**The enabling change the proposal doesn't name:** `ClosedTrade.PersonaScores` carries
no confidence. `ContributorProposal.Confidence` exists on the *Position* and is dropped
at close by `personaScoresForClose` ([account.go:333](breen/account.go:333)).

1. `PersonaScore` gains `Confidence float64` — the size-weighted mean of that persona's
   `ContributorProposal.Confidence` entries, computed in the existing `agg` loop
   (add a `conf` numerator alongside `cost`).
2. New `breen/calibration.go`:
   ```go
   type CalibrationBin struct {
       Persona  string  `json:"persona"`
       Lo, Hi   float64 `json:"lo","hi"`   // stated-confidence bucket
       Trades   int     `json:"trades"`
       Wins     int     `json:"wins"`
       WinRate  float64 `json:"win_rate"`  // raw
       Adjusted float64 `json:"adjusted"`  // shrunk toward the persona's overall rate
   }

   // computeCalibration bins each persona's stated confidence against its realized
   // win rate (PersonaScore.RealizedPnL > 0), shrinking thin bins toward the
   // persona's overall rate with the same n₀ trick as personaWeights.
   func computeCalibration(trades []ClosedTrade, bins int, n0 float64) map[string][]CalibrationBin
   ```
   Shrinkage mirrors `personaWeights` ([consensus.go:434](breen/consensus.go:434)):
   `adjusted = raw·(n/(n+n₀)) + overall·(n₀/(n+n₀))`, `n₀ = 5`.
   Bins are fixed-width over `[0,1]`, `CALIBRATION_BINS=5`.
3. Publish in `syncAccountToRedis` ([account.go:480](breen/account.go:480)) to
   `gekko:calibration` (new const beside `bookMetricsKey`,
   [account.go:49](breen/account.go:49)).
4. Serve + display: `/api/calibration` handler in
   [gordon/liveview/main.go:332](gordon/liveview/main.go:332) (copy `handleBookMetrics`),
   add `"calibration.json": "gekko:calibration"` to the export map
   ([gordon/liveview/run.go:339](gordon/liveview/run.go:339)), and a small table card in
   `frontend/src/components/SystemMetrics.tsx` — persona rows × confidence-bin columns,
   cell = realized win rate, colour-coded against the bin's midpoint so miscalibration
   is visible at a glance.

Nothing consumes the table in this step. Run one full session before Phase 1 and read
it: **if the bins are flat, Phase 1 is not worth building** and the plan should jump to
2a/3.

---

## 4. Phase 1 & 2 — acting on it

### 4.1 Calibrated confidence (step 1) — **built 2026-08-20, inert**

Built and committed off the back of Phase 0, but **`CALIBRATION_ENABLED=false`**: the
table it reads is still empty (the restart was the same day), so the ship gate above is
not yet met. Nothing changes until an operator turns it on, and turning it on before
reading the table is the one thing this section asks you not to do.

**The design decision the original sketch got wrong.** The plan above proposed mapping
stated confidence *to* the bucket's realized win rate. That is the obvious reading and
it would have flatlined the funnel: realized win rates here run 30–50% (asymmetric
payoffs make that fine) while `MID_BAND_MIN_CONFIDENCE` is 0.70, so substituting one for
the other rejects essentially every mid-band entry. What shipped instead rescales by the
**ratio** of the bucket's win rate to the persona's own overall win rate:

```go
factor    = clamp(bin.Adjusted / curve.WinRate, CALIBRATION_FACTOR_MIN, CALIBRATION_FACTOR_MAX)
calibrated = clamp(stated × factor, 0, 1)
```

Confidence stays on its own scale, so the gates keep meaning what they were tuned to
mean, while a persona that cries 0.9 and delivers half its own average still gets halved.
This also draws the line against the allocator cleanly — `personaWeights`/`AdaptiveAllocator`
answer *is this persona any good* (level); calibration answers *does this persona's own
confidence mean anything* (shape). A persona that never wins is therefore **not** cut
here; that is the allocator's job, and its overall rate is this formula's denominator.

Falls back to stated confidence — explore — on an unknown persona, a bucket thinner than
`CALIBRATION_MIN_SAMPLES`, or a persona with no wins to form a baseline from.

Call sites, all via a nil-able `confidenceCalibrator` func so `consensus.go` stays free
of tracker state and every existing test passes `nil` for unchanged behavior:
- `weightedConviction` / `pickWinningContract` / `consolidateProposals`
- `midBandConfidence` — each contributor is calibrated and *then* the max is taken (not
  the reverse), so the loudest persona can't carry the gate on a claim its record
  doesn't support. Covers both `riskGate` and the at-fill re-check.

**The self-reference trap:** `ContributorProposal.Confidence` keeps the **raw** stated
value. That field is what the calibration table is built from (via
`PersonaScore.Confidence`), so recording a calibrated number there would make the table
calibrate its own output and drift further every run. There is a test pinning this.

Knobs: `CALIBRATION_ENABLED=false`, `CALIBRATION_MIN_SAMPLES=20`, `CALIBRATION_BINS=5`,
`CALIBRATION_FACTOR_MIN=0.5`, `CALIBRATION_FACTOR_MAX=1.5` (the clamp mirrors
`personaWeights`').

**Residual risk, and how to see it.** The ratio form bounds the squeeze but does not
eliminate it: with `FACTOR_MIN=0.5`, a 0.95 claim from a persona whose top bucket
underperforms becomes 0.475 and fails the 0.70 gate. If the *whole fleet* is badly
calibrated at high confidence, the funnel can still close. So when you enable it, watch
the entry count for one run against the run before, and be ready to raise
`CALIBRATION_FACTOR_MIN` (a gentler correction) or drop `MID_BAND_MIN_CONFIDENCE` — see
the `MID_BAND_MIN_CONFIDENCE` comments in [config.go](breen/config.go:96) for the two
previous flatlines. The calibration card carries a **watching / steering** badge so it
is never ambiguous which mode a run was in.

### 4.2 Sizing scalars (steps 2a, 2b)

Land in a new `breen/sizing.go`, both pure:

```go
// drawdownScalar ramps position size from 1.0 at easeStart down to floor at the
// halt threshold, replacing the cliff between full size and no entries at all.
func drawdownScalar(ddPct, easeStart, haltPct, floor float64) float64

// kellyScalar shrinks a proposal toward the fractional-Kelly stake. p is the
// calibrated win probability, c the actual fill price, proposedFraction the
// proposal's own share of NAV. Never exceeds 1.0 — the sizing layer only shrinks.
func kellyScalar(p, c, proposedFraction, kellyFraction, minScalar float64) float64
```

`kellyScalar` is where the proposal left a hole: it gives the Kelly formula
(`f* = p − (1−p)·c/(1−c)`) but not how a *bankroll fraction* becomes a *scalar on a
proposed size*. Concrete rule:

```
target   = kellyFraction × f*          // e.g. 0.25 × 0.20 = 5% of NAV
scalar   = clamp(target / proposedFraction, minScalar, 1.0)
```

So a proposal already smaller than the Kelly stake is untouched (scalar 1.0), an
oversized one is cut to the Kelly stake, and negative `f*` floors at `SIZE_SCALAR_MIN`
rather than rejecting — the veto guards own rejection, and a small position keeps the
allocator and the calibrator fed.

Wiring: in `ExecuteOrder`, **after** `execPrice` is resolved and the mid-band re-check
passes, before the buying-power check ([tracker.go:751](breen/tracker.go:751)):
`filledSize = max(1, int(float64(proposal.TotalSize) × riskScalar × edgeScalar))`.
Placing it at the fill (not in `riskGate`) matters twice: Kelly needs the real `c`, and
because the scalars only shrink, every cap `riskGate` already cleared at full size stays
cleared. Log a `[SIZE]` line mirroring the `[RISK]` rejection format, and put both
scalars in the ledger ref so sizing is auditable.

Knobs: `DRAWDOWN_EASE_START=4`, `DRAWDOWN_EASE_FLOOR=0.25`, `KELLY_FRACTION=0.25`,
`SIZE_SCALAR_MIN=0.25`, master `SIZING_ENABLED=false`. The existing
`MAX_DRAWDOWN_HALT_PCT=10` cliff stays as backstop.

Tests (`breen/sizing_test.go`): ramp endpoints and midpoint, floor clamp, `haltPct ≤
easeStart` degenerate config, Kelly at `p=c` (zero edge ⇒ floor), a proposal below the
Kelly stake (scalar 1.0), and an `ExecuteOrder` test proving a scaled order never
exceeds the caps `riskGate` approved.

### 4.3 Cost-aware fill gate (step 2c)

Needs a field that doesn't exist: gordon computes fair value but publishes only
`Confidence`. Add `FairValue float64 \`json:"fair_value,omitempty"\`` to the proposal
struct on both sides ([gordon/main.go](gordon/main.go:840) emit site,
[tracker.go:219](breen/tracker.go:219) decode site) — zero means "unknown", which
disables the gate for that proposal so older producers and the weather strategy keep
working.

Then extend the fill-time check next to `entryBookBlock`:

```go
// entryNetEdgeBlock refuses a BUY whose edge at the ACTUAL fill no longer clears
// the round-trip cost. Gordon checks this against the book it saw seconds earlier
// (requiredEdge); this is the executive-side re-check at the book we fill on.
func entryNetEdgeBlock(fair, bid, ask float64, size int, feeModel bool, minNetEdge float64) string
```

`fair − ask ≥ halfSpread + feePerContract + ENTRY_MIN_NET_EDGE`. Knob
`ENTRY_MIN_NET_EDGE=0` (off) so it lands inert and is turned on after one run of the
0b cost data shows what the real drag is.

---

## 5. Phase 3 — exit intelligence — **trailing stop built 2026-08-21, off by default**

The ship gate was "does the MFE distribution show winners giving back big excursions?"
After ~25h of Phase 0 data (31 measured trades) the answer is yes: **15 of 31 ran +10%
or better and collectively realized +$27.93, with 7 of the 15 closing red.** Average MFE
+18.1% against an average realized −4.8%. So the trailing stop was built.

```go
func trailingStopHit(entry, best, mark float64, size int, armPct, retracePct, minAbs float64) (bool, string)
```

Armed once the best live mark reached `armPct`% above entry; fires once the mark has
given back `retracePct`% **of entry** from that high. Give-back is measured against the
entry basis rather than against the peak or as a share of the gain, so it sits on the
same scale as every other exit knob in `account.go`. Wired into `applyStopLosses` pass 1
as `STOP_LOSS → TRAIL_STOP → TAKE_PROFIT`; the stop goes first because it is the only
loss of the three, and trail/TP exit at the same mark when both would fire, so their
order decides only the label.

Two things the original sketch got wrong:

- **Arming does not protect against tick noise.** The sketch claimed unarmed positions
  couldn't fire on the noise `STOP_LOSS_MIN_ABS` exists to ignore, and inferred no floor
  was needed. But a position can arm on a *real* move and then be knocked out by one
  tick: on a $0.03 contract that ran to $0.06, a single tick down is a 33%-of-entry
  "give-back". The trail therefore shares the stop's `stopLossMinAbs` floor.
- **`exitsEnabled` had to grow.** The exit pass was skipped entirely unless a
  stop/take-profit/loss-limit was configured, so a trail-only run would have silently
  harvested nothing. Pinned by a test.

Thresholds also needed an epsilon: `(0.24−0.20)/0.20×100` is `19.999999999999996`, so a
clean +20% run would never have armed against a bare `>= 20`.

**What the parameters are worth, on the 31 measured trades.** Reconstructing the high
from `mfe_pct` and asking where the trail would have sat — if that level is above the
actual exit, price must have crossed it on the way down:

| arm | retrace | trades exiting earlier | est. P&L delta |
|---|---|---|---|
| +10% | 5–10% | 7 | **+$21.75 → +$20.10** |
| +15% | 5–10% | 4 | +$18.00 → +$16.35 |
| +20% | any | 1 | +$0.50 |
| +30% | any | 0 | 0 |

Turns −$177.76 into about −$156. Real, but **modest — ~12% of the loss, and smaller than
the $102 of modelled fees on the same 31 trades.** Note `retracePct` barely matters here:
at these prices `STOP_LOSS_MIN_ABS=0.02` binds before any give-back percentage does.

Caveats on those numbers, which are a first-order bound and not a backtest: they assume
the exit lands exactly at the trail level (optimistic — the real fill is the first 30s
mark at or below it), and they cannot see paths that dipped through the level and
recovered, which is exactly the case where a trail *hurts*. Sample is one day.

Suggested starting point if enabled: **`TRAIL_ARM_PCT=15`, `TRAIL_RETRACE_PCT=10`.**
`arm=10` scored slightly better but arms on smaller moves, which carries more of the
unmeasured "cut a winner short" risk the estimate is blind to.

Partial TP (second step, still **not built**): `TP_FRACTION` of size closed at
`TP_TRIGGER_PCT`, remainder rides under the trail. `recordClosedTradeLocked` already
takes a `size`, and `ExecuteOrder` already has a partial-sell path — but `closeLocked`
in `applyStopLosses` does not; it needs a partial variant that leaves the position open
with a reduced size and preserves `ContributorProposals` for scoring the remainder.

Knobs: `TRAIL_ARM_PCT=0`, `TRAIL_RETRACE_PCT=0` (both must be > 0 to arm the feature),
`TP_FRACTION=0`, `TP_TRIGGER_PCT=0`. Once burned in, `TRAIL_RETRACE_PCT` joins Lou's
whitelist ([lou/autotune.go:49](lou/autotune.go:49)) as a `TightenUp` knob with a
0.05 `MaxStep`, alongside the existing `STOP_LOSS_MIN_ABS` entry — not before.

## 6. Phase 4 — learned correlation

Lowest priority, independent of everything above. Record per-family daily mark returns
from the mark cache ([tracker.go:126](breen/tracker.go:126)) into a Redis sorted set;
compute rolling pairwise correlation over ≥N observations; families above
`CORR_GROUP_THRESHOLD=0.7` get a synthetic shared category that
`contractCategory` ([tracker.go:395](breen/tracker.go:395)) returns *in addition to* the
keyword result, feeding the existing category caps. The keyword table stays as the
cold-start floor — it is the only thing that works on day one of a run.

## 6.1 What Phase 0 actually shipped, and what to read off it

Landed 2026-08-20, all behaviour-neutral:

- **Excursions.** `Position.BestMark`/`WorstMark` fold in every settlement tick via
  `updateExcursions`; the mark fetch in `checkStopLosses` moved out from behind the
  exit-limb check so excursions keep accruing with every exit limb off. A binary
  settlement does not fold its payout in, so settled losers don't all read −100%.
  Adds are re-based to the new average entry.
- **Costs.** `SpreadCostUSD` (entry leg always; exit leg only on the book-filled
  `DRY_CLOSED` path) and `FeeUSD` land on every closed trade, apportioned across
  partial closes so no fill is billed twice. The fee follows Kalshi's
  `ceil(0.07·C·P·(1−P))`. `KALSHI_FEE_MODEL=true` records; `KALSHI_FEE_CHARGE=false`
  means cash is untouched — flip it only when you accept that the run's P&L stops
  being comparable with earlier ones.
- **Calibration.** `PersonaScore.Confidence` now survives to the close (it was
  dropped there before, which is why nothing could be calibrated), binned per persona
  into `gekko:calibration`, served at `/api/calibration`, rendered by
  `CalibrationTable.tsx`. Thin bins shrink toward the persona's overall rate with the
  same n₀=5 as `personaWeights`.
- `trades.csv` gains four columns; a file written under the old header is rotated to
  `.legacy-<ts>` rather than continued, so no CSV mixes row widths. `trades.jsonl`
  (what Lou reads) was already additive.

The round-trip test is worth reading as a worked example: a 100-lot bought at the ask
0.43 and sold at the bid 0.42 realizes exactly −$1.00, and `SpreadCostUSD` is exactly
$1.00. The entire loss was the round trip — which is the number Phase 0 exists to make
sayable.

**After one run, read three things** before starting anything else:

1. The calibration card. Flat rows across the confidence buckets ⇒ **skip Phase 1** and
   go straight to 2a/3. Non-flat ⇒ the calibration chain is worth its 2 days.
2. `avg_mfe_pct` vs the average realized return. A large gap is the prize a trailing
   stop plays for; a small one means Phase 3 is noise.
3. Total `fee_usd` against realized P&L. That is how much the run would have lost to
   fees it never paid — and the number that decides whether `KALSHI_FEE_CHARGE` should
   go on before any live-mode conversation.

## 7. Guardrails carried from the proposal

- Every new behaviour behind an env knob with a conservative default; every scalar
  clamped; every decision logged and in the ledger ref.
- **Never size up in v1.** Upside multiplication stays with consensus agreement.
- Explore floors everywhere: calibration falls back to stated confidence on thin bins,
  Kelly floors at `SIZE_SCALAR_MIN` rather than zero.
- Sim-honesty before learning — already satisfied by the shipped `SETTLE_PRIOR=mark`.
- Lou stays the offline brain; the new knobs join its whitelist, nothing here
  duplicates its analyzers.

## 8. Rollout

Steps 0a–0c ship together, dark, and a full run is collected before anything reads the
data. Then, in order of confidence: **2a** (ramp — no new data needed, removes a known
cliff), **3 trailing stop** (if the MFE distribution supports it), **1 → 2b** (the
calibration chain, only if the table shows real miscalibration), **2c**, **4**.

Each step: `go test ./...` in `breen/`, rebuild, `./run.sh` restart, and one run
compared against the previous on the book scorecard before the next step turns on.

## 9. Evidence log

**2026-10-15, take-profit/trailing on weather: rejected.** Weather tickets are 3-14c lotteries held to settlement. 31 trades,
+$14 net, carried by two tails (+$862 peaking at +972%, +$1,713 peaking at +692%). Simulating a take-profit at +50% to +500%
(optimistically filled at the threshold) gives -$860 to -$1,600 at every level, because it caps those two. Keep weather in
`SETTLEMENT_HOLD_PERSONAS`. The same data shows 12 weather tickets that ran +40% to +230% and expired at 0, which is the
cost of that choice.

**2026-10-15, trailing stop for gordon-default: open.** Four gordon trades peaked at +22% to +25% and were then stopped out at
-16% to -36% (ETH T2459 -$143, WTI T96.99 -$281, NASDAQ B29750 -$419, BTC T83999 -$72). A trail armed near +20% with a
~10-point retrace would have exited them near +10%. Four trades is thin and the effect on winners that dipped after +20% is
unmeasured (the ledger stores peak and final, not the path). `TRAIL_*` is global but weather is skipped by the settlement hold,
so enabling it affects gordon-default only.
