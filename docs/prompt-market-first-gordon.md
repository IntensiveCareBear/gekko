# Prompt: make Gordon market-aware (what's trading on Kalshi / Polymarket)

Paste everything below the line into a fresh session opened in the gekko repo root.

---

You are working in the Gekko repo, a paper-trading system for prediction markets (Kalshi, plus
Polymarket data). Go services talk over Kafka and Redis; everything runs through `./scripts/relaunch.sh`
(wrapper around `./run.sh`). The book is paper only (`dry_mode: true`).

## The problem

Gordon (`gordon/`, persona `gordon-default`, Llama 3.3 70B via Ollama) is **news-first, not
market-first**. Every ingested event (news, FRED, Reddit, Finnhub; Twitter is dead) triggers one pass:

1. embed the event text (`nomic-embed`)
2. `marketMatcher.Match` finds the closest Kalshi market by cosine similarity (`gordon/main.go` ~710,
   `gordon/markets.go`). Text similarity only.
3. `[VERIFY]` year/month/threshold check, `[PREGATE]` skips (one-sided book, near expiry, cooldown, family limits)
4. LLM call: event + top-3 similar past events from Qdrant + the matched market's title and bid/ask.
   Returns BUY/SELL/decline plus a "fair value" probability and size.
5. `[EDGE]` fair value vs ask, `[SKEW]`, `[EXPIRY]`, signal enrichment (momentum, book imbalance, sentiment)
6. liquidity-impact sizing, publish to Kafka, then breen's risk gates and a dry-run fill.

A market is only ever considered if some headline happens to embed near it. Nothing looks at what is
actually trading: volume, spread, open interest, price moves, new listings, markets about to resolve.
Evidence from the 10-01 runs: ~390 LLM calls produced 7 proposals and 2 fills in 5 hours. The fills were
low confidence (0.24, 0.37) and many proposals hit the same few contracts repeatedly (KXFM30YMTG 20
evaluations, KXPAYROLLS 18). The 20:50 Brent BUY was rejected at fill by breen's spread cap
(0.24/0.27 book, 12% of mid vs a 10% cap) after the LLM had given it confidence 1.00 off "sentiment
+1.00, momentum +8.5%". The LLM's fair value is a guess with no independent model behind it.

The goal is for the system to know what is trading on Kalshi and Polymarket and let that shape what
Gordon looks at, not only what news arrives. Treat "make Gordon market-first" as the problem to
explore, not a spec.

## What already exists (verify each before relying on it)

- `marketdata/` (`kalshi.go`, `main.go`): read-only poller. Discovers ~796 Kalshi markets across ~222
  series, publishes the catalog to Redis `gekko:kalshi_markets` (and `gekko:polymarket_markets`), writes
  `market:*`, `orderbook:*`, `momentum:*` keys, rotates the whole universe every ~2-3 min and refreshes
  held/watch/hot tickers every cycle (`priority_tickers`). Polymarket is public Gamma + CLOB, no key.
- `gordon/markets.go`: the matcher. Embeds new Kalshi markets into a per-persona Qdrant collection
  (`kalshi_markets` for gordon-default), prunes ones that left the catalog.
- `gordon/priority.go`, `gordon/signals.go`, `gordon/impact.go`: priority weights, momentum/imbalance
  signals, liquidity-impact model.
- `breen/`: risk gates (spread cap `entry_max_spread_pct` 0.10 / abs 0.02, one strike per family, round-trip
  caps, mid-band confidence, force-exit 1h before expiry, stop-loss) and the paper account.
- `kahuna/`: ingestion. Its own Kalshi websocket source is **not** used: `KALSHI_SIMULATED` defaults true,
  it is skipped at startup, nothing consumes its events, and the dialed URL has no auth. Do not mistake the
  empty Kalshi card on the kahuna monitor for a Kalshi outage; Kalshi is reachable and marketdata is clean.
- `gordon-weather` is a separate deterministic path (METAR + NWS forecast + per-station bias, no LLM) and
  is already market-aware per bucket. Leave it alone.

## Your first job: investigate, then propose. Do not start coding.

1. Read the code paths above end to end. Confirm or correct the pipeline description. List what market
   data marketdata already collects (fields per market and per book) and what Gordon actually reads.
2. Pull a live snapshot from Redis (`docker exec gekko-redis redis-cli`) and the running services'
   `/healthz`: how many markets, which series, what the spreads, volumes and two-sided-ness look like
   across Kalshi and Polymarket. How many markets are actually tradeable under breen's entry gates
   (two-sided book, spread under 10% of mid or 2c, 2h to 48h to expiry, price band 0.20-0.55)?
3. Check what Polymarket data is used for today beyond the `cross-venue divergence` signal, and whether
   Kalshi/Polymarket pairs of the same event can be matched.
4. Propose 2-3 concrete designs, smallest first, for market-aware candidate selection. Examples to weigh,
   not a menu to copy: a market-first scanner that ranks tradeable markets and feeds the best N to the
   LLM with a news query built from the contract; using price moves/volume/new listings as triggers;
   a cheap tradeability pre-filter ahead of the embedding match; cross-venue divergence as a candidate
   source. For each: what it changes, what it costs in LLM slots (the 70B is ~1 call/slot, 180s
   deadline), and how you would measure that it helps versus today's 2-fills-per-5-hours.
5. Report back before changing code. Keep it to what you verified; mark anything you inferred.

## Constraints

- Paper trading only; do not touch broker-worker credentials or any key files (`gekko.txt`,
  `kalshi_api_key.txt`, `*.txt` key files). Never print API keys from logs (kahuna's Finnhub error line
  writes the key in clear, avoid quoting it).
- The working tree has unrelated uncommitted changes; do not commit or revert files you did not edit.
- Known open issues you may run into, not yours to fix here: no confidence floor on entries, the
  "month [Oct] not in news [Sep]" verify penalty (~14% of `[VERIFY]` lines), SELL verdicts on unheld
  contracts (~80 per 5h), Twitter dead (402).
- `docs/forensics-2026-10-01.md` and `docs/forensics-2026-10-01-pm.md` hold the evidence; read them first.
