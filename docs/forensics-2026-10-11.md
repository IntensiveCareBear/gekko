# Forensics: overnight run 2026-10-10 21:49 → 2026-10-11 12:55

Scope: one launch at 21:49 PDT, read at 12:55 (uptime 15h06m, `uptime_seconds` 54,443 on every
service). Sources: `logs/*.log` since 21:49, `runs/{equity.csv,trades.jsonl}`, Redis
(`gekko:gordon_priority`, `gekko:run_config`, `gekko:kalshi_markets`), `/healthz`, `pmset`.
Follows [the 10-01 PM report](forensics-2026-10-01-pm.md). All **paper trading** (`dry_mode: true`,
broker-worker `kalshi_configured: false`, 0 orders).

**Verdict.** Infrastructure is clean; the strategy side is nearly idle. Net +$80 from two BTC
trades, but the weather trader did nothing because it has been paused since 10-03, and
gordon-default made 4 proposals in 15 hours.

## Headline numbers

|                     |                                                                       |
|---------------------|-----------------------------------------------------------------------|
| Uptime              | 15h06m, all 7 processes alive, no panics, 0 sleep gaps (184 equity rows, none >400s apart), caffeinate held |
| NAV                 | $100,152.42 (+0.15%), max drawdown 0.10%, 0 open positions            |
| Trades              | 2 closed in-run: TAKE_PROFIT +$152.60, STOP_LOSS −$72.00. Realized in-run +$80.60 on top of $71.82 carried |
| gordon-default      | 4 proposals → 2 filled, 2 rejected by breen. 374 LLM declines, 884 pre-gate skips, 0 fallbacks |
| gordon-weather      | 0 proposals, 0 `[WEATHER]` lines                                      |

## Finding 1 — the weather trader is paused, and has been since Oct 3

`gordon-weather` starts with `[PRIORITY] Persona "weather" PAUSED (weight 0)` (21:49:35) and
`main.go:666` skips weather observations when paused. Redis `gekko:gordon_priority` holds
`{"weights":{"gordon-default":1,"weather":0}}` with `updated_at_ms` = 2026-10-03 01:47. The weight
persists across relaunches, so every start since (10-09 14:00, 10-10 17:11, 10-10 21:49) came up paused.
The last `[WEATHER]` line in the log is 10-03 01:10.

Meanwhile kahuna is still ingesting forecasts and METAR, and the catalog carries all seven
KXHIGH series (12 buckets each, for Oct 11 and 12). The data is flowing to a trader that is switched off.
**Open question: was the weight-0 deliberate?** It lines up with the Oct 2–3 stop-outs (−$300 to −$340
per bucket). If deliberate, nothing to fix, but nothing in `run.sh` or the startup log says so beyond
that one PRIORITY line. If not, reset it from the UI.

Related: no `Loaded bias table` line in this run's weather log (last one 10-03 01:10). Expected while
paused, but check it comes back when weather is re-enabled.

## Finding 2 — gordon-default trades about once every 4 hours

The LLM is running steadily (about 15–20 declines/hour all night, 101 in the 22:00 cold-start hour).
Nearly everything dies before it reaches a fill:

- 374 declines, every one `action not BUY/SELL` (HOLD, or SELL on an unheld contract).
- 884 pre-gate skips: 520 bearish/no-trade cooldown, 191 spread/one-sided book, 38 ask outside the entry band, 29 near-expiry.
- 92 `[EDGE]` skips, typically edge +0.04–0.07 against a required 0.08–0.11.
- 2 proposals rejected by breen: Brent 22:18 (`RejectOnBreach`, a 600-lot BUY at 0.43 whose edge check then failed at 22:18:20), and BTC T83999 10:26 (size 4,790 > max position 3,750).

Flat from 01:39 to 10:26 (equity constant at $100,224.42). No error explains it; the funnel just never
passed anything. This is a consequence of the recent loosening commits not yet showing up as volume,
or the market being genuinely quiet on a Saturday night. The logs can't tell which.

## Finding 3 — the one loss was a falling-knife entry

KXBTCD-26OCT1117-T83999.99 was first proposed at 10:26 at 0.37 (conf 0.78, momentum +9.2%) and
rejected for size. At 11:06 it was re-proposed and **filled at 0.20** (conf 0.50, **momentum −58.9%**),
stopped at 0.16 six minutes later (−$72, MAE −20%). The contract had lost nearly half its price in 40
minutes and the system bought it anyway because the signal cleared the mid-band gate. The earlier
profitable trade (T83499 at 0.165, +47%, 1h51m) shows the strategy has some edge on BTC ladders; this
one is the momentum sign being ignored. A BUY with momentum below, say, −25% and no book-imbalance
confirmation is worth a gate or a confidence haircut.

## Finding 4 — wasted verify work on stale contracts

1,050 `[VERIFY]` lines, 643 of them in the first 70 minutes. The top offenders are expired or non-matching
contracts: KXCPICOMBO-26SEP (203, a September contract in October), POLY-… "year not in news" (140),
KXNASDAQ… (72). They cost embedding/verify cycles, not LLM generate calls, so low impact. The catalog
prune at 12:52 removed 380 markets, so this is mostly cold-start noise.

## Smaller things

- **Twitter feed still dead**: 38 `402 credits depleted` errors.
- **Finnhub key leak still open**: `finnhub.go:208` logs the raw error, which embeds `token=…`.
  24 lines in `logs/kahuna.log` carry the key in clear. Redact it at the log call and rotate the key.
- 2 `no live order book (mock fallback disabled)` errors at startup (FRIENDSHIP-CONNECTION-2026, POST-GAZETTE-AWARD-MILLIONS), harmless.
- `breen.log` and `liveview.log` end at 11:12 / 11:06 only because nothing traded after that; both are healthy.
- Uncommitted work in the tree: `gordon/priority.go` (keep the last weight on a transient Redis error),
  `weather.go` (PredictiveMaxEdge, MinSigmaF), `bearcooldown_test.go`, `scripts/backfill_rag.sh`. The
  running `gordon_bin` was built 21:49 and may or may not include them.

## What to do

1. Decide on weather: reset `weights.weather` to 1 from the UI, or leave it and say so somewhere.
2. Rotate the Finnhub key and redact the error line.
3. Consider a momentum guard on entries (Finding 3).
4. Re-read after a normal weekday session before judging the entry-gate loosening: this window is mostly weekend.
