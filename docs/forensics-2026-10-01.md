# Forensics: the overnight run was three hours long and traded nothing (2026-10-01)

Scope: the run started 2026-09-30 21:40 PDT on the new machine (Mac17,14, 64 GB), read at
09:55 the next morning. `logs/*.log`, the service `/healthz` endpoints, Redis state, `pmset`
sleep history and raw KAUS METARs. Follows [the 09-03 forensics](forensics-2026-09-03.md).

**Verdict.** Nothing was learned about the strategy overnight, because the strategy did not
run. The Mac slept for nine of the twelve hours; the stack was launched without the 09-03
knobs, so it is a different (older) system than the one last reviewed; the weather book is
dark because its bias table did not survive the migration; and the one LLM persona spent its
three waking hours re-deciding the same Brent contract. One real money bug turned up along
the way, in the weather day-high.

## Headline numbers

|                                   |            |
|-----------------------------------|------------|
| Wall clock                        | 12h 15m (09-30 21:40 → 10-01 09:55) |
| **Process uptime**                | **3h 02m** (`uptime_seconds` 10,912 on every service) |
| Orders executed this run          | **0** |
| NAV                               | $99,949.10, flat all night |
| Open positions                    | 1 — KXPAYROLLS-26SEP-T80000, 598 @ 0.61, bought 21:21 by the *previous* launch |
| LLM calls → proposals → fills     | 304 → 7 → 0 |
| Weather events → proposals        | 1,894 → 0 |

The −$58.99 realized and the 12 closed trades in `run_summary.txt` are all carried over from
09-05…09-08 via the Redis restore; none of it is from this run. `runs/trades.jsonl` holds one
row because `runs/` itself was not migrated.

## Finding 1 — the Mac slept through 75% of the run

`pmset -g` reads `sleep 1`: idle system sleep after one minute. Nothing in `run.sh` held a
power assertion, so the machine went down shortly after 22:10 and only surfaced for
maintenance dark-wakes. The equity curve has 14 gaps of 16–117 minutes; the marketdata
`connection reset by peer` warnings (76 `poll_errors`) are all the first request after a
wake. `run_summary.txt` says `Elapsed: 3h0m0s` — it is right, and it was the clue.

**Fixed.** `run.sh` now starts `caffeinate -is -w $$`, which holds the assertion exactly as
long as the script lives. Takes effect on the next launch.

## Finding 2 — the run was launched without the 09-03 configuration

It was started with bare `./run.sh`, not `scripts/relaunch.sh`. Every knob that file exports
is therefore at its `run.sh` default. From breen's own startup banner:

```
Settlement hold: disabled (SETTLEMENT_HOLD_PERSONAS empty — every position is managed on the mark)
Cost accounting: fee model=true (rate 0.070, charged to cash=false)
Friction/churn gates: min clip $0 notional | max 0 entries/day | max 0 round trip(s) per family per day
```

| knob | 09-03 live | this run |
|---|---|---|
| `SETTLEMENT_HOLD_PERSONAS` / `_TP_PRICE` | weather / 0.95 | off / off |
| `KALSHI_FEE_CHARGE` | true | **false** — fees modelled, not charged |
| `SHADOW_PERSONAS` | the three gemmas | empty — **the LLM persona trades live** |
| `WEATHER_BASE_SIZE` | 1000 (commit 8e5371a) | 100 |
| `MIN_EDGE` / `MIN_ENTRY_PRICE` / `MAX_ENTRY_PRICE` | 0.10 / 0.20 / 0.55 | defaults |
| `MAX_ENTRIES_PER_DAY` / `MAX_FAMILY_ENTRIES_PER_DAY` | 40 / 1 | unlimited |

The third row is the one that matters. The 08-25 review shadowed the LLM fleet until it
cleared a 200-proposal bar; the shadow scorecard in Redis is frozen at 09-08 reading
**−17.96% over 60 scored**. That fleet has not been re-activated on evidence — it has been
replaced by one un-shadowed `gordon-default` on `llama3.3:70b` (the uncommitted `run.sh`
default) with live order authority, and it has already used it (Finding 5).

**Not changed** — `relaunch.sh` also pins `GORDON_PERSONAS` to the gemma trio and 45s LLM
timeouts, both of which contradict the 70B experiment, so "just use relaunch.sh" is not the
answer either. This needs a decision: either fold the policy knobs (fee charge, settlement
hold + TP, weather size, entry band, churn caps) into `run.sh` defaults, or update
`relaunch.sh` for the 70B fleet and add the new persona to `SHADOW_PERSONAS`.

## Finding 3 — the weather book is dark, and will stay dark for days

Two legs, both silent:

- **Predictive** (the leg that wrote nearly every weather trade to date) needs a seasoned
  per-station bias. `runs/weather/bias.json` is `[]` — `Loaded bias table: 0 stations
  (0 seasoned)`. The corpus lived in `runs/weather/` on the old volume and was not in
  `migration_backup/`. `history.jsonl` has four rows, all with `forecast_high: null`, so
  even those do not train. `/Volumes/vault` is not mounted here.
- **Cleared-bucket** only buys "X° or above" once the observed high passes X+1. None did in
  three mostly-night hours.

If the old volume is reachable, copying `runs/weather/{history.jsonl,bias.json}` across
restores the book immediately. Otherwise it re-seasons from zero.

## Finding 4 — the weather day-high counted yesterday evening (fixed)

Austin, overnight:

```
21:40 PDT (23:40 CDT)   82.9°F now, local-day high 98.1°F      ← 09-30, correct
00:59 PDT (02:59 CDT)   81.0°F now, local-day high 91.0°F      ← 10-01, impossible
```

No reading after local midnight exceeded 82°F. The 91.0 is the **6-hour max group** on the
00:53 CDT METAR, which reports the maximum over the six hours *before* the observation —
18:53 to 00:53, i.e. yesterday evening's 91°F. `reduceObs` filtered observations by local
date but folded in `MaxT` by the ob's own timestamp. Miami showed the same thing (82.0 at
04:05 EDT). Any station whose first synoptic ob lands just after local midnight is affected,
every night, and the error is largest exactly when a front makes today cooler than last
night.

The consequence is not cosmetic. `DayHighF` is what the cleared-bucket leg treats as a
*settled fact* — it buys "X° or above" at up to 97¢ on it — and it is the lower truncation
for the predictive model. It did no damage today only because Austin's top bucket was 95°+.
It is at least consistent with `KXHIGHCHI-26AUG30-T86` in the 09-03 report (an "or above"
bucket entered at 0.89 that settled at zero, −$86), though that is not verifiable from here.

**Fixed** in `kahuna/ingest/weather.go`: the max group is folded in only when its six-hour
window began on today's local date. Regression test
`TestReduceObs_MaxGroupStraddlingMidnight` uses the real KAUS values. Needs a kahuna rebuild
to take effect.

## Finding 5 — what the LLM persona did with three hours

```
304 LLM calls
 ├─ 208  [EDGE]      edge below min + spread
 ├─  69  [HOLDINGS]  SELL verdict on something not held → breen "opinion-only" reject
 ├─  31  [SKEW] / 4 [FLOOR] / 7 [DECLINE]
 └─   7  proposals → 7 rejected (6 × one-strike-per-family, 1 × limit breach)
```

**5a. The bearish cooldown is not running.** 27 of the 69 unactionable SELL verdicts are one
weekly Brent series, `KXBRENTW-26OCT0217-T81.99` alone re-asked every ~20 seconds for the
last ten minutes of the log, at ~36s of 70B inference each. The fix for exactly this is
sitting uncommitted in `gordon/main.go` (`bearCooldown`, `consensusEnabled`) — but
`gordon_bin` was built at 21:40 and `main.go` was saved at 21:57. The running binary
predates it. Its test passes; it just needs a rebuild. Nothing to fix, only to deploy.

**5b. Confidence gates nothing outside the mid band.** The open position:

```
21:18  [VERIFY] KXPAYROLLS-26SEP-T80000 entity-mismatch penalty 0.40 (conf→0.29)
21:19  [EDGE]   BUY skipped — fair 0.63 vs book 0.60/0.62: edge +0.010 < required 0.070
21:21  BUY 598 KXPAYROLLS-26SEP-T80000 @ 0.61 | conf 0.12        ← executed, $365
```

Skipped for a 1¢ edge, then bought two minutes later at confidence 0.12. The only
confidence floor is `MID_BAND_MIN_CONFIDENCE`, which covers [0.42, 0.58]; 0.61 is outside
it. Size does not track confidence either — the T90000 proposals went 874 lots at conf 0.56,
1,119 at 0.75 and **1,111 at 0.26**. The same market was re-evaluated 38 times, the model's
fair value wandering 0.63–0.68 against an unchanged 0.60/0.62 book until one sample cleared.
That is sampling noise until something passes, not edge. The position is flat-marked
(+$8.97, which is the entry half-spread) into Friday's jobs report; force-exit is scheduled
for an hour before the recorded expiry of 10-02 03:40 PDT.

**5c. The month check penalises every contract that expires next month.** 272 of 474
`[VERIFY]` lines are `month [Oct] not in news [Sep]`, a flat −0.40 on news written 09-30
about a contract dated 10-02. It will fire on every near-dated contract for the last few
days of every month. Given 5b it currently changes nothing downstream, which is its own
problem. Not fixed — it wants a rule ("adjacent month within N days of expiry is not a
mismatch"), not a patch.

## Smaller things

- **Startup embed starvation.** 493 `[DROP]`s, all before the 21:40 relaunch: `nomic-embed`
  calls timing out while the 45 GB `llama3.3:70b` loaded. Zero since. Worth knowing the
  first ten minutes after a cold start are blind.
- **`priority_tickers: 0`** with a position held — the shared-atomic bug from 09-03, still
  open.
- **`best_mark`/`worst_mark` 0.61/0.61** on the open position twelve hours in — the
  `syncToRedis`-only-on-fills bug from 09-03, still open.
- **`docker-compose.yml`** qdrant volume now points at `./qdrant_storage` (uncommitted).
  Correct for this machine; the old path does not exist here.

## What changed

```
kahuna/ingest/weather.go        6h max group only counts when its window began today
kahuna/ingest/weather_test.go   TestReduceObs_MaxGroupStraddlingMidnight
run.sh                          caffeinate -is -w $$
```

Nothing was restarted. All three, plus the already-written bearish cooldown, land on the
next launch (`run.sh` rebuilds).

## Follow-up, same day

Both open decisions were answered: the 70B setup is the real one, and the old `runs/` was
copied into `migration_backup/runs`.

- **`runs/` restored.** Archives, `lou/` and the legacy CSV copied across; `trades.*`,
  `equity.*` and `weather/history.jsonl` are the old rows followed by this machine's (12
  trades, 348 corpus rows). `weather/bias.json` is the old table — all seven stations
  seasoned, n = 47–51. `forecasts.json` was left alone; the old one is from 09-08.
- **`runs/lou/overrides.env`** restored without its three `OLLAMA_*` lines (45s deadlines,
  2 slots), which were tuned for the gemmas and would have overridden the 70B defaults.
- **`scripts/relaunch.sh`** is now the 70B launch: the gemma `GORDON_PERSONAS`, the 45s
  timeouts and `SHADOW_PERSONAS` are gone; every policy knob from 09-03 stays. That makes
  `gordon-default` **live**, under `MAX_ENTRY_PRICE=0.55` (which alone would have refused
  the 0.61 payrolls buy), the fee charge and the churn caps.

## Next

1. **Decide the configuration** (Finding 2) before the next unattended run — in particular
   whether `gordon-default` on the 70B model trades live or shadows. Today it trades live
   with no fee charge, no churn cap and no confidence floor.
2. **Recover `runs/weather/`** from the old volume if it still exists.
3. **Relaunch**, then confirm in the logs: `[PREGATE] … bearish/no-trade cooldown`, no
   `local-day high` above the night's readings after local midnight, and an equity curve
   with no gaps.
4. A confidence floor on entries (5b) and the adjacent-month rule (5c) — both policy, both
   small.
5. Still open from 09-03: the mute-fleet alert. Last night's failure was a different shape
   (asleep, not mute), but `orders_executed == 0 && uptime ≪ wall clock` is the same
   one-line class of check.
