# Forensics: the fleet was dead and the weather book was riding 97¢ to zero (2026-09-03)

Scope: the 46 closed trades in `runs/trades.jsonl` (08-26 → 09-02), the 427-proposal shadow
book in `gekko:shadow_proposals`, the live order books behind the 7 open positions, and
`logs/ollama.log` back to the 08-12 server start. Follows
[the 08-25 desk review](performance-review-2026-08-25.md) and the
[breen intelligence plan](breen-intelligence-implementation-plan.md).

**Verdict.** The reported +0.72% is two lottery tickets. Under them, three things: the LLM
fleet had been producing nothing at all for twelve hours; the weather book was watching
positions reach 97¢ and settle at zero because it structurally could not sell them; and the
persona re-activation bar was on course to pass on a single crowded oil trade counted
sixty-five times.

## Headline numbers

|                                    |            |
|------------------------------------|------------|
| Reported net P&L                   | +$724.38 (+0.72%) |
| ...of which two trades             | **+$1,158** (POLY TP +$714.62, KXHIGHNY-26AUG27-B80.5 +$443.03) |
| **The other 44 trades**            | **−$441** |
| Closed trades / win rate           | 46 / 17.4% (8 W, 38 L) |
| Fees, now genuinely charged        | $73.18 — but $37.90 of it on 4 KXPAYROLLS trades that lost $87 |
| Exits fired since 08-31            | **zero** |
| LLM proposals in the 6h run        | **zero** of 6,393 attempts |

`KALSHI_FEE_CHARGE=true` is live, so fix 1 of the 08-25 review is doing its job — and
immediately shows how badly the small mid-price clips are taxed. KXPAYROLLS handed back
43% of its loss to the venue.

## Finding 1 — Ollama had been dead since 04:27, and nothing noticed

`llm_proposals: 0`, `llm_fallbacks: 6393`, every event logging
`RAG pipeline produced no proposal (embed/Qdrant/LLM failure)`. Not a gate and not shadow
mode: the embedding call itself was failing.

```
04:27:27  GGML_ASSERT([rsets->data count] == 0) failed     ← llama.cpp Metal residency-set bug
                                                              (ggml-org/llama.cpp#17869)
          → every subsequent model load, for twelve hours:
          ggml_metal_library_init: "Unable to reach MTLCompilerService. The process is
          unavailable because the compiler is no longer active. Latest invalidation
          reason: Connection init failed at lookup with error 141 - Reentrancy avoided"
          → ggml_metal_init: failed to initialize the Metal library
          → GGML_ASSERT(backend) failed → llama runner terminated, exit status 2
```

One crash under memory pressure (free 2.2 GiB at the time) poisoned the `ollama serve`
process's Metal state, and it never re-established the compiler XPC connection. The server
had been up since 08-12 — 22 days — so nothing ever restarted it. Every gemma persona
produced literally nothing; weather, which does not use the RAG path, was the only book
still trading.

Fixed by restarting `ollama serve`, preserving its original `OLLAMA_FLASH_ATTENTION=1`
and `OLLAMA_KV_CACHE_TYPE=q8_0` (the KV quantization matters on a 16 GB machine, and it is
memory pressure that triggers the assert in the first place). Crash log preserved at
`logs/ollama.log.crashed-20260903`. Embeddings and `gemma3:4b` verified, and gordon was
back on the tape within two minutes.

**Left undone.** Nothing alerts on this. `personas_reachable: 4` was green throughout,
because the HTTP endpoint answers fine while every model load fails. A health check that
watched the ratio of `llm_proposals` to `llm_fallbacks`, or simply alerted on
`llm_proposals == 0` over a window, would have caught it in minutes rather than twelve
hours. Worth doing before the next unattended run.

## Finding 2 — the weather book could not sell, and rode 97¢ marks to zero

Ten of the 38 resolved weather positions reached a peak mark of **0.97 or better**. Six of
those ten settled at **$0.00**.

```
KXHIGHMIA-26AUG27-B93.5   entry 0.280  peak 0.985  →  0.00   -$14.71
KXHIGHNY-26AUG28-B84.5    entry 0.150  peak 0.985  →  0.00    -$7.95
KXHIGHLAX-26AUG28-B86.5   entry 0.190  peak 0.985  →  0.00   -$10.04
KXHIGHAUS-26AUG28-B101.5  entry 0.410  peak 0.985  →  0.00   -$21.35
KXHIGHCHI-26AUG30-T86     entry 0.890  peak 0.985  →  0.00   -$86.10
KXHIGHAUS-26AUG27-B103.5  entry 0.425  peak 0.975  →  0.00   -$44.22
```

These are intraday daily-high *bands*. Mid-afternoon the temperature is sitting inside the
bucket, so the market bids it to 98¢ — then the high climbs one more degree into the next
band and the contract is worthless. The market at 98¢ is systematically wrong about that,
and selling into it is the trade.

### 2a. Two-sided books were the wrong test for an exit

`markPriceLive` required a fresh **two-sided** book before any exit could fire, on the
reasoning that "a one-sided quote is good enough to VALUE a position but not to fire an
order against, because there is no counterparty on the side the exit would have to cross."
That is right for an ask-only book and backwards for a bid-only one. Every position here is
long, so an exit crosses the **bid**; the ask side has no say in whether it can happen.
Grading both cases as `quoteOneSided` conflated the two opposite situations:

- **asks only, no bid** — genuinely unexitable. Correctly refused, before and now.
- **bids only, no ask** — a contract the market wants so badly nobody will offer it.
  Maximally exitable, and refused anyway.

The second is not a corner case; it is what a weather bucket looks like once the
temperature is sitting in it. Live at 16:55 on 09-03:

```
KXHIGHCHI-26SEP03-B92.5   entered 0.150 × 150 lots
book: 34,616 contracts bid at 0.99, zero asks      ← $126 of deep, executable exit
```

New `exitQuote` / `bidFromBook` in `breen/account.go`: the exit map is now built from the
best bid rather than a two-sided mid, using the same staleness bound as `quoteFromBook` so
valuation and exit never disagree about whether a book is live. Valuation is untouched —
`observed` still takes one-sided quotes of either kind, which is what the 08-30 fix was for.

**Watch item.** This also changes the exit *price* from the mid to the bid. That is the
honest number — it is the fill you would get, it is the leg `SpreadCostUSD` already assumes
on the `DRY_CLOSED` path, and it is the same discipline as `resolveMark`'s realization
haircut. But it means percentage stops on the non-weather book now measure against the bid,
so a position whose book widens after entry will stop out sooner than it used to. The entry
gate refuses wide books at entry and `STOP_LOSS_MIN_ABS` still floors the move, but this is
the one thing to watch as the LLM fleet comes back.

### 2b. Settlement hold needed one exception, and only one

`SETTLEMENT_HOLD_PERSONAS=weather` (08-25 desk review fix #2) skips every mark-based exit,
correctly: a 23¢ contract wobbling to 19¢ says nothing about tomorrow's station high, and
the review priced managing that as −$428 actual vs +$2,950 held.

But there is one mark that is not noise: the one where the market has come all the way over
to our side. At a 95¢ bid the contract is priced as near certain, leaving 5¢ of upside
against 95¢ of downside. Holding for that last nickel is not collecting a settlement edge;
it is writing a very short-odds option on an outcome we have already been paid for.

New `settlementTakeProfit` in `breen/settlementhold.go`, knob `SETTLEMENT_HOLD_TP_PRICE`
(0 = off, **0.95 live as of 2026-09-03 20:01**). It is an **absolute price**, deliberately,
and that is what makes it compatible with the hold policy rather than a reversal of it: a
percentage limb asks how far the position has travelled from *our* entry, which is a fact
about us; this asks where the contract is trading, which is a fact about the market. The
desk review disabled the former for exactly the right reason and this does not re-enable it.

Modelled over all 38 resolved trades, reconstructing each peak from `mfe_pct`:

| sell at | fires | new gross | delta | winners cut | losers saved |
|---|---|---|---|---|---|
| 0.80 | 9  | +$285.44 | +$188.81 | −$55.46 | +$244.27 |
| 0.90 | 10 | +$432.50 | +$335.87 | −$25.46 | +$361.33 |
| **0.95** | **10** | **+$467.30** | **+$370.67** | **−$10.46** | **+$381.13** |
| 0.97 | 10 | +$481.22 | +$384.59 | −$4.46  | +$389.05 |

0.95 rather than 0.97 because the peaks cluster at 0.985–0.99 and a threshold pressed
against them buys a rounding error's worth of extra edge for a real loss of headroom. The
cost to the four genuine winners is $10.46 in total — a contract that reaches 0.99 pays
1.00, so the nickel is the whole downside, while not selling costs the entire position 60%
of the time.

Caveats, stated plainly: one sample of 38 trades over eight days; peaks are reconstructed
from recorded MFE, so a position is assumed to have been sellable at a level its mark
reached; and the model cannot see paths that crossed 0.95 on the way *up* and would have
been sold before a genuine run. That last one is bounded by the winners-cut column, which is
small precisely because these contracts do not stop at 0.95 — they go to 0.99 or to zero.

**First fill, 30 seconds after the restart:**

```
20:02:23 [TAKE-PROFIT] KXHIGHCHI-26SEP03-B92.5 closed @ 0.9900 (entry 0.1500, size 150,
         P/L $126.00) — bid 0.99 ≥ settlement take-profit 0.95 (entry 0.150, +$126.00)
         — the market has already priced the edge in
```

Both changes were load-bearing: 2a to see the bid at all, 2b to act on it.

## Finding 3 — the persona bar would have passed on one crowded trade

The published scorecard read 102/200 scored, +1.73% expectancy, gemma-bear +11.98%. Scoring
the 427-proposal book by contract series against live books tells a different story:

```
KXBRENTW      65 props   +$3,118   +18.3%      ← all of it
KXU3          13          -$234    -20.3%
KXGDP          8          -$487    -21.9%
POLY           5          -$363    -15.0%
KXPAYROLLS     4          -$272    -28.4%
KXGDPNOM       4          -$167    -22.9%

gemma-bear on Brent:   19 props  +$2,034   +33.0%
gemma-bear elsewhere:  15 props    -$539   -20.4%
```

118 actionable proposals span **27 distinct contracts**, 65 of them one weekly Brent series.
One directional oil move that happened to be right, re-proposed on every tick as it went the
persona's way, is not 65 pieces of evidence — but the bar counted proposals, so the fleet
was on course to be re-activated on it.

The bar now has a breadth half (`SHADOW_BAR_MIN_CONTRACTS`, default `SHADOW_BAR_PROPOSALS/4`
= 50, **live**), and two clauses hanging off it:

- **Breadth.** The scored rows must span at least that many distinct contracts. "No more
  than four bites at the same apple, on average."
- **Robustness.** Expectancy must still be positive with the single best-performing *series*
  struck out. This is the 08-25 review's own instinct ("still +$878 with the five best
  excluded") turned into a rule. On the current book: strike KXBRENTW and it is −$1,624.

Both hang off the one knob, so `SHADOW_BAR_MIN_CONTRACTS=0` restores the previous
count-and-edge bar exactly. The scorecard now also publishes `per_family`, sorted by how
much of the book each series moves, and the `[SHADOW-SCORE]` log leads with it — the
concentration story should never need looking for.

Note `shadowSeries()` is deliberately coarser than `contractFamily()`: the latter keeps the
event date, so it would read `KXBRENTW-26SEP0417` and `KXBRENTW-26SEP1117` as unrelated
evidence when they are the same weekly oil trade re-expressed.

**Still open — gemma-neutral cannot pass any actionable bar.** 177 of its 186 emissions are
sizeless bearish signals; it has 9 actionable proposals in six days. As written, no bar
counting actionable proposals can ever re-activate it. That is a design question about what
a signal-only persona is *for*, not an arithmetic one, and it is unchanged from 08-30.

## Two smaller things, not fixed

**`gekko:positions` is only written by `syncToRedis`**, which fires on fills, exits and
recovery — not on the mark tick. So live `BestMark`/`WorstMark` never reach Redis between
trades, and the UI's excursion figures are whatever they were at the last trade. Closed-trade
MFE/MAE is stamped from in-memory state and is correct, so this is an observability bug
rather than a data one — but a breen crash would restore pre-crash excursions from the
snapshot.

**`priority_tickers` and `watched_tickers` are shared atomics** (`marketdata/health.go`)
written by `pollCycle` for *both* venues. The Polymarket cycle runs after the Kalshi one and
overwrites both counts with its own, which is why `priority_tickers` reads 0 while held
positions are in fact being prioritized. Cosmetic, but it made the held-ticker mechanism look
broken during this investigation, which cost time. Split them per venue.

## What changed, and how to put it back

Everything landed behind a knob, house style. The two that are **on**:

```
SETTLEMENT_HOLD_TP_PRICE=0.95      # 0 restores hold-to-resolution
SHADOW_BAR_MIN_CONTRACTS=50        # 0 restores the count-and-edge bar
```

The exit-side liquidity fix (2a) has no knob, because refusing to sell into a 34,616-lot bid
is not a policy anyone would choose. `scripts/relaunch.sh` now carries the reconstructed
launch command with both knobs and a comment explaining each.

## Next

1. **Watch the non-weather stops** for a run now that exits price at the bid (2a watch item),
   with the LLM fleet live again for the first time since 04:27.
2. **Alert on a mute fleet** — `llm_proposals == 0` over a window is a one-line health check
   and would have saved twelve hours.
3. **Re-read the shadow scorecard** after a few clean days. It is the first time the fleet
   will have run with working embeddings *and* an honest bar.
4. The 08-25 review's entry-calibration finding is still unactioned: the 20–50¢ band is a
   real edge and the sub-20¢ flow is lottery tickets paying vig. `MIN_ENTRY_PRICE=0.20` is
   live, so this should be visible in the next period's numbers — check it.
