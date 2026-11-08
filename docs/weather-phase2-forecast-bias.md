# Weather Phase 2 — forecast-error bias model (the local model)

**Status:** SHIPPED (dark, self-gating), 2026-07-11. Phase 1 (cleared-bucket trading) is
live (`a952bad`…`4d84fbf`). Phase 2 extends the weather strategy from "trade buckets the
day's high has ALREADY cleared" to "price buckets that are still live, by predicting
the final daily high with a de-biased forecast + calibrated uncertainty." It lands dark:
the model self-gates per station and only prices predictively once ≥10 forward
(forecast, realized) pairs have accumulated for that station, so it needs a multi-week
data runway before it trades. Phase 1 cleared-bucket trading is unaffected and runs
alongside it throughout.

## As-built (what actually shipped)

The four build-order items below are all implemented. Concrete decisions taken:

- **Forecast ingest** — `kahuna/ingest/weather_forecast.go`: a new `WeatherForecastSource`
  polls NWS `api.weather.gov` per station (two-step `/points`→gridpoint `/forecast`,
  grid URL cached; keyless, `User-Agent` set, `api.weather.gov` allowlisted in
  compliance). It records the daily-high forecast per (station, date) to a file-backed
  `ForecastStore` (`runs/weather/forecasts.json`, survives restart) and emits a
  `WEATHER_FORECAST` event carrying today's forecast high.
- **Forward corpus** — the existing METAR source (`weather.go`) gained a day-rollover
  hook: when a station's local date advances, the prior day's realized high is final, so
  it joins that with the recorded forecast and appends one row to
  `runs/weather/history.jsonl` (idempotent per (station, date), restart-safe). Join uses
  the latest forecast of the day; the hour-of-day residual is deferred to the richer
  regressor.
- **Bias model** — `kahuna/ingest/weather_corpus.go`: a *pure* per-station estimator
  (mean residual = bias, sample-stddev = sigma with a neutral-prior floor of 6°F,
  residual quantiles, sample count, `seasoned = N≥10`) recomputed on every new resolved
  pair and written atomically to `runs/weather/bias.json`. Kept in-process in kahuna
  (no new binary/cron) but pure and unit-tested, matching Lou's JSON-table convention.
- **Predictive pricing** — gordon reads `bias.json` (mtime-cached `BiasTable`, mirroring
  the Lou lesson book) and caches `WEATHER_FORECAST` events (`forecastCache`). In
  `weather.go`, for every still-live bucket it computes `P(final high ∈ bucket)` from
  `Normal(forecast+bias, sigma)` **truncated below at the running high**, with a ±0.5°F
  rounding correction for Kalshi's integer-degree settlement, and emits a BUY where
  `P − ask ≥ WEATHER_PREDICTIVE_MIN_EDGE` (default 0.05). Predictive proposals carry
  `Confidence = P` and no `SettleResolution` (not settled fact); cleared buckets already
  emitted by Phase 1 are de-duplicated out. Gated on `seasoned` + a cached forecast, so
  absent either it emits nothing.

**Knobs:** `WEATHER_FORECAST_ENABLED` (default on when `WEATHER_SIMULATED=false`),
`WEATHER_FORECAST_POLL` (1h), `WEATHER_PREDICTIVE` (on), `WEATHER_PREDICTIVE_MIN_EDGE`
(0.05), `WEATHER_PREDICTIVE_HAIRCUT` (0), `WEATHER_PREDICTIVE_MIN_ASK` (0.03), and the
shared file paths `WEATHER_FORECAST_STORE` / `WEATHER_HISTORY_FILE` / `WEATHER_BIAS_FILE`
(run.sh sets these to absolute `runs/weather/*` paths so kahuna and gordon agree).

**Fee model: done** (breen `KALSHI_FEE_MODEL` on, `KALSHI_FEE_CHARGE` on since 2026-09; fees are debited
from cash). **2026-10-19: the predictive path loses.** 169 settled picks: mean fair 0.26, hit rate 0.089, negative in every band. The cause is the sigma floor: gordon applies `max(table sigma, WEATHER_MIN_SIGMA_F=3.0, station floor)`, which caps a 2°F bucket near 0.26 and buys cheap outer buckets. DEN/MDW/MIA day-ahead error is about 1°F. Fit sigma per station from day-ahead error, re-score, and keep `WEATHER_PREDICTIVE=false` until the table is positive (`gekko_improvement_tasks.md` 10-19 review). Refit tested the same day (`scripts/weather_sigma_backtest.py`): across all 414 buckets of 69 settled station-days, market mid Brier 0.080 against at least 0.118 for every sigma, so sigma isn't the limit. The forecast is: NWS + bias at pick time knows less than the book. Off until the forecast input improves. **Still open:** the estimator is intentionally v1 (empirical bias + Gaussian); the
corpus + table interface supports swapping in the richer regressor later without
touching the strategy.

---

_Original scoping below._

## What Phase 2 buys us

Phase 1 only ever trades the single top "X° or above" bucket, and only once the
observed high has reached it — a rare, settled edge. Phase 2 lets us price **every**
bucket (ranges and "or below" included) *before* resolution: given a predicted final
high and its uncertainty, compute `P(final high ∈ bucket)` for each bucket and trade
wherever that fair probability diverges from the market. This is the larger, diurnal
edge the Phase 1 doc deferred.

## Grounding: what the data actually allows (verified against live APIs)

- **Forecast is available, keyless.** `api.weather.gov` (`/points/{lat},{lon}` →
  `/forecast`) returns a daytime high per station (needs a `User-Agent` header).
  NWS forecasts are a strong baseline; our job is to learn their *residual bias* and
  *error spread*, not to out-forecast NWP.
- **Realized high we already compute.** The Phase 1 METAR source tracks each
  station's running local-day high; the end-of-day value is the realized-high label
  (a proxy for Kalshi's NWS Climatological Report max — see caveats).
- **Training pairs can't be backfilled.** METAR history is capped (~2 weeks) and NWS
  serves only the *current* forecast — there's no keyless archive of past forecasts.
  So `(forecast, realized)` pairs must be collected **forward**. The model therefore
  starts neutral and earns confidence over days/weeks; it can't be trained offline
  before launch.

This forward-only constraint is why Phase 2 dovetails with Lou's learning-loop
infrastructure (`docs/lou-learning-loop-proposal.md`) — a forward-accumulated store
that corrects behavior over time — rather than being a train-once ML artifact.

## Proposed architecture (Lou-aligned, incremental)

1. **Forecast ingest** — pull the NWS daily-high forecast per station (new keyless
   source / extension of the weather source; `api.weather.gov` allowlisted, `lat,lon`
   per station, forecasts refresh a few times/day). Emits/records `forecast_high`.
2. **Forward corpus** — each resolved day, append one record per station:
   `{station, date, forecast_high, realized_high, hour_features…}` to a JSONL history
   (mirrors `runs/lou/history.jsonl`). This is the training data, accumulated live.
3. **Bias model** — a per-station estimator over the corpus: a bias term
   (mean `realized − forecast`) plus an empirical error distribution (quantiles /
   σ). Written to a small JSON table (`runs/weather/bias.json`, mtime-cached), the
   way Lou writes `lessons.json`. Neutral prior (bias 0, wide spread) until it has
   ≥ N pairs per station, so it lands dark and self-gates.
   - *Start simple, upgrade later:* the v1 estimator is empirical bias + quantiles.
     The same corpus + table interface supports swapping in a richer regressor
     (features: running-high-so-far, hour-of-day, day-of-year, NWS forecast → final
     high) without touching the strategy.
4. **Predictive pricing** — the weather strategy reads the bias table and, for a live
   bucket, computes `P(final high ∈ [lo,hi])` from the bias-corrected forecast
   distribution, **truncated below by the running high so far** (the final high can't
   be less than what's already been observed — this tightens the distribution sharply
   as the afternoon peak passes). Trade where fair − ask ≥ edge, same gate as Phase 1.
5. **Gating** — predictive trades stay dark until (a) the bias table has enough data
   per station and (b) the real-expiry fix is live (below). Phase 1 cleared-bucket
   trading is unaffected and keeps running throughout.

## Dependencies

- **Real-expiry fix — LANDED (`a50b47c`).** Predictive pricing is a function of
  *time to resolution* (how much diurnal warming is left today), so it needs correct
  per-market expiry. `breen/expiry.go tickerExpiry` already parses the weather ticker
  format (`KXHIGHNY-26JUL11` → 2026-07-11 23:59 ET, Kalshi's high-temp close), so this
  dependency is satisfied — no separate gate.
- **Fee model (landed; was absent in sim).** Predictive edges are thinner than cleared-bucket
  edges (buying a 0.60 that's really 0.70, not a 0.90 that's really 1.0), so taker
  fees matter more. Worth landing the fee model before trusting Phase 2 paper ROC.

## Caveats / modeling notes

- **Label fidelity.** Kalshi settles on the NWS *Climatological Report* daily max;
  we train on the METAR daily max, a close but not identical proxy (rounding,
  climate-day window). Good enough for a bias/uncertainty model; a later refinement
  could parse the CLI product or use resolved-market outcomes as labels.
- **Cold start.** With no pairs, the model must not trade — the neutral-prior +
  min-sample gate handles this. Expect a multi-week runway before predictive trading
  turns on for a given station.
- **Distribution shape.** Empirical quantiles are robust but coarse with few samples;
  a Gaussian(bias, σ) fallback smooths the thin-data regime.

## Effort (rough)

- Forecast ingest source: ~½ day.
- Forward corpus logging (forecast + realized join per resolved day): ~½ day.
- Bias model (estimator + JSON table + mtime cache + neutral prior/gate): ~1 day.
- Predictive pricing in the strategy (bucket probability, running-high truncation,
  edge gate): ~1 day.
- Plus the unavoidable **data runway** (weeks) before live predictive trading, and
  the real-expiry dependency.

## Decisions (resolved 2026-07-10)

1. **Forecast input: NWS forecast + learned bias/uncertainty.** Anchor on the keyless
   NWS daily-high forecast; the local model learns its per-station residual bias +
   error spread.
2. **Estimator: empirical per-station bias + error quantiles**, neutral prior until
   enough samples, behind a table interface so a richer regressor can swap in later.
3. **Sequencing: build now.** The real-expiry fix is already landed, so we build the
   forecast ingest + forward logging + bias model now; live predictive trading gates
   only on per-station data sufficiency (min pairs), not on any pending fix.

## Build order

1. NWS forecast ingest (keyless `api.weather.gov`, allowlist, per-station lat/lon).
2. Forward corpus: join each day's forecast with its realized METAR high → JSONL.
3. Bias model: per-station bias + error quantiles → `bias.json`, neutral prior + gate.
4. Predictive pricing in the weather strategy (bucket probability, running-high
   truncation, edge gate), dark until the per-station model is seasoned.
