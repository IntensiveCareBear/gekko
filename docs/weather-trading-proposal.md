# Weather trading stack — scoping proposal

**Status:** built and live (Phase 1 cleared-bucket trades plus the Phase 2 predictive model; see
`weather-phase2-forecast-bias.md`). As of 2026-10-15 it has no demonstrated edge over the market
(`scripts/weather_review.py`; ~0 net P&L over 31 trades). Original scoping follows. Backlog item #1 from the 2026-07-10 Polymarket-bots
review (feasibility High / effort Medium / impact High). Reference repos:
`yangyuan-zhen/PolyWeather` (190★, Polymarket) and
`AruneshDev/Automated-Trading-System-Kalshi-Weather-Model` (96★, already
Kalshi-native).

## Why weather

Kalshi's daily high-temperature markets are the canonical *solvable* prediction
market: the underlying (today's observed high at a specific station) is a
public, free, keyless numeric feed that updates every hour, and the outcome is
mechanically determined by that feed — not by opinion, sentiment, or a headline.
This is **orthogonal to everything gekko trades today** (news/LLM-driven macro,
crypto, sports). The edge is *data freshness*, not judgement: once the observed
high for the day clears a bucket's lower bound, that YES is ~locked at 1.0 while
the book may still price it at 0.85; late in the day the remaining diurnal
warming is small and tightly bounded, so the fair distribution over buckets
collapses faster than a casual market-maker reprices.

Crucially, an LLM is the *wrong tool* for this. The fair value is a deterministic
function of (observed-high-so-far, hour-of-day, station climatology). So the core
design decision below is: **weather should bypass gordon's LLM and price markets
directly.**

## How gekko is wired today (the three integration points)

1. **Data in** — `kahuna/ingest/*.go`. Each source implements
   `IngestionSource.Start(ctx, chan<- *GekkoIngestEvent)`, is constructed in
   `kahuna/main.go`, configured via `kahuna/config/config.go` env vars, and its
   emitted `SourceURL` host must be SHA-256-allowlisted in
   `kahuna/compliance/filter.go`. FRED (`kahuna/ingest/fred.go`) is the closest
   template — it's the only *numeric ground-truth* source and even does
   double-duty (emits events **and** serves a derived JSON artifact via the
   monitor). Weather is the same shape.

2. **Market discovery** — `marketdata/discovery.go` enumerates open Kalshi
   markets by series/category and publishes the catalog to
   `gekko:kalshi_markets` (Redis). **Gap:** `run.sh` currently sets
   `KALSHI_CATEGORIES="Crypto,Economics,Financials,Commodities"` — the weather
   markets live in Kalshi's **Climate** category (high-temp series such as
   `KXHIGHNY`, `KXHIGHLAX`, `KXHIGHCHI`, `KXHIGHMIA`, `KXHIGHDEN`, `KXHIGHAUS`,
   `KXHIGHPHIL` — exact tickers/structure to be confirmed against the live
   `/series` + `/markets` API). Until Climate is discovered, the high-temp
   markets don't even enter the catalog. This is a one-line config change plus a
   priority-series list.

3. **Proposal out** — gordon builds a `StrategyProposal`, marshals it, and
   produces it to the outbound Kafka topic that breen consumes
   (`gordon/main.go` ~862). breen's consensus/allocator/risk-gate/sim then own
   execution. **Any component that produces a well-formed `StrategyProposal` to
   that topic participates — no LLM required.** This is the seam the weather
   strategy plugs into.

## Proposed architecture

Two components, mirroring the reference split of "data" + "model":

### Component A — METAR ingest source (`kahuna/ingest/weather.go`)

A keyless poller against **aviationweather.gov**'s Data API (free, no key, no
budget — only a calls/min pacing guard, exactly like the FRED/Reddit pattern).

- **Config** (`WeatherConfig`, wired through `config.go` + `kahuna/main.go` +
  `run.sh`, dark by default like Reddit):
  - `WEATHER_SIMULATED` (default true → idle)
  - `WEATHER_STATIONS` — ICAO ids mapped to Kalshi cities
    (`KNYC,KLAX,KMDW,KMIA,KDEN,KAUS,KPHL,...`)
  - `WEATHER_POLL_INTERVAL` (METAR/SPECI refresh hourly; poll ~10 min to catch
    off-cycle SPECI corrections)
  - `WEATHER_MAX_CALLS_PER_MIN` (pacing guard)
- **Emits** a `GekkoIngestEvent` per new observation with a readable,
  embeddable sentence (FRED-style): `"Central Park, NY (KNYC) observed 82°F at
  14:51 ET; day's high so far 84°F. Source: NOAA/aviationweather.gov."` plus a
  structured JSON payload (`WeatherObs{Station, City, TempF, DayHighF, ObsTime,
  ...}`) for the strategy to consume.
- **SourceID** `WEATHER` (new const in `ingest.go`), **SourceURL** on
  `aviationweather.gov` → **new allowlist hash in `compliance/filter.go`.**
- Serves a `Metrics()` snapshot for the System-tab External-APIs card (same
  pattern as `RedditMetrics`/`FREDMetrics`; register via
  `mon.SetWeatherSource`).

Even shipped *alone*, Component A gives the existing LLM personas real-time
temperature facts to reason over — a safe, incremental first drop.

### Component B — deterministic weather strategy (the actual edge)

A specialist that turns observations into `StrategyProposal`s **without the LLM**.
It joins the METAR feed (day's observed high per station) against the discovered
Climate markets (bucket thresholds parsed from `gekko:kalshi_markets` titles),
computes a fair YES probability per bucket, and proposes BUY where
`fair − ask ≥ MIN_EDGE` (reusing the exact edge gate the LLM path already uses in
`gordon/signals.go`).

Fair-value model, Phase 1 (deterministic, no training):
- If observed day-high already ≥ bucket lower bound → YES fair ≈ 1.0
  (minus a small settlement-risk haircut).
- Otherwise, remaining-warming headroom from a static per-station diurnal +
  monthly-climatology table, shrinking toward 0 as the day's max-heating hour
  (~15:00–16:00 local) passes. Late-day, the distribution is nearly a step
  function at the current high — that's the exploitable window.

**Where B lives — open decision (see below).** Cleanest: a new dark-by-default
`weather` persona/strategy path inside gordon that produces `StrategyProposal`s
directly (skipping embed→Qdrant→LLM), so it inherits breen's consensus, sizing,
risk gate, sim, scorecard, and the liveview Strategy Stream for free. Heavier:
a standalone `weather-worker` module producing to the same topic.

### Phase 2 (separate follow-on) — forecast-error bias model

Learn per-station/per-source systematic over/under-forecast bias from logged
(forecast, realized-high) pairs and correct future fair values. This is an
ML/stats effort with its own data-collection runway — **explicitly out of scope
for the first sitting**, and dovetails with Lou's learning-loop infrastructure
(`docs/lou-learning-loop-proposal.md`) rather than being built fresh.

## Dependencies & interactions

- **Real expiry handling.** breen currently assigns *random* 2–48h expiry and
  ignores real Kalshi resolution dates (top bug per the 2026-07-10 forensics,
  `[[gekko-forensics-2026-07-10]]`). Daily high-temp markets resolve at a fixed
  local end-of-day; the strategy's whole late-day edge depends on *time to
  resolution* being correct. **Weather is far more valuable after real expiry
  lands** — worth sequencing behind (or bundling with) that fix. Phase 1 can
  still ship and trade the "already-cleared-the-bucket" case, which is
  time-insensitive.
- **Fee model.** The sim has no Kalshi fee model (2026-07-05 forensics). Weather
  edges are often small (buying a 0.90 YES that's really 0.99); taker fees can
  eat a thin edge. Not a blocker for Phase 1 but the ROC will read optimistic
  until the maker-exec/fee item (#2) lands.
- **Settlement in sim.** Paper settlement is `rand.Float64() < entryPrice`
  (`breen/account.go`). For weather that's *especially* wrong — a 0.99 YES that
  actually resolves YES ~100% of the time would settle as a coin-ish flip. To
  measure the weather edge honestly in paper, the sim should settle
  weather-tagged positions deterministically against the observed high (or at
  least against the real Kalshi resolution once expiry handling exists). Flag as
  a required sim change for a meaningful A/B.

## Effort

- **Component A (METAR source):** ~half day. Straight FRED-clone: source +
  config + main wiring + allowlist hash + monitor card + tests. Low risk.
- **Component B (deterministic strategy, Phase 1):** ~1 day. The fair-value
  model is simple; the work is the market-title bucket parser, the
  station→series mapping table, and the integration seam (new gordon strategy
  path vs. worker — see decision).
- **marketdata Climate discovery:** ~1 line + a priority-series list; verify
  against the live API.
- **Sim settlement fix for honest measurement:** ~half day, and arguably a
  prerequisite for trusting the paper A/B.

Total for a first, tradeable, dark-by-default drop: **~2 sessions**, matching the
backlog estimate. Phase 2 bias model is a separate effort.

## Decisions (resolved 2026-07-10)

1. **Component B lives inside gordon** — a new dark `weather` strategy path that
   emits `StrategyProposal`s directly, skipping embed→Qdrant→LLM, reusing
   consensus/sizing/risk/sim/scorecard/Strategy-Stream.
2. **Ship Phase 1 cleared-bucket only.** Trade only the time-insensitive case
   where the day's observed high has *already* cleared a bucket's lower bound
   (fair YES ≈ 1.0, outcome is settled fact — no dependence on time-to-
   resolution). The late-day diurnal edge (Situation 2, which *does* depend on
   correct expiry) is deferred until the real-expiry fix lands — see below. This
   de-risks the whole source→discovery→strategy→settlement pipeline on real
   markets without coupling weather to a larger bug fix.
3. **Take the sim-settlement change.** Settle weather-tagged positions
   deterministically against the observed high (not `rand < entryPrice`), so the
   paper A/B actually measures the edge.
4. **Full liquid city set** from day one — NYC, Chicago, LA, Miami, Austin,
   Denver, Philadelphia.

### Hard prerequisite for the diurnal edge (not for Phase 1)

The **real-expiry fix is now the top of the bug list, non-negotiable, next up**
(`[[gekko-forensics-2026-07-10]]`): breen must honor real Kalshi resolution
dates instead of assigning random 2–48h expiry. Phase 1 cleared-bucket trading
ships *without* it, but Situation 2 (pricing a still-live bucket from remaining
diurnal warming) stays dark until real expiry is in — because that edge is
entirely a function of hours-to-end-of-day.

## File-touch summary (Phase 1)

- `kahuna/ingest/weather.go` (+ `_test.go`) — new METAR source.
- `kahuna/ingest/ingest.go` — `SourceWeather` const.
- `kahuna/compliance/filter.go` — aviationweather.gov allowlist hash.
- `kahuna/config/config.go` + `kahuna/main.go` — `WeatherConfig` wiring.
- `kahuna/monitor/monitor.go` — `SetWeatherSource` + `weather_*` healthz keys.
- `frontend/src/components/SystemStatus.tsx` — External-APIs weather card.
- `marketdata/discovery.go` / `run.sh` — add Climate category + priority series.
- Component B home (per decision #1): new gordon strategy path **or** a
  `weather-worker` module + deterministic fair-value model + bucket parser +
  station→series map.
- breen sim settlement (per decision #3): deterministic settlement for
  weather-tagged positions.
</content>
</invoke>
