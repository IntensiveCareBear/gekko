# Forensics: 2026-07-13 (+$814) vs 2026-07-14 (−$167 realized)

Run started 2026-07-13 10:19. As of 11:44 today: NAV $99,808 (−0.19%), realized −$728, unrealized +$537, 6 open positions. Benchmark NAV $100,932 — the book trails its benchmark by ~1.1%.

## Day-over-day

|                | 07-13 | 07-14 (to 11:44) |
|----------------|-------|-------|
| Trades closed  | 14    | 17    |
| Realized PnL   | +$813.71 | −$166.58 |
| Wins / losses  | 7 / 7 | 4 / 13 |
| Stop-losses    | 5     | 6 (+1 LOSS_LIMIT) |
| Median hold    | 287 min | 74 min |

**Yesterday wasn't broadly good** — it was one family: KXBRENTW +$748 of the +$814. Ex-Brent, yesterday was ~+$65. **Today isn't broadly bad** — Brent is still +$146 today; the damage is KXGDP −$185 (two sibling strikes T2.0/T2.5 stopped out 25 min apart — same macro event, double exposure) and POLY churn −$94. The "good day → red day" swing is mostly Brent variance plus two new failure modes below. Median hold collapsing 287→74 min is the churn signature.

## BUG 1 (new, critical for the learning loop): persona PnL attribution inflated ~9x

Across the run, `persona_scores` attribute **−$6,531** of PnL to personas while the book actually realized **−$728**. 234 of 265 trades have persona-sum ≠ trade PnL beyond tolerance. Three mechanisms, all in `breen/`:

1. **Consensus shrink not propagated** — `consolidate` (consensus.go) sizes the executed order at `baseSize × mult` (mult floors at 0.5), but stores each `ContributorProposal` at its **original** proposed size. `personaScoresForClose` (account.go) then scores the persona on the unscaled size → a single-contributor order with w×c≈0.5 credits the persona ~2× the real PnL. (First trade of the run: trade −$9.02, persona scored −$17.22.)
2. **Buying-power cap not propagated** — tracker.go caps `filledSize = affordable`, but `contributorProposalsForOpen` returns `p.ContributorProposals` untouched.
3. **Partial closes never shrink proposals** — on partial exit `pos.Size -= sellSize` but `ContributorProposals` keep full size, so each subsequent close scores `proposalSize × (sellSize/currentSize)` — inflation grows every cycle. Observed 07-08 POLY sequence: trade PnLs +5.65/+4.67/+3.27/+1.82 scored as +10.22/+13.34/+17.90/+27.88.

**Blast radius:** `computePersonaStats` scorecard, Redis `gekko:persona_stats`, the persona kill-switch weights (`personaWeights` derive from this performance → personas get benched on phantom losses), and lou's persona return-on-capital analyzer. `allocator.Observe` uses ReturnPct (persona's own entry vs exit) so the bandit is less poisoned, but the Cost/PnL fields it logs are wrong.

**Fix:** scale proposal sizes by `filledSize / Σ proposalSizes` at open (both consensus and cap paths), and scale remaining proposals by `(pos.Size − sellSize) / pos.Size` on partial close.

## BUG 2: sell/re-buy churn on a held family bypasses the cooldown

`riskGate`'s re-entry cooldown (30 min) only applies when `!familyHeld` — adds while holding are exempt by design. Combined with direct (non-consensus) SELL exits, this allows a flip-flop loop: today gemma-neutral **sold** POLY-25312457… five times 09:36–10:47 (book-imbalance signal) while gemma-neutral/gemini-bull **bought it back** at 11:08/11:09/11:32 (sentiment +1.00 signal), each round trip paying the ~2% bid-ask spread. −$41.6 on that one market, and the loop was still running at snapshot time (position rebuilt to 4,809 contracts, family exposure pinned at the $1,996 cap).

Same persona, opposite signals, 20 minutes apart — there is no netting between a persona's exit conviction and its (or a sibling's) re-entry conviction, and no hysteresis. Options: apply the family cooldown to BUYs for N minutes after **any** exit (even partial), or require re-entry edge > round-trip spread, or net buy/sell proposals within the consensus event window.

## Contributing weaknesses (both days, not day-specific)

- **Sentiment signal is saturated:** ~80% of all sentiment readings are exactly +1.00 both days (naive lexicon scorer clips at ±1). A constant +1 is what kept re-arming the POLY re-buys. Already on bug_list.md as an improvement; today it had a dollar cost.
- **Reddit feed is effectively dead:** kahuna hit 429 escalation to a 5h20m backoff (level 6) twice today; yesterday logged 1,945 Reddit 429 warnings. Sentiment is running on Finnhub headlines alone (which also had 8 timeouts + 5 × 502 yesterday).
- **Stops fire late on stale books:** stop-loss is 15% but only acts on LIVE marks; KXGDP-T2.5 realized −27.2%, KXNASDAQ100 −20.0%, KXHIGHNY −20.4%. Thin overnight/morning books let positions bleed past the stop before a fresh book arrives.
- **Weather strategy (KXHIGH*) is net negative:** 9 trades, −$48 whole-run; stopped out in NY both days (same B92.5 strike re-entered today after yesterday's stop — family cooldown allows it after 30 min).
- **Sibling-strike stacking:** KXGDP T2.0 + T2.5 held simultaneously — family exposure cap shares a budget but nothing prevents holding multiple strikes of the same event in the same direction; both stopped together.

## Fixes shipped (2026-07-14 12:14)

- **Bug 1** — `personaScoresForClose` (breen/account.go) now normalizes each persona's score to its proportional share of the quantity actually closed, so per-persona PnL sums to the trade's PnL. One change covers all three inflation mechanisms (consensus shrink, buying-power cap, partial closes). Regression test: `TestPersonaScoring_NormalizesToClosedSize`. Note: PersonaScores already baked into stored ClosedTrades keep their old inflated values; the scorecard fully cleans up at the next run reset.
- **Bug 2** — `riskGate` (breen/tracker.go) re-entry cooldown now applies after ANY family exit, full or partial, held or not — the still-held exemption that enabled the POLY sell/re-buy loop is removed. Test updated in `riskguards_test.go`.
- **Fix 3 (sibling-strike stacking)** — new `OneStrikePerFamily` guard (env `ONE_STRIKE_PER_FAMILY`, default on) in `riskGate`: rejects a BUY on a family strike while a *different* strike of that family is already open. The run is YES-only, so two strikes of one event are the same directional bet; the family notional cap bounded their combined size (KXGDP T2.0+T2.5 = $1,034 < the ~$2,000 cap) but still let them stack and stop out together. Adds to the *same* strike still pass. Test: `TestRiskGate_OneStrikePerFamily`. Existing sibling positions are grandfathered — the guard only blocks new stacking; they can still be sold.
- Deployed: breen_bin rebuilt, process bounced under the run.sh supervisor, state reloaded clean from Redis each time.

### Verification note on Fix 1
The first post-fix close (the accumulated POLY-25312457… position) scored −$846.75 in persona PnL against a −$826.09 trade — a 2.5% residual, versus the ~9x inflation before. The residual is genuine proposal-vs-fill price differentiation (each persona scored on the price it proposed, not the actual top-of-book fill), which is the intended per-persona differentiation, not size inflation. Forcing exact summation would require scoring every persona at the position's blended fill price, which erases the differentiation `personaScoresForClose` exists to provide.

## What's working

- Brent (KXBRENTW): +$894 across both days — the one persistent edge; take-profits banked +$198 overnight.
- Risk plumbing did its job where wired: family exposure cap correctly rejected the 11:08/11:09/11:34 POLY adds; LOSS_LIMIT closed the POLY-105473… family at −$56.
- No crashes, panics, or execution errors in breen/gordon/executive-core/broker-worker either day.
