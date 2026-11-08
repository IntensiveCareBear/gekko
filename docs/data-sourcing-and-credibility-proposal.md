# Data Sourcing & Credibility Proposal — stop treating any one feed as an oracle

*2026-07-08 — proposal / design sketch. Not started.*

## Problem

Gekko's trade quality is capped by its input quality, and today the input layer
is thin and credulous:

1. **Only two real news-type sources, both keyword-query driven.** Live ingestion
   is effectively (a) Google News RSS (`news.google.com/rss/search?q=…`, labelled
   `REUTERS` but actually a Google aggregator query) and (b) X/Twitter recent
   search. Both are English-only and both are steered by a hand-written boolean
   keyword query. Kalshi/Polymarket are market-data feeds, not news. There is no
   primary-source tier (government releases, SEC/EDGAR, official econ prints),
   no outlet diversification, no non-English, no other social platforms.

2. **Every authorized source is weighted equally — including X.** The compliance
   gatekeeper (`kahuna/compliance/filter.go`) is a binary domain *allowlist*:
   a URL is authorized or it isn't. Downstream, `SourceID` is carried on the
   event and copied onto the emitted proposal as metadata
   ([gordon/main.go](../gordon/main.go) ~734), but it **never enters the
   confidence/sizing math**. `SignalInputs` / `compositeConfidence`
   ([gordon/signals.go](../gordon/signals.go)) blend match score, momentum,
   volume surge, cross-venue edge, and a lexicon sentiment score — and nothing
   else. So an anonymous tweet and a wire-service story that embed to the same
   market match produce the **same** conviction and size. That is the literal
   "treat X as an oracle" failure.

3. **No per-author / per-outlet reputation.** On X, any `author_id` that matches
   the query passes. `MockTweet` even carries `Likes`/`Retweets`, but they are
   dropped — engagement isn't used as a weak credibility prior, and there's no
   allow/deny/reputation list for accounts. On the news side, all outlets behind
   the RSS aggregator are indistinguishable.

4. **No corroboration signal.** Each event is embedded and matched in isolation.
   Five independent outlets reporting the same fact and one rumor tweet are
   treated as six unrelated events, not "one well-corroborated event." There is
   no cross-source dedup that could *raise* confidence when sources agree or flag
   a lone-source claim.

5. **No bias / lean tracking.** We keep no notion of an outlet's or account's
   political/market lean or historical accuracy, so we can't discount a source
   that is reliably wrong or one-sided, nor surface *why* a proposal fired.

Net effect: a single loud account can move sizing as much as a corroborated wire
story, and we have no dial to correct it.

## Goal

Make **source credibility a first-class, tunable input** to conviction and
sizing, and **broaden + diversify** the sources feeding the matcher — without
turning Kahuna into a heavyweight NLP pipeline. Concretely:

- A per-event **credibility score** (source tier × author reputation ×
  corroboration) that flows into `SignalInputs` and moves confidence/size,
  the same way momentum and cross-venue edge already do.
- A **wider, rated source set**, each tagged with a tier and (where known) a
  bias/lean label, so the operator can see and tune how much each stream counts.
- Everything **fail-open and default-neutral**: absent scores collapse to today's
  behavior, so this ships incrementally without a flag day.

## Design

### 1. Credibility metadata on the event (Kahuna)

Extend `GekkoIngestEvent` ([kahuna/ingest/ingest.go](../kahuna/ingest/ingest.go))
with optional, self-describing credibility fields (all omitempty; old events and
other producers are unaffected):

```go
type SourceCredibility struct {
    Tier        int     `json:"tier,omitempty"`         // 1=primary/official, 2=major wire, 3=mainstream, 4=social/unverified
    Outlet      string  `json:"outlet,omitempty"`       // canonical outlet/account id, e.g. "reuters", "@federalreserve"
    Lean        string  `json:"lean,omitempty"`         // "left"|"center"|"right"|"unknown" (news) or "" (n/a)
    AuthorScore float64 `json:"author_score,omitempty"` // [0,1] reputation for the emitting account/outlet
    Engagement  int     `json:"engagement,omitempty"`   // weak prior: likes+RT (social) or 0
}
```

Populate it at the source:
- **Tier** from a static registry keyed by canonical outlet/domain (see §3).
- **Author reputation** from a small, editable JSON reputation file
  (`config/source_reputation.json`) — allow/deny plus a [0,1] score per account
  or outlet; unknown ⇒ tier default. This is where "@federalreserve = 1.0,
  random anon = 0.3" lives, hot-reloaded on mtime like Lou's lesson book.
- **Engagement** from the X `public_metrics` we already fetch (currently dropped).

The compliance gatekeeper stays a hard allowlist (security boundary); credibility
is a *separate, softer* layer that grades what's already allowed. Don't conflate
the two.

### 2. Fold credibility into conviction (Gordon)

Add one field to `SignalInputs` and one term to `compositeConfidence`
([gordon/signals.go](../gordon/signals.go)):

```go
type SignalInputs struct {
    // …existing…
    Credibility float64 // [0,1]; 0.5 = neutral/unknown. Below 0.5 discounts, above rewards.
}
```

- Map the event's `SourceCredibility` → a single `[0,1]` credibility scalar in
  `main.go` before building `SignalInputs` (tier weight × author score, nudged by
  a capped engagement prior and by the corroboration count from §4).
- In `compositeConfidence`, add a **direction-independent** multiplier/penalty:
  low credibility shrinks both confidence and `sizeMult`; high credibility gives a
  small bounded boost. Unlike momentum/sentiment this is *not* signed by trade
  direction — a dubious source is dubious whether it says BUY or SELL. Keep the
  effect bounded (e.g. size ×[0.5, 1.15]) so it discounts noise without letting a
  single "trusted" source dominate.
- Neutral default (0.5) ⇒ byte-for-byte identical to today, so this is safe to
  land dark and tune up.

Surface the credibility term in the proposal `notes` (it already carries
"momentum …", "sentiment …") so the Strategy Stream can show *why* a trade was
sized down — pairs with the persona-labeling backlog item.

### 3. Broaden & tier the source set (Kahuna)

Introduce a **source registry** (`kahuna/ingest/registry.go` + a JSON the
operator can edit) mapping canonical outlet/domain → `{tier, lean}`. Seed it and
grow the live set beyond the current two:

- **Tier 1 — primary/official:** Federal Reserve / Treasury / BLS / SEC EDGAR
  press feeds, official govt RSS, exchange notices. These are the closest thing to
  ground truth for the macro/econ markets we trade.
- **Tier 2 — major wires:** Reuters, AP, Bloomberg (real feeds, not the Google
  aggregator standing in for "Reuters" today).
- **Tier 3 — mainstream outlets:** a diversified basket across the lean spectrum
  so we're not reading one editorial slant; each tagged with a lean label.
- **Tier 4 — social/unverified:** X/Twitter (kept), optionally Reddit or others,
  always entering at low base credibility and only rising via author reputation +
  corroboration.

Each new source is just another `IngestionSource` (the plugin contract already
exists); the registry supplies its tier/lean at emit time. Keep everything behind
the existing simulated/live gating and per-source budgets.

### 4. Corroboration (Kahuna, lightweight)

Before emitting, hash-and-bucket recent events by a coarse semantic key (e.g.
normalized entities / a cheap minhash of `RawContent`) over a short rolling
window. Attach a **corroboration count** = number of *distinct outlets* reporting
the same thing recently. Feed that into the credibility scalar in §2: a claim
carried by four independent tier-2/3 outlets scores well above the same claim
from one tier-4 account. This is the mechanism that stops one loud tweet from
mattering as much as a corroborated story, and it's the highest-leverage piece.
(No heavy NLP required — coarse bucketing over a minutes-long window is enough.)

### 5. Operator visibility (Frontend)

- On the **System/External-APIs** cards, show each live source with its tier and
  recent volume (extends the existing Twitter/Gemini health cards).
- In the **Strategy Stream**, show the source + credibility badge on each
  proposal (dovetails with the persona-labeling backlog item), so a low-credibility
  trade is visibly marked.
- Optionally a **Controls** knob for the global credibility weight (how hard low
  credibility discounts), wired like the other model-controls knobs.

## Phasing

**Phase 1 — make source count, without new sources (highest leverage, ~1 sitting).**
Add `SourceCredibility` to the event, a static tier table for the *existing*
sources (X = tier 4, RSS = tier 2/3), the `source_reputation.json` file, and the
`Credibility` term in `compositeConfidence` (neutral default). Immediately stops
X from being an oracle; no new feeds, no UI required.

**Phase 2 — corroboration.** Add the rolling cross-source dedup/bucketing and the
corroboration count into the credibility scalar.

**Phase 3 — broaden sources.** Add tier-1 official feeds and real tier-2 wires;
diversify tier-3 with lean labels; grow the registry. Each is an independent
`IngestionSource`.

**Phase 4 — visibility & tuning.** Source/credibility badges in the Strategy
Stream, tiers on the System cards, and (optional) a global credibility-weight
knob on Controls.

## Notes / non-goals

- **Not** building a bias-detection ML model. "Lean" is a static, human-editable
  label per outlet, and "reputation" is an editable score file — both start
  coarse and get corrected over time (Lou could eventually propose reputation
  adjustments from realized outcomes, but that's a later tie-in, not scope here).
- **Compliance stays a hard boundary.** Credibility grades the allowed set; it
  never widens it. A source must pass the gatekeeper *and* then gets a tier.
- **Everything degrades to today.** Missing credibility ⇒ 0.5 neutral ⇒ current
  behavior, so each phase is safe to land dark and tune.
- Tie-ins: persona labeling / Strategy-Stream work (badges), Lou (outcome-driven
  reputation), the econ-calendar blackout already in `gordon/main.go` (a tier-1
  signal we already special-case — the registry generalizes that idea).

## Appendix — Phase 1 concrete implementation scope (~1 sitting)

Verified against the code 2026-07-09. Ships dark (neutral 0.5 ⇒ byte-identical to
today). Order: Kahuna emit → Gordon consume → tune.

**Kahuna (emit a tier on every event):**
1. `kahuna/ingest/ingest.go` — add `Credibility *SourceCredibility` (omitempty) to
   `GekkoIngestEvent` with the `SourceCredibility` struct from §1. Keep it a
   pointer so existing producers/tests and quarantine JSON are unaffected.
2. `kahuna/ingest/registry.go` (new) — `func BaseTier(src SourceID) int` static
   table: `FRED`=1, `REUTERS`/`FINNHUB`=2, `TWITTER`=4 (Reddit will register as 4).
   Load optional `config/source_reputation.json` (allow/deny + [0,1] per
   outlet/account) with mtime hot-reload, mirroring Lou's `LessonBook` pattern
   (`gordon/lessons.go`) — return neutral when absent (fail-open).
3. Populate at each source's emit site (`twitter.go`, `reuters.go`, `finnhub.go`,
   `fred.go`, `reddit.go`): set `Tier` from `BaseTier`, `Outlet`/`AuthorScore`
   from the reputation file, and `Engagement = Likes+Retweets` for Twitter/Reddit
   (currently dropped). One helper `attachCredibility(ev, outlet, engagement)`.

**Gordon (fold into conviction):**
4. `gordon/signals.go` — add `Credibility float64 // [0,1]; 0.5 neutral` to
   `SignalInputs` (line 28). In `compositeConfidence` (line 40), after the signed
   `adj` block, apply a **direction-independent** factor:
   `credFactor := clamp(1.0+(in.Credibility-0.5)*1.0, 0.5, 1.15)` then
   `sizeMult *= credFactor` and `confidence *= clamp(credFactor, 0.85, 1.05)`
   (gentler on confidence than on size). Guard `if in.Credibility != 0` so the
   zero-value stays a no-op; append a `notes` line `credibility 0.NN`. Neutral 0.5
   ⇒ credFactor 1.0 ⇒ unchanged.
5. `gordon/main.go` (~734, where `SourceID` is copied to proposal metadata) — map
   the event's `SourceCredibility` → one `[0,1]` scalar
   (`tierWeight[tier] * authorScore`, nudged by a capped engagement prior) and set
   `SignalInputs.Credibility` before calling `compositeConfidence`. Missing
   metadata ⇒ 0.5. `tierWeight`: `{1:1.0, 2:0.8, 3:0.65, 4:0.45}`.
6. Tests: `signals_test.go` — tier-1 sizes up vs tier-4 on identical signals;
   0.5/zero is a no-op (regression lock).

**Explicitly deferred to later phases:** corroboration count (Phase 2), the
`source_reputation.json` growing real per-account scores, UI badges (Phase 4). The
engagement prior and reputation file can land as no-ops in Phase 1 (tier alone is
the immediate win that stops X from being an oracle).

**Reddit tie-in:** Reddit ships as an `IngestionSource` (see the reddit-integration
work) entering at tier 4 like X — so it must land credibility Phase 1 first, or at
minimum register in `BaseTier`, else it's another equal-weighted oracle.
