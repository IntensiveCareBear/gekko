# Desk review: why the book still bleeds (2026-08-25)

Scope: current ledger period 2026-08-09 → 08-25 (341 closed trades, `runs/trades.jsonl`), the 14 archived runs since 06-22, `runs/weather/history.jsonl` (264 station-days), Lou's lesson book, and the relevant code paths in `breen/`, `gordon/`, `lou/`, `run.sh`. Interactive version with charts: see the "Gekko-1 Desk Review" artifact.

**Verdict.** Not one leak — three, stacked, and the scoreboard under-reports all of them. The LLM persona book has no edge and should be benched. The weather model *does* have edge at settlement — and the exit engine is destroying it. The fee model knows the truth and nobody charges it to the P&L.

## Headline numbers (current period)

|                       |          |
|-----------------------|----------|
| Net P&L (reported)    | −$876.57 (−0.88%) |
| Modelled fees, recorded but never debited | −$599.30 |
| **Economic P&L**      | **≈ −$1,473 (−1.5%)** |
| Closed trades         | 341 over 16 days, median clip $17.70 notional |
| Win rate / payoff / PF | 32.8% / 1.43 / 0.70 (breakeven needs 41% at this payoff) |
| Exits                 | 190 STOP_LOSS (−$2,516) · 90 TAKE_PROFIT (+$1,531) · **10 RESOLVED** · 51 other |
| Friction              | $836 total = 3.5% of notional per round trip; 7.7% on the 204 clips under $25 |

## Finding 1 — the scoreboard lies

`KALSHI_FEE_CHARGE` defaults false, `run.sh` never sets it, and `RealizedPnL` in `breen/account.go` is `(exit − entry) × size`. The half-spread is inside the P&L (fills cross the book, marks at mid — `breen/costs.go` gets this right), but $599 of modelled venue fees exist only as telemetry. Every learner (allocator, Lou, persona scorecard) optimizes against a P&L that flatters every trade — most of all the small mid-price clips where `7% × P(1−P)` bites hardest.

Decomposition: signal P&L (mid-to-mid) −$637 → spread crossed −$237 → reported −$874 → fees −$599 → economic ≈ −$1,473. Friction is 57% of the economic loss, but note the first term: the picks lose even frictionless. Both a cost problem and a selection problem.

## Finding 2 — the vig is unplayable at this clip size

Median entry 23¢ → ~2.5¢ round-trip fee + 2–4¢ round-trip spread ≈ **15–20% of premium consumed by friction**. No signal on this desk clears that hurdle per round trip. Settlement is the cheap exit (no exit spread, no exit fee); the book paid two legs on 331 of 341 trades.

## Finding 3 — settlement-edge strategy, day-trader risk manager

The weather gordon's edge (if any) realizes at settlement; breen manages it with a 15%-of-entry mark stop and 40% TP on thin books.

- 15% of 23¢ = 3.5¢ — inside bid-ask bounce; 39/190 stops were moves ≤3¢.
- Stops fire late on stale marks: median realized stop **−23%** vs the configured 15% (already flagged in `docs/forensics-2026-07-14.md`; still true).
- ~Half of stopped trades with MFE data were +10% or better before stopping out.

**Counterfactual** (284 of 300 KXHIGH trades matched to realized highs in `runs/weather/history.jsonl`, re-scored as settling $1/$0): actual −$428 → held-to-settlement **+$2,950**. Robust: excluding the five best counterfactual trades the swing is still +$878. Caveats: METAR-derived highs vs official NWS settlement; holding all raises overnight exposure.

**Entry calibration** (settled-YES rate vs price paid): 0.20–0.30 → 39.2% (+15pp edge, n=51); 0.30–0.40 → 37.9% (+3pp, n=58); 0.40–0.50 → 50.0% (+6pp, n=28); ≤0.20 ≈ fair value (n=134); 0.50–0.60 → 27.3% (−26pp, n=11). The 20–50¢ band is a real edge; the sub-20¢ flow is lottery tickets paying vig.

The model bought contracts that settled YES more often than the market priced, and the risk layer sold nearly all of them back at a loss before resolution.

## Finding 4 — churn

89 contracts traded >1×; 165 extra round trips; −$490 concentrated in repeat names. `KXHIGHLAX-26AUG22-T83` was entered 8 times in one day. The 30-min cooldown meters churn, doesn't stop it — no new-information requirement for re-entry. Same failure mode as July's POLY sell/re-buy loop.

## Finding 5 — the persona book has no alpha, and the system knows

Lou's validated lessons (conf 0.86–0.95): bull −10.8% on $725k deployed, cautious-bull −11.7%, bear −11.5%, gemma-bull −8.2%, gemma-neutral −7.3%, weather-persona −11.0% (pre-exit-fix). This period the non-weather book: −$419 on 41 trades (Brent −$373/18, GDP −$281 on two sibling strikes — sequential sibling re-entry is not blocked by `ONE_STRIKE_PER_FAMILY`).

Why it keeps trading them: `ADAPTIVE_EXPLORE_FLOOR=0.25` / `ADAPTIVE_MIN_WEIGHT=0.25` mean a two-month loser still gets quarter-weight forever; Lou's auto-tune whitelist contains exactly one knob (`MAX_ENTRY_PRICE`) against 19 validated lessons; and binary/source drift (the `MIN_EDGE` 0.05-vs-0.10 saga) goes undetected.

## Finding 6 — the long tape

Cumulative since 06-23: **≈ −$52k across ~3,700 trades** (06-22's +$50k run predates realistic fills; excluded). Runs improved from −$10k to −$1k because `MAX_ENTRY_PRICE` walked 0.70→0.55 and clips shrank — betting less, not picking better.

## What's working

Risk plumbing (caps, drawdown halt — max DD 1.31%, every mistake survivable); the `costs.go` decomposition (needs to be charged); cleared-bucket weather near-arb; the weather entry calibration itself; Polymarket +$165/7; and the July forensics show the desk can find and fix real bugs.

## Fix list (expected P&L per unit of work)

1. **Truth scoreboard.** `KALSHI_FEE_CHARGE=true`; all learners consume net-of-fee P&L.
2. **Let the weather book resolve.** No mark % stop / 40% TP on weather. Size for 100% loss (binary = the stop), hold to settlement, exit early only on information (forecast reversal, or model prob < bid − round-trip cost). Keep dollar backstops. Priced at ≈ +$3,400/16 days on the same entries.
3. **Bench the personas — shadow mode.** Keep proposing, stop filling; written re-activation bar (e.g. positive net-of-fee expectancy over 200 shadow trades). Kill the 0.25 floor for validated losers.
4. **Edge > friction gate.** Reject entries whose modelled edge < 2× (round-trip fee + spread). Floor clip size (~$200) and budget trades/day: 341 trades → ~40 without touching winners.
5. **One round trip per contract-family per day** unless model inputs changed. Also block sequential sibling-strike re-entry after a family stop.
6. **Give Lou hands + detect drift.** Whitelist a family blacklist, persona bench, 0.50+ entry stop; every process logs effective config hash at startup vs source.
7. **Hygiene.** RSA private key at repo root (`gekko.txt`) and plaintext API keys — rotate and move out of tree. Fix or drop the dead Reddit ingest (sentiment saturates at +1.00). If stops survive anywhere, fix stale-mark slippage.

One line: charge the fees, bench the storytellers, and let the weather book do the only thing it's provably good at — being right at settlement. Two weeks in that configuration will tell you whether Gekko has a business.

**Status 2026-10-19.** Fix 1 done (fees charged). Fix 2 done for weather (`SETTLEMENT_HOLD_PERSONAS=weather`). Fix 3 done, then undone on 10-01 (gordon-default trades live). Fix 4: clip floor and daily budget shipped; the friction gate was built (`a31118a`) but `ENTRY_EDGE_FRICTION_MULT` is still 0. Fix 5 shipped (`6c4423b`); relaunch.sh now allows `MAX_FAMILY_ENTRIES_PER_DAY=2`. The 10-19 review found the same thing again: the LLM book has no edge and friction is its loss. Open items are tracked in `gekko_improvement_tasks.md` (10-19 review section).
