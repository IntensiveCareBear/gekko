# Ideas to Borrow: HarrierOnChain/Prediction-Markets-Trading-Bot-Toolkits → Gekko

_Reviewed 2026-07-11. Source: https://github.com/HarrierOnChain/Prediction-Markets-Trading-Bot-Toolkits (main, ~3.2k lines Rust)._

## What the repo actually is

The README advertises ten strategies, but nine of the `src/bot/*` files are 12–16 line stubs pointing at per-venue repos. The substance is one production-quality **copy-trading pipeline** for Polymarket plus a well-built service layer: `clob.rs` (EIP-712 signing + L2 HMAC auth), `order_executor.rs`, `risk_guard.rs`, `position_monitor.rs` (TP/SL), `market_cache.rs`, `eligibility.rs`, and `onchain.rs` (Polygon WebSocket log subscription). That service layer is where the borrowable ideas live.

## What gekko already does as well or better

No action needed on: drawdown circuit breaker, per-family loss limits and exposure caps, re-entry cooldowns, category exposure caps, spread/staleness entry blocks (`entryBookBlock`), impact-modeled order slicing (`gordon/impact.go`), force-exit before resolution, dry-run mode, payoff-skew sizing, and the whole LLM/RAG/learning layer (which Harrier has nothing like). Harrier's "two-phase risk check" (I/O-free fast gate, book fetch only when needed) is a pattern gekko already follows with `RiskState` pre-LLM vetoes.

## Ideas worth borrowing

### 1. Take-profit exits (highest value, lowest effort)

Gekko has stop-losses (`STOP_LOSS_PCT` / `STOP_LOSS_USD` / `STOP_LOSS_MIN_ABS` in `breen/account.go:checkStopLosses`) but **no take-profit path at all** — winners only exit via a Gordon SELL proposal or the near-expiry force-exit. Harrier's `position_monitor.rs` polls midprice per open position and fires an exit when unrealized P&L crosses *either* threshold, with per-category TP/SL overrides.

Run #2's own data argues for this: entries <0.20 won 68% — those winners ride to resolution risk when they could bank, and mean-reverting markets routinely give back gains. Borrow:

- Add `TAKE_PROFIT_PCT` (and optionally `TAKE_PROFIT_USD`) alongside the stop-loss checks in the same `checkStopLosses` sweep — the mark-cache plumbing already exists.
- Stamp TP/SL thresholds **onto the position at open** (Harrier's `OpenPosition` carries `take_profit_pct`/`stop_loss_pct`), so Gordon or per-category config can vary them per trade instead of one global env var.
- Route the exit through the normal executor path (Harrier: "same signing path, same risk gates"), which breen already does.

### 2. On-chain whale-flow feed for Polymarket (new signal source)

Harrier's flagship trick (`onchain.rs` + `copy_trading.rs`): subscribe to Polygon via WebSocket `eth_subscribe`/`logs` with a **server-side filter** — CTF Exchange contract addresses, topic0 = `OrderFilled`, watched wallet packed into topic2 (maker) — so the process only wakes when a tracked wallet actually trades. They claim 3–30s lead over Polymarket's public positions API. Decoded fills become structured events (token, side, shares, price, notional, tx hash).

For gekko this is a natural **kahuna ingest source** (`kahuna/ingest/polygon_fills.go`) publishing to Kafka like the existing feeds. Two uses, in order of ambition:

- **Signal feature:** Gordon gets "smart-money flow" per market — aggregate net whale buying as a momentum/confirmation input, exactly the kind of feature the proposal-quality work needs (the post-mortem's remaining lever).
- **Copy lane (later):** a deterministic fast path that mirrors proven wallets with breen's risk gates in front. Harrier's sizing menu is worth taking with it: percentage / fixed / **adaptive** (copy percent decays smoothly in log-space as whale notional grows — small conviction bets copied aggressively, huge bets copied cautiously).

Their reconnect loop (exponential backoff 1s→30s, reset on clean close) is standard but worth copying verbatim.

### 3. Depth floor + market-behavior circuit breaker

Gekko's breakers all key on **our own P&L** (drawdown, family loss limits). Harrier's `risk_guard.rs` adds two guards keyed on **market behavior**:

- **Depth guard:** before any order, top-5-levels depth in USD on the relevant side must exceed `min_depth_usd`. `entryBookBlock` checks spread and staleness but not dollar depth — a penny-wide book with 3 contracts on the ask passes today. Add a `MIN_BOOK_DEPTH_USD` limb; the order book is already in hand at that call site.
- **Flow-anomaly trip:** N consecutive large trades on one token inside a rolling window trips a cooldown on that market — cheap protection against wash-trading/manipulation bursts and news shocks arriving faster than the LLM loop can reprice. Pure in-memory ring buffer, no I/O (fits the riskGate "stays I/O-free" constraint).
- **Fail closed:** when a book fetch fails mid-check, Harrier trips the breaker rather than trading blind ("prefer tripping to firing blind"). Worth auditing gekko's fallback paths for the same posture — `requireLive` covers the entry case, but exits and marks fall back to cached/haircut values.

### 4. Metadata-driven eligibility and per-category config

Gekko's `contractCategory` is a hardcoded keyword table over ticker strings — the improvement doc itself flags it ("a config-driven map or Gordon-stamped tag could refine it later"). Harrier resolves **real venue metadata** (Gamma API categories + tags, TTL-cached in `market_cache.rs`) and runs everything off it:

- Allow/blocklists by category, tag, and market slug, with clean semantics: blocklists always win; if any allowlist is populated the filter turns strict (allowlist-only); case-insensitive matching.
- Per-category exposure caps and per-category TP/SL thresholds keyed on the same metadata.

For gekko: have `marketdata` fetch and cache category/tags per market (Kalshi series metadata, Polymarket Gamma), stamp them onto events, and let breen's category caps + the new TP/SL read them. This replaces the heuristic keyword table, closes the "cross-series correlation" gap properly, and gives an operator kill-switch per category ("no more crypto dailies") without a redeploy.

### 5. Polymarket broker correctness (use their `clob.rs` as the reference)

Comparing `broker-worker/polymarket.go` against Harrier's working client surfaced real gaps — worth fixing before any wet Polymarket run:

- **No L2 auth headers.** The order POST sends only `Content-Type`; Polymarket's `/order` requires L2 HMAC headers (key/signature/timestamp/passphrase). Harrier derives API creds via the L1 EIP-712 sign-in flow at startup, then HMAC-signs each request (`l2_headers`). Current gekko orders would be rejected.
- **Token ID is fabricated.** `tokenID = Keccak256(contractID)` — real CLOB token IDs are the ERC-1155 outcome-token IDs from Gamma, not a hash of your internal string. Resolve them via the metadata cache from idea 4.
- **Hardcoded verifying contract, no neg-risk handling.** Harrier switches between the CTF Exchange and the Neg-Risk Exchange per market; multi-outcome (neg-risk) markets signed against the wrong contract will fail.
- **Order-type discipline.** Gekko sends everything GTC with 24h expiration — a resting order that can fill hours later at a stale price with nothing managing it. Harrier uses **FAK for entries** (take available liquidity, cancel the rest — consistent with gekko's take-liquidity style) and **GTD with a short configurable expiration for exits**, plus a limit price of signal±buffer clamped to [0.01, 0.99].

### 6. Small utilities

Price clamp + buffer helpers (`clamp_price`, entry pays up by `price_buffer`, exit gives ground by it — makes fills deterministic in dry-run replays), midprice endpoint (`/midpoint?token_id=`) as a cheap poll target instead of full book fetches for mark-to-market, and a minimum-order floor framed explicitly as a **negative-EV micro-trade guard** (fees + spread eat sub-$5 orders).

## Suggested order

1. Take-profit in `checkStopLosses` — small, config-gated, testable in the existing `riskguards_test.go` style.
2. Depth floor in `entryBookBlock` — one new limb, book already fetched.
3. Metadata pipeline (categories/tags) — unlocks 1's per-category thresholds and replaces the keyword table.
4. Polymarket broker fixes — prerequisite for any live Polymarket trading; port Harrier's clob.rs recipe.
5. Polygon whale-flow kahuna source → Gordon feature — the biggest new-alpha item, and independent of the rest.
6. Flow-anomaly breaker + copy lane — after the feed from 5 exists.

## Housekeeping note

`gekko.txt` at the repo root is a bare RSA private key, and `kalshi_api_key.txt` / `GEMINI_API_KEY.txt` sit next to it. If this repo ever gets pushed anywhere, rotate those and move them out of the tree (Harrier's config split — committed `config.json` for settings, gitignored `config.yaml` for credentials — is a reasonable pattern).
