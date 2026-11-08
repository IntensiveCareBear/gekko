# Ideas to Borrow: MrFadiAi/Polymarket-bot → Gekko

_Reviewed 2026-07-11. Source: https://github.com/MrFadiAi/Polymarket-bot (main, ~21k lines TypeScript). Companion to `harrier-toolkit-borrowings.md` — overlaps are noted rather than repeated._

## What the repo actually is

A Polymarket-only bot with three strategies — same-market structural arbitrage (`arbitrage-service.ts`), a flash-crash two-leg strategy on the 5/15-minute crypto UP/DOWN markets (`dip-arb-service.ts`), and copy trading with wallet analytics (`smart-money-service.ts`, `wallet-service.ts`) — on top of a clean client layer: CLOB, Gamma, Data API, subgraph, an on-chain CTF client with real `splitPosition`/`mergePositions` calls, and a WebSocket service built on the official `@polymarket/real-time-data-client`. Good integration test coverage; the deep docs are in Chinese (`docs/arb/arbitrage.md` and `docs/strategies/dip-arb-strategy.md` are the two worth reading).

**Caveat:** the README's headline v3.1 features ("4-layer risk protection", "dynamic position sizing", daily/monthly loss limits) do **not** appear in the source — no daily-loss, streak-sizing, or halt logic exists anywhere in `src/`. Treat the README as marketing; the code is the reference. Gekko's breen risk stack is far ahead of anything actually implemented here.

## What gekko already does as well or better

No action needed on: the whole risk-guard stack (drawdown halt, family loss limits, category caps, cooldowns, force-exit, payoff-skew sizing), TP/SL sweeps (`TAKE_PROFIT_PCT` landed post-Harrier), rate limiting (marketdata's per-client limiter covers what their Bottleneck registry does), WebSocket ingestion (kahuna), momentum + orderbook-imbalance features (`marketdata/momentum.go`, gordon), impact-modeled slicing, and everything LLM/RAG/learning — this repo has no counterpart to Gordon or Lou.

## Ideas worth borrowing

### 1. Mirror-book effective pricing (small, sharpens every Polymarket trade)

The core insight documented in `docs/arb/arbitrage.md`: **buying YES @ p is identical to selling NO @ (1−p)**, so every binary market has two order books quoting the same asset. Their `getEffectivePrices` always takes the better of the two:

- effective buy-YES = min(YES best ask, 1 − NO best bid)
- effective sell-YES = max(YES best bid, 1 − NO best ask)

`marketdata/polymarket.go` currently records **only the YES token ID** and polls only that book — gekko literally cannot see half the liquidity. Concretely: fetch/subscribe both outcome tokens per market, compute effective prices in the poll cycle, and use them for marks, `entryBookBlock`, and execution routing (if the NO side is cheaper, express the YES view by selling NO). This improves every fill and mark on Polymarket regardless of strategy, and it's a prerequisite for idea 2.

### 2. Same-market structural arbitrage with CTF merge/split (new deterministic lane)

`arbitrage-service.ts:checkOpportunity` + `ctf-client.ts`:

- **Long arb:** when effective buy-YES + buy-NO < $1 − threshold, buy both, then call `mergePositions` on the CTF contract to redeem the pair for $1 immediately — no waiting for resolution, no resolution risk.
- **Short arb:** when effective sell-YES + sell-NO > $1 + threshold, `splitPosition` $1 collateral into a YES+NO pair and sell both.
- Sizing details worth keeping: size = min(top-of-book size both legs, balance-implied size, max) × a **safety factor** (default 0.9) so both legs can actually fill; a min-size floor; FOK orders on both legs.

The on-chain merge/split capability is the piece gekko lacks entirely (broker-worker only places CLOB orders). It needs a Polygon wallet + the L2-auth fixes already flagged in the Harrier doc, but the payoff is a risk-free-by-construction lane that breen barely has to guard. Their gas-estimation helpers in `ctf-client.ts` (lines ~720–750) show the gas-cost accounting needed to set the profit threshold honestly.

**Gekko-specific extension neither repo has:** the same sum-to-$1 math applies **cross-venue** — Kalshi YES + Polymarket NO on an equivalent event. Gekko is uniquely positioned since it already trades both venues; harder (no merge escape hatch, resolution-criteria drift between venues), so treat as research after the single-venue lane works.

### 3. DipArb: flash-crash two-legger on 5/15-min UP/DOWN markets (new strategy lane)

The most original thing in the repo (`dip-arb-service.ts`, 2.3k lines, well documented). Mechanics:

- **Signal:** a price-history ring buffer per token; trigger when best-ask drops ≥15% within a **3-second sliding window** (`slidingWindowMs` — explicitly framed as the core parameter: 15% in 3s = panic/order-book failure, 15% in 5min = trend, don't touch). A `surgeThreshold` mirror catches spikes and buys the *opposite* side.
- **Leg1:** buy the crashed side at market × (1 + 2% slippage cap), only within the first `windowMinutes` (2) of a round.
- **Leg2:** wait for the opposite side's ask such that leg1 + leg2 ≤ `sumTarget` (0.92) — locking ≥8% regardless of outcome; timeout after 3 minutes and exit the naked leg.
- **Settle:** auto-merge the pair back to USDC (via idea 2's plumbing) rather than waiting for resolution.
- Execution hygiene: per-round open-price baselines, execution cooldown, optional order splitting.

Why it fits gekko: it's a deterministic fast lane (like the Harrier copy-lane idea) that needs no LLM — kahuna already has the Polymarket WSS feed, and breen's guards slot in front unchanged. The 3s-window ring buffer is exactly the same in-memory structure as the Harrier flow-anomaly breaker, so the two share an implementation. Their hard-won defaults (sumTarget relaxed 0.95→0.92 for fill rate, leg2 timeout cut 300s→180s to shed naked legs faster) are worth keeping as starting values.

### 4. Settlement-oracle price feeds for crypto markets (new kahuna source, cheap)

Two related feeds gekko has neither of (kahuna has Finnhub/FRED/weather but no crypto spot). 2026-10-19: this matters on Kalshi too. Half of gordon's trades are `KXBTCD`/`KXETHD` daily thresholds priced from headlines; a spot+vol fair is the fix (`gekko_improvement_tasks.md` 10-19 review):

- **`crypto_prices` topic on Polymarket's own WSS** (`realtime-service-v2.ts`) — streams the **Chainlink prices the UP/DOWN markets settle against**. For any crypto market, spot-vs-price-to-beat plus time-remaining gives a directly computable fair value; contract price minus fair value is a real edge signal, not a sentiment proxy. Free — it's on the socket kahuna already connects to.
- **Binance klines/ticker** (`binance-service.ts`) — spot typically leads the Chainlink update, so it's the early-warning input for DipArb's leg timing and a general momentum feature for Gordon on any crypto-category market (which the post-mortem's category work already identifies as a correlated bucket).

Suggested shape: `kahuna/ingest/cryptoprices.go` publishing to Kafka like the existing feeds; Gordon gets `fairValue`/`oracleDelta` features stamped on crypto-market events.

### 5. Wallet quality scoring for the copy lane (upgrades Harrier idea 2)

Harrier supplied the fast on-chain fill feed; this repo supplies the **vetting layer** that decides who deserves to be copied — and it needs only the public Data API (REST), so it can ship before any Polygon infra:

- Composite 0–100 score (`wallet-service.ts` ~line 1049): PnL 40%, win rate 30%, **consistency 20% (variance of per-market PnL — filters one-hit wonders whose profit is a single lucky whale bet)**, activity 10%.
- Recommendation gates (`smart-money-service.ts` ~1879): copy only at win rate ≥60% AND profit factor ≥1.5; 50%/1.2 → "follow at reduced size"; below → don't.
- Copy-execution options worth keeping (`AutoCopyTradingOptions`): `sizeScale`, `maxSizePerTrade`, `minTradeSize` (ignore the whale's dust), `sideFilter` (e.g. copy exits only), per-trade `delay`, FOK/FAK choice, dry-run.
- `categorizeMarket` + per-category wallet stats answer "is this wallet only good at sports?" before mirroring its politics trades.

Fits lou's territory: the same scoring loop gekko runs on its own trades, pointed at external wallets. A periodic leaderboard-scan job producing a vetted-wallet list in Redis is the natural first step; the Harrier on-chain feed then only fires for wallets on that list.

### 6. Small utilities

- **Exit-capability floor:** Polymarket's minimum order is ~$1, so a position (or post-partial-fill remnant) below it is *unsellable until resolution*. They enforce every position ≥ $1.50 at open. Gekko's min-order floor guards entry EV; this is a different failure — add a check that sized-down orders (payoff-skew, near-expiry 0.25×) never fall below the venue minimum, and treat partial fills that leave sub-minimum dust as force-merge/exit-at-any-price cases.
- **Tick-size-aware rounding** (`price-utils.ts`): direction-aware rounding (floor for sells, ceil for buys) against the market's actual tick size (0.1/0.01/0.001/0.0001 — it varies per market and can *change* via `tick_size_change` WSS events, which they handle). Broker-worker currently assumes a fixed price grid.
- **Market microstructure signals** (`market-service.ts:detectSignals`): gekko has momentum + imbalance already; the missing one is **whale-print detection** off the public `activity` WSS topic — a zero-infra preview of the Harrier Polygon feed.

## Suggested order

1. Mirror-book pricing in marketdata (both token IDs + effective prices) — improves everything, blocks nothing.
2. Tick-size handling + exit-capability floor in broker-worker/breen — small correctness fixes.
3. `crypto_prices`/Binance kahuna source → Gordon fair-value feature — cheap, direct edge on crypto markets.
4. CTF merge/split client + structural-arb lane — needs the Polymarket broker fixes from the Harrier doc first.
5. DipArb lane — after 3 and 4 (uses the oracle feed for timing and merge for settlement).
6. Wallet-scoring job → vetted-wallet list — pairs with the Harrier whale feed whenever that lands.

## Housekeeping note (repeat)

`gekko.txt` (bare RSA private key), `kalshi_api_key.txt`, and `GEMINI_API_KEY.txt` still sit in the repo root — flagged in the Harrier doc, still unrotated/unmoved.
