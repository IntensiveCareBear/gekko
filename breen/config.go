package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DryMode             bool
	RedisAddr           string
	KafkaBrokers        []string
	BrokerToken         string
	BrokerSocketPath    string
	KafkaInboundTopic   string
	KafkaProposalsTopic string
	LedgerFilePath      string

	// Paper-trading run settings.
	StartingCapital  float64
	AutoApprove      bool
	ExpiryMin        time.Duration
	ExpiryMax        time.Duration
	SnapshotInterval time.Duration
	RunExportDir     string

	// Portfolio risk gate caps (0 = unlimited for that dimension).
	MaxContractExposure    float64
	MaxContractExposurePct float64 // per-contract notional cap as a fraction of NAV (0 = off)
	MaxCategoryExposure    float64 // combined notional cap across a correlation category (0 = off)
	MaxCategoryExposurePct float64 // category cap as a fraction of NAV (0 = off)
	MaxTotalDeployed       float64
	MaxOpenPositions       int
	RejectOnBreach         bool

	// Post-mortem risk guards (0/empty = disabled).
	PerContractLossLimit   float64       // halt new entries on a contract once its cumulative realized PnL ≤ -limit
	PerContractLossCloses  bool          // also CLOSE a family's open positions once its bleed passes the loss limit
	ExitCooldown           time.Duration // min wait before re-entering a contract after closing it
	MaxDrawdownHaltPct     float64       // halt all new entries once NAV is this far below starting capital
	SizingEnabled          bool          // ramp BUY size down between DrawdownEaseStart and the halt
	DrawdownEaseStart      float64       // drawdown % where the size ramp begins
	DrawdownEaseFloor      float64       // size scalar reached at the halt threshold
	ForceExitBeforeExpiry  time.Duration // close open positions this long before resolution (avoid NO→$0 settlement)
	StopLossPct            float64       // close a position once its unrealized loss reaches this % of entry cost
	StopLossUSD            float64       // close a position once its unrealized loss reaches this many dollars
	StopLossMinAbs         float64       // no stop fires unless the per-contract adverse move also exceeds this many dollars
	TakeProfitPct          float64       // bank a position once its unrealized gain reaches this % of entry cost (0 disables)
	TakeProfitUSD          float64       // bank a position once its unrealized gain reaches this many dollars (0 disables)
	TrailArmPct            float64       // trailing stop arms once a position has run this % above entry (0 disables)
	TrailRetracePct        float64       // ...and fires once it gives back this % of entry from its best mark (0 disables)
	SettlementHoldPersonas string        // comma-separated personas whose positions hold to resolution (mark-based exits skipped; dollar backstops kept)
	SettlementHoldTPPrice  float64       // absolute bid at which a settlement-hold position is banked anyway (0 = hold all the way to resolution)
	ShadowPersonas         string        // comma-separated personas that keep proposing but never fill (recorded to the shadow book instead)
	ShadowBarProposals     int           // actionable shadow proposals required before the re-activation bar can be met
	ShadowBarMinContracts  int           // distinct contracts those proposals must span, so volume is not mistaken for evidence
	ShadowScoreInterval    time.Duration // how often the shadow book is scored against realized outcomes
	MarkFallbackHaircut    float64       // realization mark = entry × haircut when no live/cached quote exists
	MarkStaleMaxAge        time.Duration // how long a cached live mark stays usable as a fallback
	SettlePrior            string        // sim-settlement YES-probability source when no real Kalshi result: "mark" (freshest mark) or "entry" (entry price)
	MetricsMinSamples      int           // min per-persona/book trades before Sharpe/Sortino are reported (below it the scorecard shows "—")
	CategoryLossLimit      float64       // halt new entries in a correlation category once its cumulative PnL ≤ -limit
	OneStrikePerFamily     bool          // reject a BUY on a family strike while a DIFFERENT strike of that family is already open
	MidBandLow             float64       // mid-band conviction gate: band lower bound
	MidBandHigh            float64       // mid-band conviction gate: band upper bound
	MidBandMinConfidence   float64       // min confidence for BUYs priced inside the band (0 disables)
	MinClipNotional        float64       // reject entries whose notional is below this — too small to carry round-trip friction (0 disables)
	MaxEntriesPerDay       int           // cap on entries that FILL per calendar day; churn pays the vig per round trip (0 disables)
	MaxFamilyEntriesPerDay int           // cap on round trips started per contract family per day; also blocks sibling-strike re-entry (0 disables)
	FamilyBlacklist        string        // comma-separated contract families refused outright; Lou may append at the `intervene` autonomy level

	// Entry book-quality gate (2026-07-05 post-mortem): BUYs must fill against a
	// live, fresh, tight order book — never a fallback proposal price.
	EntryRequireLiveBook  bool          // refuse BUYs with no live two-sided book (kills the proposal-price fallback fill)
	EntryMaxSpreadPct     float64       // max top-of-book spread as a fraction of mid (0 = off)
	EntryMaxSpreadAbs     float64       // absolute spread tolerance in dollars — spreads at/below this always pass the pct check
	BookFreshMaxAge       time.Duration // max as_of_ms age of a book usable for entry fills and live marks (0 = off)
	EntryEdgeFrictionMult float64       // a BUY needs modelled edge ≥ this × round-trip friction at the real fill (0 = off)

	// Execution-cost accounting (breen-intelligence Phase 0). FeeModel RECORDS the
	// modelled venue fee on every fill and close; FeeCharge additionally DEBITS it
	// from the paper account's cash. Separate knobs on purpose: recording alone
	// exposes the drag on the scorecard without breaking P&L comparability with
	// earlier runs, and charging is the honest-books step to take before live mode.
	KalshiFeeModel  bool
	KalshiFeeCharge bool
	KalshiFeeRate   float64 // coefficient in fee = ceil(rate × contracts × P × (1−P))

	// Confidence calibration. CalibrationBins (Phase 0) sets the number of
	// stated-confidence buckets in the per-persona table, which is always computed and
	// published. CalibrationEnabled (Phase 1) is what makes it change decisions:
	// consensus conviction and the mid-band gate then reason on confidence rescaled by
	// each persona's own record at that confidence level. Off by default — turn it on
	// only once the published table shows miscalibration worth correcting.
	CalibrationBins       int
	CalibrationEnabled    bool
	CalibrationMinSamples int     // per-bucket trades needed before its record overrides the stated value
	CalibrationFactorMin  float64 // floor on the rescaling factor (most a bucket can cut conviction)
	CalibrationFactorMax  float64 // ceiling on it (most a bucket can raise conviction)

	// Consensus engine (Phase 3): buffer competing persona proposals and consolidate.
	ConsensusEnabled       bool
	ConsensusWindow        time.Duration
	ConsensusMinConviction float64
	// ConsensusBearVeto weighs bearish signals (unheld SELLs from bear personas)
	// against concurrent BUYs on the same event, shrinking or vetoing the long.
	ConsensusBearVeto bool
	// ConsensusBearCacheTTL keeps each bearish signal live for this long after its
	// window so a BUY on the same contract/event can still be vetoed by it. Without
	// it the veto only fired when bear and bull landed in the same ~3s window, which
	// never happened (persona skew is minutes): the veto had never engaged in
	// production. 0 disables the cache (window-only, legacy behavior).
	ConsensusBearCacheTTL time.Duration

	// Adaptive allocation (Phase 4): bandit-style persona weighting that learns from
	// realized performance. Off by default — needs settled trades to be meaningful.
	AdaptiveEnabled      bool
	AdaptiveDecay        float64
	AdaptiveGain         float64
	AdaptiveExploreFloor float64
	AdaptiveMinWeight    float64 // lower clamp on adaptive weights (must drop with the explore floor — the floor can't bind below it)
	AdaptiveMinTrades    int

	// Persona kill-switch (2026-07-03 post-mortem): bench a persona at 0 weight for
	// BenchDuration once it has ≥ BenchMinTrades settled trades with EWMA score below
	// BenchScore. BenchMinTrades 0 disables.
	PersonaBenchScore     float64
	PersonaBenchMinTrades int
	PersonaBenchDuration  time.Duration
}

func LoadConfig() *Config {
	// Support both EXECUTIVE_DRY_MODE and GEKKO_DRY_MODE, default to true
	dryMode := true
	if val, ok := os.LookupEnv("EXECUTIVE_DRY_MODE"); ok {
		if b, err := strconv.ParseBool(val); err == nil {
			dryMode = b
		}
	} else if val, ok := os.LookupEnv("GEKKO_DRY_MODE"); ok {
		if b, err := strconv.ParseBool(val); err == nil {
			dryMode = b
		}
	}

	brokersEnv := getEnv("KAFKA_BROKERS", "localhost:9092")
	brokers := strings.Split(brokersEnv, ",")
	for i, b := range brokers {
		brokers[i] = strings.TrimSpace(b)
	}

	return &Config{
		DryMode:             dryMode,
		RedisAddr:           getEnv("REDIS_ADDR", "localhost:6379"),
		KafkaBrokers:        brokers,
		BrokerToken:         getEnv("GEKKO_BROKER_TOKEN", ""),
		BrokerSocketPath:    getEnv("BROKER_SOCKET_PATH", "/tmp/gekko-broker.sock"),
		KafkaInboundTopic:   getEnv("KAFKA_INBOUND_TOPIC", "kahuna.ingest.public"),
		KafkaProposalsTopic: getEnv("KAFKA_PROPOSALS_TOPIC", "gordon.strategies.proposed"),
		LedgerFilePath:      getEnv("LEDGER_FILE_PATH", "ledger.db"),

		StartingCapital:  getEnvFloat("STARTING_CAPITAL", 100000),
		AutoApprove:      getEnvBool("EXECUTIVE_AUTO_APPROVE", false),
		ExpiryMin:        getEnvDuration("EXPIRY_MIN", 2*time.Hour),
		ExpiryMax:        getEnvDuration("EXPIRY_MAX", 48*time.Hour),
		SnapshotInterval: getEnvDuration("SNAPSHOT_INTERVAL", 5*time.Minute),
		RunExportDir:     getEnv("RUN_EXPORT_DIR", "./runs"),

		// Default caps sized for the $100k paper account. The 27h post-mortem showed
		// a single $5k+ contract (KXRATECUT) drove 48% of losses, so per-contract
		// exposure is now bound by the TIGHTER of an absolute $15k and 2% of NAV
		// (≈$2k on the seed account). Total deployed ≤ $60k (keep dry powder), ≤ 25
		// open positions.
		MaxContractExposure:    getEnvFloat("MAX_CONTRACT_EXPOSURE", 15000),
		MaxContractExposurePct: getEnvFloat("MAX_CONTRACT_EXPOSURE_PCT", 0.02),
		// Correlation-category cap (#5): ≤6% of NAV (≈$6k, ~3 families at the per-contract
		// cap) combined across a macro/index/crypto cluster, so one shock can't sink an
		// oversized correlated book at once.
		MaxCategoryExposure:    getEnvFloat("MAX_CATEGORY_EXPOSURE", 0),
		MaxCategoryExposurePct: getEnvFloat("MAX_CATEGORY_EXPOSURE_PCT", 0.06),
		MaxTotalDeployed:       getEnvFloat("MAX_TOTAL_DEPLOYED", 60000),
		MaxOpenPositions:       int(getEnvFloat("MAX_OPEN_POSITIONS", 25)),
		RejectOnBreach:         getEnvBool("RISK_REJECT_ON_BREACH", true),

		// Post-mortem guards. Defaults active: stop throwing good money after bad on a
		// single contract (−$300 cumulative), 30-min cooldown between re-entries to cut
		// churn, halt new entries past a 10% drawdown, and force-exit positions 1h
		// before resolution so a NO never decays to $0 on settlement.
		PerContractLossLimit:  getEnvFloat("PER_CONTRACT_LOSS_LIMIT", 300),
		PerContractLossCloses: getEnvBool("PER_CONTRACT_LOSS_CLOSES", true),
		ExitCooldown:          getEnvDuration("EXIT_COOLDOWN", 30*time.Minute),
		MaxDrawdownHaltPct:    getEnvFloat("MAX_DRAWDOWN_HALT_PCT", 10),
		SizingEnabled:         getEnvBool("SIZING_ENABLED", false),
		DrawdownEaseStart:     getEnvFloat("DRAWDOWN_EASE_START", 4),
		DrawdownEaseFloor:     getEnvFloat("DRAWDOWN_EASE_FLOOR", 0.25),
		ForceExitBeforeExpiry: getEnvDuration("FORCE_EXIT_BEFORE_EXPIRY", time.Hour),
		// Position stop-loss (2026-07-03 post-mortem): without one, 51 trades rode past
		// -20% for -$8.5k. Close on the FIRST of -15% of cost or -$300 unrealized,
		// evaluated each settlement tick against LIVE marks only.
		StopLossPct:    getEnvFloat("STOP_LOSS_PCT", 15),
		StopLossUSD:    getEnvFloat("STOP_LOSS_USD", 300),
		StopLossMinAbs: getEnvFloat("STOP_LOSS_MIN_ABS", 0.02),
		// Position take-profit (2026-07-11, harrier-toolkit idea #1): mirror of the stop.
		// Both limbs default off — banking winners early changes trading behavior and is
		// an operator opt-in; enable via TAKE_PROFIT_PCT / TAKE_PROFIT_USD.
		TakeProfitPct: getEnvFloat("TAKE_PROFIT_PCT", 0),
		TakeProfitUSD: getEnvFloat("TAKE_PROFIT_USD", 0),
		// Trailing stop (Phase 3). Off by default: it changes exit behaviour, so it is
		// an operator opt-in like take-profit. The Phase 0 excursion data is what
		// justifies it existing — 15 of the first 31 measured trades ran +10% or better
		// and collectively realized +$27.93, 7 of them closing red.
		TrailArmPct:     getEnvFloat("TRAIL_ARM_PCT", 0),
		TrailRetracePct: getEnvFloat("TRAIL_RETRACE_PCT", 0),
		// Settlement-hold (2026-08-25 desk review fix #2). Empty = off, so this lands
		// inert like every other behaviour change; SETTLEMENT_HOLD_PERSONAS=weather is
		// the configuration the review's +$2,950 counterfactual describes.
		SettlementHoldPersonas: getEnv("SETTLEMENT_HOLD_PERSONAS", ""),
		// Settlement-hold take-profit (2026-09-03 forensics). An ABSOLUTE price, not a
		// percentage of entry: it is not about how big the gain got, it is about the
		// contract having re-priced to near-certainty, at which point the settlement
		// edge has already been paid out by the market and only the tail risk is left.
		// 0 = off. 0.95 is the level the forensics priced; see settlementhold.go.
		SettlementHoldTPPrice: getEnvFloat("SETTLEMENT_HOLD_TP_PRICE", 0),
		// Shadow mode (desk review fix #3). Empty = off. The review's target
		// configuration is the whole LLM fleet, leaving weather as the only book that
		// fills; re-activation is a written bar (positive net-of-fee expectancy over
		// ~200 shadow proposals), not a hunch.
		ShadowPersonas: getEnv("SHADOW_PERSONAS", ""),
		// The re-activation bar's denominator, and how often the shadow book is scored
		// against it (2026-08-30). 200 is the review's own wording. Only actionable
		// proposals count — see shadowscore.go on why bearish signals cannot.
		ShadowBarProposals: int(getEnvFloat("SHADOW_BAR_PROPOSALS", 200)),
		// The breadth half of the bar (2026-09-03). 200 proposals that span fewer than
		// this many distinct contracts are one trade re-counted, not 200 observations:
		// on 09-03 the book's 118 actionable proposals covered 27 contracts, 65 of them
		// a single weekly Brent series. A quarter of the proposal threshold means "no
		// more than four bites at the same apple, on average". 0 disables the check.
		ShadowBarMinContracts: int(getEnvFloat("SHADOW_BAR_MIN_CONTRACTS", getEnvFloat("SHADOW_BAR_PROPOSALS", 200)/4)),
		ShadowScoreInterval:   getEnvDuration("SHADOW_SCORE_INTERVAL", 30*time.Minute),
		// Honest marks (2026-07-03 post-mortem): a realization with no live book is
		// valued at the last live mark seen within 30m, else entry × 0.5 — never a
		// flattering "flat at entry" fill.
		MarkFallbackHaircut: getEnvFloat("MARK_FALLBACK_HAIRCUT", 0.5),
		MarkStaleMaxAge:     getEnvDuration("MARK_STALE_MAX_AGE", 30*time.Minute),
		// Sim-settlement honesty (breen-intelligence Phase 0): when a market expires with
		// no real Kalshi resolution, resolve YES with probability = the freshest mark, not
		// the stale entry price (a position that cratered 0.60→0.10 should settle YES ~10%,
		// not 60%). "entry" restores the old behavior so past runs stay comparable.
		SettlePrior: normalizeSettlePrior(getEnv("SETTLE_PRIOR", "mark")),
		// Cost accounting (breen-intelligence Phase 0): model the Kalshi fee on every
		// leg so expectancy stops being biased by an uncounted cost, but do NOT charge
		// it to cash yet — flipping KALSHI_FEE_CHARGE changes realized P&L and makes a
		// run incomparable with every run before it, which is the operator's call.
		KalshiFeeModel:  getEnvBool("KALSHI_FEE_MODEL", true),
		KalshiFeeCharge: getEnvBool("KALSHI_FEE_CHARGE", false),
		KalshiFeeRate:   getEnvFloat("KALSHI_FEE_RATE", defaultKalshiFeeRate),
		CalibrationBins: int(getEnvFloat("CALIBRATION_BINS", defaultCalibrationBins)),
		// Phase 1 lands inert. Calibration can only shrink or grow the conviction the
		// entry gates were tuned against, and this gate has flatlined the funnel twice
		// (2026-07-08, 2026-07-12) — so it goes on deliberately, after reading the
		// table, with the entry count watched for the run that follows.
		CalibrationEnabled:    getEnvBool("CALIBRATION_ENABLED", false),
		CalibrationMinSamples: int(getEnvFloat("CALIBRATION_MIN_SAMPLES", 20)),
		CalibrationFactorMin:  getEnvFloat("CALIBRATION_FACTOR_MIN", defaultCalibrationFactorMin),
		CalibrationFactorMax:  getEnvFloat("CALIBRATION_FACTOR_MAX", defaultCalibrationFactorMax),
		// Small-sample guard for the risk-adjusted scorecard: Sharpe/Sortino need enough
		// trades to mean anything, so hold them back until a persona/book clears this.
		MetricsMinSamples: int(getEnvFloat("METRICS_MIN_SAMPLES", 20)),
		// Entry gates (2026-07-03 post-mortem): pause a whole correlation category
		// once it has bled -$500 this run, and require strong conviction for
		// coin-flip-priced (0.35–0.65) BUYs. The confidence floor is judged on raw
		// persona conviction (midBandConfidence); 0.75 produced zero passing entries
		// over a full run (2026-07-08 flatline) because the gordon entry floor/ceiling
		// confines all entries to this band — 0.70 re-opens a selective funnel.
		CategoryLossLimit: getEnvFloat("CATEGORY_LOSS_LIMIT", 500),
		// One strike per family (2026-07-14 forensics): the run is YES-only, so two
		// strikes of one event (KXGDP-26JUL30 T2.0 + T2.5) are the same directional
		// bet — they stopped out 25m apart on one GDP move. The family notional cap
		// bounds their COMBINED size but still lets the ladder stack; this collapses a
		// family to a single active strike so conviction concentrates instead of
		// spreading across correlated strikes. Adds to the SAME strike still pass.
		OneStrikePerFamily:   getEnvBool("ONE_STRIKE_PER_FAMILY", true),
		MidBandLow:           getEnvFloat("MID_BAND_LOW", 0.35),
		MidBandHigh:          getEnvFloat("MID_BAND_HIGH", 0.65),
		MidBandMinConfidence: getEnvFloat("MID_BAND_MIN_CONFIDENCE", 0.70),
		// Friction gates (desk review fix #4), both off by default. The review's target
		// shape: MIN_CLIP_NOTIONAL=200 (median clip was $17.70, and the 204 clips under
		// $25 paid 7.7% friction) and MAX_ENTRIES_PER_DAY≈40 (341 trades over 16 days).
		MinClipNotional:        getEnvFloat("MIN_CLIP_NOTIONAL", 0),
		MaxEntriesPerDay:       int(getEnvFloat("MAX_ENTRIES_PER_DAY", 0)),
		MaxFamilyEntriesPerDay: int(getEnvFloat("MAX_FAMILY_ENTRIES_PER_DAY", 0)),
		FamilyBlacklist:        getEnv("FAMILY_BLACKLIST", ""),

		// Entry book-quality gate (2026-07-05 post-mortem: 31,900 dry fills fell back
		// to proposal prices and stopped out instantly against real mids). Spread cap
		// is the LOOSER of 10% of mid and an absolute $0.02, so cheap contracts with
		// penny-wide books still qualify.
		EntryRequireLiveBook: getEnvBool("ENTRY_REQUIRE_LIVE_BOOK", true),
		EntryMaxSpreadPct:    getEnvFloat("ENTRY_MAX_SPREAD_PCT", 0.10),
		EntryMaxSpreadAbs:    getEnvFloat("ENTRY_MAX_SPREAD_ABS", 0.02),
		BookFreshMaxAge:      getEnvDuration("BOOK_FRESH_MAX_AGE", 90*time.Second),
		// Edge-vs-friction gate (desk review fix #4). The review's shape is 2×: an entry
		// must show at least twice the round-trip cost in modelled edge. Off by default,
		// and inert for any proposal whose strategy supplies no fair value.
		EntryEdgeFrictionMult: getEnvFloat("ENTRY_EDGE_FRICTION_MULT", 0),

		ConsensusEnabled:       getEnvBool("CONSENSUS_ENABLED", false),
		ConsensusWindow:        getEnvDuration("CONSENSUS_WINDOW", 3*time.Second),
		ConsensusMinConviction: getEnvFloat("CONSENSUS_MIN_CONVICTION", 0.4),
		ConsensusBearVeto:      getEnvBool("CONSENSUS_BEAR_VETO", true),
		ConsensusBearCacheTTL:  getEnvDuration("CONSENSUS_BEAR_CACHE_TTL", 10*time.Minute),

		AdaptiveEnabled: getEnvBool("ADAPTIVE_ALLOCATION_ENABLED", false),
		AdaptiveDecay:   getEnvFloat("ADAPTIVE_DECAY", 0.3),
		AdaptiveGain:    getEnvFloat("ADAPTIVE_GAIN", 0.5),
		// Floor and min-weight both dropped 0.5 → 0.25 (2026-07-03 post-mortem:
		// 12–23%% win-rate personas kept trading at ≥50%% weight). They must move
		// together — weightFor clamps to minW before applying the floor.
		AdaptiveExploreFloor: getEnvFloat("ADAPTIVE_EXPLORE_FLOOR", 0.25),
		AdaptiveMinWeight:    getEnvFloat("ADAPTIVE_MIN_WEIGHT", 0.25),
		AdaptiveMinTrades:    int(getEnvFloat("ADAPTIVE_MIN_TRADES", 5)),

		PersonaBenchScore:     getEnvFloat("PERSONA_BENCH_SCORE", -0.30),
		PersonaBenchMinTrades: int(getEnvFloat("PERSONA_BENCH_MIN_TRADES", 20)),
		PersonaBenchDuration:  getEnvDuration("PERSONA_BENCH_DURATION", 12*time.Hour),
	}
}

// Snapshot returns the run configuration as a flat, human-readable map for
// export bundles (durations as strings, credentials omitted).
func (c *Config) Snapshot() map[string]interface{} {
	return map[string]interface{}{
		"dry_mode":          c.DryMode,
		"starting_capital":  c.StartingCapital,
		"auto_approve":      c.AutoApprove,
		"expiry_min":        c.ExpiryMin.String(),
		"expiry_max":        c.ExpiryMax.String(),
		"snapshot_interval": c.SnapshotInterval.String(),

		"max_contract_exposure":     c.MaxContractExposure,
		"max_contract_exposure_pct": c.MaxContractExposurePct,
		"max_category_exposure":     c.MaxCategoryExposure,
		"max_category_exposure_pct": c.MaxCategoryExposurePct,
		"max_total_deployed":        c.MaxTotalDeployed,
		"max_open_positions":        c.MaxOpenPositions,
		"reject_on_breach":          c.RejectOnBreach,

		"per_contract_loss_limit":    c.PerContractLossLimit,
		"per_contract_loss_closes":   c.PerContractLossCloses,
		"exit_cooldown":              c.ExitCooldown.String(),
		"one_strike_per_family":      c.OneStrikePerFamily,
		"max_drawdown_halt_pct":      c.MaxDrawdownHaltPct,
		"force_exit_before_expiry":   c.ForceExitBeforeExpiry.String(),
		"stop_loss_pct":              c.StopLossPct,
		"stop_loss_usd":              c.StopLossUSD,
		"stop_loss_min_abs":          c.StopLossMinAbs,
		"take_profit_pct":            c.TakeProfitPct,
		"take_profit_usd":            c.TakeProfitUSD,
		"trail_arm_pct":              c.TrailArmPct,
		"trail_retrace_pct":          c.TrailRetracePct,
		"settlement_hold_personas":   c.SettlementHoldPersonas,
		"settlement_hold_tp_price":   c.SettlementHoldTPPrice,
		"shadow_personas":            c.ShadowPersonas,
		"shadow_bar_proposals":       c.ShadowBarProposals,
		"shadow_bar_min_contracts":   c.ShadowBarMinContracts,
		"mark_fallback_haircut":      c.MarkFallbackHaircut,
		"mark_stale_max_age":         c.MarkStaleMaxAge.String(),
		"settle_prior":               c.SettlePrior,
		"metrics_min_samples":        c.MetricsMinSamples,
		"category_loss_limit":        c.CategoryLossLimit,
		"mid_band_low":               c.MidBandLow,
		"mid_band_high":              c.MidBandHigh,
		"mid_band_min_confidence":    c.MidBandMinConfidence,
		"min_clip_notional":          c.MinClipNotional,
		"max_entries_per_day":        c.MaxEntriesPerDay,
		"max_family_entries_per_day": c.MaxFamilyEntriesPerDay,
		"family_blacklist":           c.FamilyBlacklist,

		"entry_require_live_book":  c.EntryRequireLiveBook,
		"entry_max_spread_pct":     c.EntryMaxSpreadPct,
		"entry_max_spread_abs":     c.EntryMaxSpreadAbs,
		"book_fresh_max_age":       c.BookFreshMaxAge.String(),
		"entry_edge_friction_mult": c.EntryEdgeFrictionMult,

		"kalshi_fee_model":        c.KalshiFeeModel,
		"kalshi_fee_charge":       c.KalshiFeeCharge,
		"kalshi_fee_rate":         c.KalshiFeeRate,
		"calibration_bins":        c.CalibrationBins,
		"calibration_enabled":     c.CalibrationEnabled,
		"calibration_min_samples": c.CalibrationMinSamples,
		"calibration_factor_min":  c.CalibrationFactorMin,
		"calibration_factor_max":  c.CalibrationFactorMax,

		"consensus_enabled":        c.ConsensusEnabled,
		"consensus_window":         c.ConsensusWindow.String(),
		"consensus_min_conviction": c.ConsensusMinConviction,
		"consensus_bear_veto":      c.ConsensusBearVeto,
		"consensus_bear_cache_ttl": c.ConsensusBearCacheTTL.String(),

		"adaptive_enabled":       c.AdaptiveEnabled,
		"adaptive_decay":         c.AdaptiveDecay,
		"adaptive_gain":          c.AdaptiveGain,
		"adaptive_explore_floor": c.AdaptiveExploreFloor,
		"adaptive_min_weight":    c.AdaptiveMinWeight,
		"adaptive_min_trades":    c.AdaptiveMinTrades,

		"persona_bench_score":      c.PersonaBenchScore,
		"persona_bench_min_trades": c.PersonaBenchMinTrades,
		"persona_bench_duration":   c.PersonaBenchDuration.String(),
	}
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

// normalizeSettlePrior clamps SETTLE_PRIOR to the two supported modes, defaulting
// any unrecognized value to "mark" (the honest default).
func normalizeSettlePrior(v string) string {
	if strings.ToLower(strings.TrimSpace(v)) == "entry" {
		return "entry"
	}
	return "mark"
}

func getEnvFloat(key string, defaultVal float64) float64 {
	if val, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return defaultVal
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return defaultVal
}
