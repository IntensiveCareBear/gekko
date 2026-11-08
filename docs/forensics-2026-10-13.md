# Forensics: run 2026-10-12 19:24 → 2026-10-13 18:10

Scope: one launch at 19:24 PDT, read at 18:10 (uptime 22h46m, 0 sleep gaps, caffeinate held).
Sources: `logs/*.log` since 19:24, `runs/{equity.csv,trades.jsonl,run_summary.txt}`, liveview
`/api/positions` and `/healthz`, plus a status check of every unfinished-feature claim in `docs/`,
`gekko_improvement_tasks.md` and `bug_list.md` against the code. Follows
[the 10-11 report](forensics-2026-10-11.md). All **paper trading** (broker-worker `Kalshi=false | Polymarket=false`).

**Verdict.** The money is coming from the weather trader, which was un-paused for this run. Gordon-default
is a rounding error. The weather P&L is a lottery shape (25% win rate, 7–10x winners) and is only
profitable because three tails hit; the unfinished-features list is mostly stale docs, not missing code.

## Headline numbers

|                   |                                                                                  |
|-------------------|----------------------------------------------------------------------------------|
| NAV               | $100,920.62: +$423 in this run (from $100,497.63 at 19:04), +$920.62 since the 10-09 reset |
| Realized / open   | $1,645.84 realized (cumulative), −$621.25 unrealized on 11 open positions          |
| Max drawdown      | 2.31%, trough $99,484 at 08:39 on 10-13; recovered +$1,400 between 08:39 and 17:19 |
| Closed trades     | 18 cumulative: 6 wins / 12 losses (RESOLVED 7, STOP_LOSS 5, TAKE_PROFIT 6)         |
| weather           | 12 trades, 3 wins, **+$1,597.13**; best +$1,713 (LAX), +$862 (DEN), +$560 (NY)       |
| gordon-default    | 6 trades, 3 wins, **+$48.71**; 619 LLM declines, 14 breen RejectOnBreach           |
| Errors            | 0 panics; Twitter 402 ×57, Finnhub fetch errors ×14, Reddit 429 ×9, 8 "no live order book" |

## Finding 1 — weather is the P&L, and its edge is three tails

Weather was **weight 1.0 from 19:24:34** (`[PRIORITY] Persona "weather" weight 1.00`), so the 10-11 open
question was answered by someone resetting it. The bias table loads (`7 stations (7 seasoned)`), the
predictive path is firing (108 `[WEATHER]` lines, mostly "predicted final high … predictive BUY").

By settlement date:

| Target date | Result | Detail |
|---|---|---|
| Oct 11 | +$1,660 | LAX B74.5 bought at 0.125, sold 0.99 (+$1,713); PHIL −$53 |
| Oct 12 | **−$1,483** | 8 losers: 6 RESOLVED to 0 (−$43 to −$192 each), NY T69 stop −$380, MIA B88.5 stop −$326 |
| Oct 13 (so far) | +$1,421 | DEN B66.5 0.09→0.96 (+$862), NY T71 0.37→0.95 (+$560); 9 more tickets still open |

Entries are 3–14¢ at 1,000 contracts, so each losing ticket costs $30–190 and each winner pays 7–10x.
That is a defensible shape, but one Oct-12-style day erased a full day's gain and the sample is 12 trades.
Two things to keep in view:

- **Settlement hold is on for weather.** Breen logs `Settlement hold: weather — % stop / take-profit / trailing / force-exit all SKIPPED`.
  Yet two Oct 12 positions were STOP_LOSS closes (NY T69 −57.9% MAE, MIA −83%), so the dollar backstop (−$300) is
  firing on weather. That is the only exit control on this book, and it fired on trades that had `mfe_pct` +141% (MIA)
  before reversing. The winners exited at 0.95–0.99; I did not trace which rule closes them, so confirm that before relying on it.
- **42 breen rejections** `family already holds a different strike (one st…)`. Weather proposes several buckets per
  station-day; breen keeps one. Fine as a risk rule, but it means which bucket wins the race decides the day. Worth
  logging the rejected bucket's later settle so you can tell whether the rule is costing or saving money.

Open now: 9 weather lottery tickets on Oct 13 (all expire 23:59 ET, 20:59 PDT) plus KXCPICORE-26SEP-T0.2
(3,750 contracts at 0.37, expiry 10-14). That ticker says SEP but resolves Oct 14 and is a real CPI print, not the
stale-contract noise from the last report.

## Finding 2 — gordon-default is still near-idle, and the RejectOnBreach count is new

- 619 declines, all `model declined to trade`. 424 pre-gate bearish/no-trade cooldown skips (the biggest bucket,
  up from 520 over 15h, so about the same rate), 208 spread/one-sided skips, 105 ask-outside-band, 249 `[EDGE]` skips.
- **14 `[RISK] Rejected BUY … gordon limit-breached and RejectOnBreach`**, across 12 tickers (Brent ×4, BTC ×3, WTI,
  CPI, gas, ETH, …). Last report had 2 over 15h. This is gordon producing proposals that breach its own limits,
  consistent with the 10x bet size from `e3778e3` (3,750 contracts is now a normal ask). Those are proposals that
  *would* have traded, so the size/limit mismatch is gating more entries than any other single rule. Decide whether to
  raise the limit or clamp the size in gordon before it proposes, rather than having breen reject after the LLM call.
- The only losing gordon trade, KXWTI-26OCT1314-T93.99, was a 3,750 lot at 0.26 stopped at 0.19 (−$353, −26.9%),
  against a −15% stop that logged −$262.50 at the time of the stop (`trades.jsonl` records −$353; the $91 gap is probably fees and spread, not checked).
- BTC ladder trade KXBTCD-26OCT1417-T83999.99 returned +$250.

## Finding 3 — Finnhub key is still being written to the log

16 lines in this run's `kahuna.log` contain `token=…`. Open since the 10-11 report; redact at `finnhub.go` where the
raw HTTP error is logged, then rotate the key. Twitter is still dead (57 × `402 credits depleted`); disable the
source instead of retrying it every cycle.

## Finding 4 — the running binaries contain the uncommitted work

`gordon_bin` (19:24:33) and `kahuna_bin` (19:24:32) were built after the last edit to `gordon/weather.go` (18:59)
and `gordon/priority.go` (10-10 21:48), so the weather changes (`PredictiveMaxEdge`, `MinSigmaF`) are in the version
that just made the money. They are **uncommitted**: 13 modified files plus `bearcooldown_test.go`, `scripts/backfill_rag.sh`
and `migration_backup/` (345 MB). Commit the working weather code before changing anything else.
Also `logs/ollama.log` is 144 MB and growing; rotate it.

## Unfinished features: documentation vs code

Checked each against the code and this run's startup logs. "Stale" means the doc is wrong, not that work remains.

### Built but switched off (a decision, not a gap)

| Feature | State | Evidence |
|---|---|---|
| Trailing stop (Breen Phase 3) | Built, off | `Trailing stop: disabled (set both TRAIL_ARM_PCT and TRAIL_RETRACE_PCT)` |
| Calibrated confidence (Phase 1) | Built, off | `Calibrated confidence: enabled=false` |
| Cost-aware fill gate (Phase 2) | Built, off | `edge ≥ 0.0× round-trip friction (0 = off)` |
| Lou auto-tune | Built, off by default | `run.sh` only applies `runs/lou/overrides.env` if it exists |
| Fee model charge | **On** (doc says off) | `fee model=true (rate 0.070, charged to cash=true)` |

### Not built (real gaps)

| Item | Where documented | Status |
|---|---|---|
| Breen Phase 2 sizing scalars, graduated de-risking (expectancy/vol-regime) | `breen-intelligence-*`, `clodds-mining.md` | No `edgeScalar`/vol-regime code. Drawdown halt is still a binary 10% cliff |
| Breen Phase 4 learned correlation | `breen-intelligence-implementation-plan.md` | Keyword table only |
| Partial take-profit (`TP_FRACTION`) | same | "still not built" |
| Source credibility Phases 1–4 | `data-sourcing-and-credibility-proposal.md`, tasks L124–127 | No `SourceCredibility` or `source_reputation` anywhere in the tree |
| Gordon's creed Phase 2 (activity trigger) and 3 (on-demand ingest) | `gordons-creed-proposal.md` | No `ActivityEvent` / `market_activity` in marketdata, gordon, kahuna or breen |
| Cloud LLM overflow to Claude | tasks L77 | No Anthropic client in `gordon/` |
| Persist hosted-backend daily request counter | tasks L136 | No persistence in `gordon/gemini.go` |
| Per-contract dashboard | tasks L56 | Backend `contractRealizedPnL` exists, no panel |
| Strategy Scorecard as per-persona line graph | tasks L158 | Table only |
| `bug_list.md`: RNG reseed in `engine.go`, silent RAG upsert loss, lexicon sentiment, frontend lint (23 errors) | `bug_list.md` | Not re-verified this run; the file is undated and predates most of the repo's commits |

### Polymarket live execution (blocked, only matters if you go live there)

`gekko_improvement_tasks.md` L172–191 lists 12 defects in `broker-worker` (fabricated token ID, no L2 auth, wrong
EIP-712 domain, struct hash omits `signer`, inverted BUY amounts, failure masking that returns `Success: true`, no
fill confirmation, no cancel). All open. Both Kalshi and Polymarket credentials are absent, so nothing is exposed
today, but **Kalshi live execution has no equivalent checklist in the docs**. Since the strategy is working on Kalshi
data, that is the missing document before any real money.

### Stale docs to fix

- `weather-trading-proposal.md` header still says "scoped, not built". Weather is live and is the main source of P&L.
- `weather-phase2-forecast-bias.md`: "fee model absent / still open". The Kalshi fee model is on and charged.
- `gekko_improvement_tasks.md` L96 "Rate-limit Gordon's proposal generation (planned)". Done:
  `Proposal rate throttle: est. fleet 0.200 gen/s ÷ 3 gordons ⇒ ~15s/call`, plus `llmlimiter.go`.
- `gekko_improvement_tasks.md` L195 "plaintext keys world-readable". The key files are `chmod 700`; what remains is
  no `.env` consolidation (no `.env` exists).
- `breen-intelligence-implementation-plan.md` table: "KALSHI_FEE_CHARGE off", now on.

## Recommended order

1. Commit the weather work (Finding 4). It is the profitable code and it is untracked.
2. Redact the Finnhub token in the log call and rotate the key (Finding 3).
3. Fix the gordon size/limit mismatch (14 RejectOnBreach) before touching gates again.
4. Decide on a take-profit and a bucket-ladder policy for weather before sizing it up; the P&L is three trades.
5. Write the Kalshi live-execution checklist; leave Polymarket and creed Phases 2–3 until then.

## Weather review (added after the report): cheap tickets and the one-strike rule

Run `scripts/weather_review.py` (scores every `[WEATHER] Weather(model)` pick against Kalshi's settled result
and official high, cached in `runs/weather/settlements.json`). Sample at time of writing: **72 picks, 30
station-days, 5 calendar days**. Oct 13 is not finalized yet, so the DEN and NY winners (+$1,421) are **not** in it.

- **The model's `fair` is overconfident on this sample.** Mean fair 0.28, realized hit rate 8%. EV/contract is
  negative in every ask band (about −$0.005 to −$0.20), −$4.7k if all 72 were taken at 1,000 lots. Cheap tickets (ask < 0.10)
  lost about $0.04 per contract against a fee of about $0.003, so the loss is hit rate, not friction.
- **Edge size does not sort it.** By edge band the only positive one is ≥0.15 (n=24, +$1.4k, driven by a few
  winners). Nothing here supports tightening `WEATHER_PREDICTIVE_MAX_EDGE` (0.25); nothing supports loosening it.
- **Forecast error is bigger than the bias table's sigma at two stations.** AUS mean error +3.65°F against sigma
  1.5, LAX mean |error| 5.2°F against sigma 2.4. Both are a few days of data, but those two stations account
  for much of the overconfidence. Candidate fix: widen sigma (or floor it per station) for AUS and LAX, then re-score.
- **`history.jsonl` is a sound input.** It matches the official high within 0.1°F except one LAX day (1°F off).
- **One-strike-per-family is saving money here.** The 27 rejected picks would have lost $2,842 (7% hit) against
  +$140 for the 10 fills. It would have cost one winner (CHI Oct 12, +$820). Keep it.

Not concluded: whether weather has positive EV overall. The realized +$1,597 came from 3 trades that this
sample mostly predates or excludes, and 5 days cannot separate skill from luck. Re-run the script after Oct 13
finalizes and weekly after that; it needs about 20 settled days before the ask-band rows mean anything.

## Update 2026-10-15 13:54: the money stopped

Stack restarted 10-13 20:50 (calibration on, RejectOnBreach off) and again 10-15 11:07. Both healthy, 0 panics,
`reject-on-breach=false` and `Calibrated confidence: enabled=true` confirmed in breen's startup log, sigma floors in the binary.

| | 10-13 18:10 | 10-15 13:52 |
|---|---|---|
| NAV | $100,921 | **$97,879** (−2.12% since start, max drawdown 3.88%) |
| Realized | +$1,646 | −$1,332 |
| Closed trades | 18 (6 wins) | 55 (11 wins, 20%) |

- **Weather: 19 trades since, 0 wins, −$1,583.** All 10 Oct 13 tickets and all 9 Oct 14 tickets expired at 0
  (the DEN/NY Oct 13 winners in the last report were already booked). Lifetime weather is about +$14 over 31 trades,
  so the earlier +$1,597 was three lucky tails, not an edge. Review script on 112 settled picks over 7 days:
  mean fair 0.27 vs hit rate 8.9%, EV −$0.054/contract, −$6.0k if all were taken. The ≥0.15 edge band is the only positive one (+$1.5k, n=33).
- **One-strike-per-family: verdict softened.** The rejected side still lost more in total (−$3,057 vs −$957), but it beat the
  fill in 8 of 17 families (NY Oct 13 +$778, CHI Oct 12 +$820). It is not obviously helping; it is hiding that the picks are bad.
- **Gordon-default: 18 trades since 10-13 20:50, 5 wins, −$1,394** (it made 6 trades in the prior 4 days). 13 of 18 closed
  STOP_LOSS or LOSS_LIMIT. **Three were stopped out at 0 minutes held** (BTC T81749 −$188, BTC T82499 −$300, POLY −$38) and four more
  inside 30 minutes. Entries fill at the ask and stops mark against the bid/mid, while `ENTRY_MAX_SPREAD_PCT` is 0.15 and the stop is −15%:
  a book at the spread cap is already at the stop on the first mark. That is a config interaction, not a signal failure.
  Spread paid on these 18 trades: $303.
- Can't separate how much of the gordon activity comes from RejectOnBreach=false versus the earlier gate loosening;
  both landed in the same 48 hours. Sizes are now 3,000–5,400 contracts, so each stop costs $100–$420.

Suggested, not done: (1) set `ENTRY_MAX_SPREAD_PCT` to about 0.05 (or stop on mid with a 5-minute grace) so a stop can't fire on the spread alone;
(2) stop adding weather capital: 10x size on a model whose hit rate is a third of its fair; (3) restore RejectOnBreach only if the gordon
size jump proves to be the cause (compare against 3,750-lot caps before the change).
