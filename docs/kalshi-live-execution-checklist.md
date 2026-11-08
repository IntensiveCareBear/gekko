# Kalshi live execution: what has to be true before real money

Written 2026-10-15 from a read of `broker-worker/{main,kalshi}.go`, `breen/{client,tracker}.go` and `run.sh`.
Every item cites code; none has been tested against Kalshi (not even the demo API). Today the stack is paper only:
breen `dry_mode` defaults to true, broker-worker logs `Kalshi=false`, and no order has ever left the machine.

**Verdict.** The order path can sign and post a limit order, and that is about all it does. The bugs in section 1
can place the wrong order (wrong price, wrong venue, wrong action), so they are blockers. Section 2 is what a live
book needs to stay correct. Section 3 is the rollout. Do them in order.

## 1. Blockers: the order itself can be wrong

| # | Defect | Where | Fix |
|---|---|---|---|
| 1 | **Venue routing by substring** (fixed 2026-10-15: `routeExchange` now routes `POLY-` tickers to Polymarket and the rest to Kalshi, explicit `Exchange` wins). With no `Exchange` on the intent, any contract ID containing `btc` or `poly` routes to **Polymarket**. `KXBTCD-…` is a Kalshi ticker. Breen never sets `Exchange` (`breen/client.go:66`). | `broker-worker/main.go:117-124` (breen's `inferExchange`, `breen/account.go:67`, still matches `btc`/`eth`: no routing effect, but Kalshi crypto fees are booked as $0, see `gekko_improvement_tasks.md` 10-19 review) | Set `Exchange` from the proposal's source (or ticker prefix `KX*` ⇒ kalshi) in breen; make the broker reject an empty exchange instead of guessing. |
| 2 | **Out-of-range price becomes 50¢.** `priceCents <= 0 \|\| > 99` is replaced by 50, so a bad `MaxPrice` (0, 1.0, NaN→0) posts a 50¢ limit. | `kalshi.go:55-58` | Reject with a failed `TradeResult`. |
| 3 | **Price truncates.** `int64(0.29*100)` is 28 (float error), so a BUY limit sits 1¢ under the intended price and a SELL 1¢ under. | `kalshi.go:55` | `int64(math.Round(p*100))`. |
| 4 | **Unknown action becomes a buy.** Anything but `buy`/`sell` is coerced to `buy`. | `kalshi.go:50-53` | Reject. |
| 5 | **No size or notional guard in the broker.** `Count` is passed through unchecked; breen's caps are the only brake. | `kalshi.go:60-68` | Broker-side hard cap (`BROKER_MAX_ORDER_NOTIONAL`) and `Count > 0`. |
| 6 | **Credential plumbing is absent.** `run.sh` starts broker-worker with only the socket, token and health address. The broker wants `KALSHI_API_KEY` and `KALSHI_PRIVATE_KEY_PEM` (PEM text), while the repo uses key *files* (`kalshi_api_key.txt`, `gekko.txt`). | `run.sh:335-338`, `broker-worker/main.go:44-45` | Read `KALSHI_PRIVATE_KEY_FILE` in the broker; pass only when an explicit `LIVE_TRADING=true` is set. |
| 7 | **Environment is implicit.** `KALSHI_API_BASE` defaults to the **demo** host. Good default, but nothing checks the key matches the host and nothing logs which one is live. | `kalshi.go:36` | Log base URL at startup; refuse prod base unless `LIVE_TRADING=true`. |

## 2. Correctness once orders are real

- **No fill confirmation.** The broker returns `Success: true` on HTTP 200/201 and sets `BrokerTxID` to *its own*
  `client_order_id`, not Kalshi's `order_id`. Nothing polls `GET /portfolio/orders/{id}` or checks `status`, so a resting,
  partially filled or cancelled order is booked as fully filled (`breen/tracker.go:1123-1150`: position opened at the
  *proposal* price and size).
- **No cancel, no expiry.** Limit orders have no `expiration_ts` and there is no cancel call. A GTC order that never
  filled stays on the book after breen has already moved on. Add `expiration_ts` (minutes) and a cancel path used by the
  force-exit sweep and shutdown.
- **Live branch skips accounting.** The paper branch debits cash, records spread and fee, runs the excursion tracking and
  the stop logic off a marked book. The live branch (`tracker.go:1304` onward) opens the position at the requested
  price with no cash debit. A live book needs the same ledger as paper, driven by actual fills.
- **No position reconciliation.** Nothing reads `GET /portfolio/positions` or `/balance` to compare against breen's
  map. A restart, a manual trade or a missed fill leaves the two silently different. Reconcile on startup and every N minutes;
  halt entries on a mismatch.
- **Failure masking is smaller here than for Polymarket but present:** the response body is never parsed, so a 200 with an
  error payload counts as success.
- **Error failsafe.** After N consecutive API failures, cancel open orders and halt entries.
- **Settlement.** Paper settles at expiry from marks; live positions settle on Kalshi. Pull settled results from the
  market endpoint (`result`, `expiration_value`, as `scripts/weather_review.py` already does) instead of inferring.

## 3. Rollout

1. Fix section 1; add broker tests for each (the broker has `broker_test.go` but none of these cases).
2. Point at the **demo** API with demo credentials and run one order end to end: place, poll, cancel, reconcile.
3. Run breen live against demo for a week at paper-equivalent size; compare booked vs reconciled positions daily.
4. Prod, with `BROKER_MAX_ORDER_NOTIONAL` at a few dollars and a small `STARTING_CAPITAL`, one persona only.
5. Raise limits only after the fill-rate and slippage seen live are inside what the paper cost model assumed.

## 4. Do not go live on this strategy yet

As of 2026-10-15 the book is down 2.1% with no demonstrated edge (see `forensics-2026-10-13.md`: weather about 0 net over
31 trades, model fair 0.27 against an 8.9% hit rate; gordon-default stopped out 13 of 18). The execution work above is
worth doing regardless, but it should not be the thing that puts capital behind the current signals.
