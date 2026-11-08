# Forensics: 2026-10-19 relaunch → 2026-10-21 12:54

Scope: the 10-19 18:17 relaunch (`5682e19`: friction gate 2x, predictive weather off, gordon-default
trading paper again) and the current launch at 10-20 19:09 (uptime 17h45m). Sources: `runs/trades.jsonl`,
`runs/equity.csv`, `logs/{gordon,gordon-weather,breen}.log` sliced to this run, liveview `/api/proposals`,
Redis `gekko:*`, `scripts/gordon_review.py --since 2026-10-09`, and a replay of every settled gordon trade
on Kalshi 1-minute candles. All **paper trading**. Follows [the 10-13 report](forensics-2026-10-13.md) and the
10-19 review in `gekko_improvement_tasks.md`.

**Verdict.** No edge, as on 10-19. Gordon's losses are almost all fees, and the 10x size multiplies them.
Weather has been dormant since predictive went off: the cleared-bucket path has never fired, in any run.
None of the exit rules I tested would have helped. The levers that work are size and whether gordon trades at all.

## Headline numbers

|                  |                                                                                   |
|------------------|-----------------------------------------------------------------------------------|
| NAV              | $94,351.98 (−5.65% since the 10-09 reset); −$612 since 10-19 18:00, −$185 this launch |
| Drawdown         | 7.35% now, 7.54% max; breen's graduated sizing is at ×0.78; halt at 10%            |
| Since 10-19 18:00| 15 closes: 7 weather (all losers, −$951, the last predictive picks resolving), 8 gordon (−$463) |
| This launch      | 2 gordon closes (−$206, +$92), 1 open (KXFEDDECISION-26OCT-H25, 5,733 @ 0.12)       |
| Gordon funnel    | 3,981 activity looks → 3,248 pre-gate skips (1,911 spread cap) → 523 LLM declines, 307 edge skips → 21 proposals → **3 fills** |
| Errors           | 5 "no live order book", no panics                                                 |

## Finding 1: gordon has no edge, so its P&L is just fees

The 49 gordon-default trades since 10-09 come to **−$3,993 true P&L**, made up of **$3,094 fees + $709 spread**
and a pre-friction result of **−$189**. Median hold is 33 minutes, so every trade pays the 7% Kalshi fee
twice for a move the model didn't predict.

`gordon_review.py` (2,846 scored proposals on settled markets):
- Brier: LLM fair 0.2012 vs book mid 0.2032. That's level with the market (it was 0.2101 vs 0.2019 on 10-19).
- **First proposal per ticker (425 tickers): EV +$0.002/contract.** The headline "+$76k if all taken" comes
  from repeat counting (KXBRENTW is 220 rows on 6 tickers).
- EV by edge band is flat (+0.020 at 3–7¢, +0.040 at 7–10¢), so raising `MIN_EDGE` won't pick better trades.
- Confidence still doesn't predict outcomes: conf 0.6–0.8 wins 29%, conf <0.6 wins 45%.

Crypto is the *less* bad half now: pre-friction +$1,233 on 30 trades against −$1,423 on 19 non-crypto. The 10-19
"exclude crypto" item isn't supported by this sample.

## Finding 2: the 10x size is costly and blocks most proposals

`BET_SIZE_MULT=10` puts each order at ~$1,000 notional on thin books. Gordon's engine shrinks orders that walk
more than 3¢ through the book and flags them `limit_breached`, and breen (`RISK_REJECT_ON_BREACH=true`) then
rejects them. **This launch: 15 of 21 gordon proposals were breached and rejected**, every one on slippage
(0.06–0.55 abs). The uncommitted `controls.go` change (tier caps scale with the multiplier) fixed the
*size-cap* breach and is in the running binary (built 19:09 from the 19:08 edit; tests pass). The slippage
breach is a separate cap that the change doesn't touch.

With zero edge, size only scales the loss: about $63 in fees per trade at 10x. The size gate also leaves gordon
with very few fills (3 this launch), so the scoring takes longer to reach a useful sample.

## Finding 3: no exit rule rescues it (replay on real price paths)

I replayed the 36 settled gordon trades (Kalshi 1-min `yes_bid` closes, from the real entry to settlement):

| Exit rule                                   | P&L     |
|---------------------------------------------|---------|
| **Actual** (−15% stop, +40% TP, trail 20/10) | **−$2,791** |
| Hold to settlement, no stops                | +$562 (13/36 won; one fee instead of two) |
| Settlement-hold + breen's −$300 backstop     | −$7,094 |
| % stop −15 / −25 / −40 / −60, then settle    | −$5,635 / −$4,847 / −$7,506 / −$9,713 |

Adding gordon to `SETTLEMENT_HOLD_PERSONAS` looks good on paper (+$562 when held), but breen keeps its
dollar backstop. At 10x size, −$300 is ~25–30% of cost, which is inside normal daily-contract noise, so
9 eventual winners (WTI, CPICORE, BTC 81.75k) would have been cut at the bottom. Pure holding is +$562 on 36
trades with ~$1k swings per trade, which is zero within the noise. Leave exits alone until there's an edge.
(The replay uses minute bid closes, which gap lower than breen's mark, so it's somewhat harsher on stops
than live trading would be. The ranking would still hold.)

## Finding 4: weather is dormant, not hedged

Predictive is off (`WEATHER_PREDICTIVE=false`). The cleared-bucket path has logged **0** "cleared bucket
BUY" lines in the whole `gordon-weather.log` history, and all 68 weather trades ever were predictive.
The path only buys "X° or above" tails after the day's high has cleared them by 1°F, and by then Kalshi is
already at ≥0.97. The gordon-weather process now only re-embeds the catalog every ~35 min. The idea from 10-19
of fading longshots on the NO side is still the only weather hypothesis with a positive sign, and the system is
YES-only.

## Knobs

Ranked. Per the 10-19 rule, change one per relaunch and decide the sample size before judging it.

1. **`BET_SIZE_MULT=10 → 1`** (recommended). Cuts the burn ~10× and keeps most proposals inside the 3¢ slippage
   cap, so more of them fill and get scored. Judge after **100 first-per-ticker taken BUYs** in
   `gordon_review.py`, with the edge sign as the decision.
2. **Bench gordon** (`SHADOW_PERSONAS=gordon-default`). Stops the bleed entirely and the shadow
   scorecard keeps running. This is the 10-19 recommendation again; the evidence hasn't changed.
3. **Not recommended, with evidence:** settlement-hold for gordon (−$7.1k replay), wider or narrower stops
   (all worse), higher `MIN_EDGE` (EV flat across edge bands), excluding crypto (crypto is the less-bad half).

Housekeeping: commit the `controls.go` diff; the `relaunch.sh` comment says llama3.3:70b, but the fleet is
running `qwen2.5:32b` (run.sh default).
