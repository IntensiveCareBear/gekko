package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

type Position struct {
	ContractID string    `json:"contract_id"`
	EntryPrice float64   `json:"entry_price"`
	Size       int       `json:"size"`
	Timestamp  time.Time `json:"timestamp"`
	Expiry     time.Time `json:"expiry"` // when the contract resolves YES/NO (paper run)
	// Attribution: Persona is the order's identity ("consensus" for consolidated
	// orders); Contributors are the personas that actually voted for it, so per-persona
	// stats and the adaptive allocator credit each one on close.
	Persona      string   `json:"persona,omitempty"`
	StrategyID   string   `json:"strategy_id,omitempty"`
	Contributors []string `json:"contributors,omitempty"`
	// ContributorProposals carries each contributing persona's OWN proposed entry
	// price and size (pre-consolidation). At close, each persona is scored against
	// the actual outcome using its own proposal, so a consensus order yields genuine
	// per-persona differentiation instead of crediting every contributor with the
	// identical consolidated result.
	ContributorProposals []ContributorProposal `json:"contributor_proposals,omitempty"`
	// SettleResolution is a deterministic settlement outcome ("YES"/"NO") carried from
	// a strategy that knows the result from real-world ground truth (the weather
	// strategy on an already-cleared bucket). settleExpired prefers a real Kalshi
	// resolution, then this, and only then the probabilistic sim fallback.
	SettleResolution string `json:"settle_resolution,omitempty"`
	// BestMark/WorstMark are the running extremes of the LIVE marks seen while this
	// position was open, folded in on every settlement tick (breen-intelligence
	// Phase 0). They become the closed trade's MFE/MAE — the dataset every exit-policy
	// question needs ("would a trailing stop at X have helped?"). Zero means "never
	// marked" (a fresh open, or a position recovered by ledger replay): readers
	// substitute the entry basis rather than treating 0 as a real price.
	BestMark  float64 `json:"best_mark,omitempty"`
	WorstMark float64 `json:"worst_mark,omitempty"`
	// EntrySpreadCostUSD is the half-spread actually crossed to open this position
	// (Σ (ask − mid) × size over every fill/add), and EntryFeeUSD the modelled venue
	// fee on those fills. Both accumulate across adds and are apportioned to the
	// closed trade by the fraction of the position that closes.
	EntrySpreadCostUSD float64 `json:"entry_spread_cost_usd,omitempty"`
	EntryFeeUSD        float64 `json:"entry_fee_usd,omitempty"`
}

// observeMark folds one LIVE mark into the position's running excursion extremes.
// Zero extremes mean "never marked", so both seed from the entry basis before the
// first comparison. Non-positive marks are ignored — the excursion record must never
// be moved by a book that briefly quoted nothing.
func (p *Position) observeMark(mark float64) {
	if mark <= 0 {
		return
	}
	if p.BestMark <= 0 {
		p.BestMark = p.EntryPrice
	}
	if p.WorstMark <= 0 {
		p.WorstMark = p.EntryPrice
	}
	if mark > p.BestMark {
		p.BestMark = mark
	}
	if mark < p.WorstMark {
		p.WorstMark = mark
	}
}

// resetExcursions re-seeds the extremes to the position's current entry basis. Called
// after an add re-averages EntryPrice: MAE/MFE are only meaningful against the basis
// the trade will actually be scored on, so the pre-add excursion is discarded rather
// than silently re-interpreted against a different entry.
func (p *Position) resetExcursions() {
	p.BestMark = p.EntryPrice
	p.WorstMark = p.EntryPrice
}

// ContributorProposal is one persona's individual proposal that fed a (possibly
// consolidated) order — retained so the persona can be scored on its own terms.
type ContributorProposal struct {
	Persona    string  `json:"persona"`
	EntryPrice float64 `json:"entry_price"`
	Size       int     `json:"size"`
	Confidence float64 `json:"confidence,omitempty"`
}

type PriceLevel struct {
	Price  float64 `json:"price"`
	Volume int     `json:"volume"`
}

type OrderBook struct {
	ContractID string       `json:"contract_id"`
	Bids       []PriceLevel `json:"bids"`
	Asks       []PriceLevel `json:"asks"`
	// AsOfMs is the capture time stamped by marketdata. 0 on books written by
	// older builds and on mock books; consumers treat 0 as "age unknown" and
	// fall back to the Redis key TTL as the staleness bound.
	AsOfMs int64 `json:"as_of_ms,omitempty"`
}

type PositionTracker struct {
	mu           sync.RWMutex
	positions    map[string]*Position
	redisClient  *redis.Client
	ledger       *ImmutableLedger
	brokerClient *BrokerClient
	dryMode      bool
	// allowMockBook permits generateMockBook to fabricate an order book when no
	// live book exists in Redis. Default false: live NAV must never be marked
	// against synthetic prices (it caused the dashboard's "Today's P&L" to
	// oscillate as marks flipped between real and fabricated). Enable only for
	// keyless dev/tests via ALLOW_MOCK_ORDERBOOK.
	allowMockBook     bool
	proposalChannel   chan *StrategyProposal
	webCommandChannel chan string
	autoApprove       bool
	ordersExecuted    atomic.Int64 // health metric: orders processed via ExecuteOrder
	proposalsRejected atomic.Int64 // health metric: proposals dropped by the portfolio risk gate
	consensusOrders   atomic.Int64 // health metric: consolidated orders emitted by consensus
	consensusSkipped  atomic.Int64 // health metric: consensus windows skipped (too split)
	noQuoteMarks      atomic.Int64 // health metric: open positions with no book at all at the last snapshot
	oneSidedMarks     atomic.Int64 // health metric: open positions valued off a one-sided book at the last snapshot
	shadowProposals   atomic.Int64 // health metric: proposals suppressed by shadow mode (recorded, never routed)

	// Portfolio-level risk gate. A zero field means "no limit" for that dimension
	// (gate disabled unless SetRiskCaps configures it). Enforced before execution.
	caps RiskCaps

	// Optional adaptive allocator (Phase 4); nil unless ADAPTIVE_ALLOCATION_ENABLED.
	allocator *AdaptiveAllocator

	// Optional buy-and-hold benchmark (e.g. SPY); nil unless BENCHMARK_ENABLED.
	benchmark *BenchmarkTracker

	// Paper-trading account (DRY mode). All mutated under mu.
	startingCapital float64
	cash            float64
	realizedPnL     float64
	peakNAV         float64
	lastNAV         float64 // most recent marked NAV, refreshed by snapshotAccount; read by the drawdown circuit breaker
	maxDrawdownPct  float64 // running worst peak-to-trough drawdown over the run (for reporting; #11)
	closedTrades    []ClosedTrade

	// Per-family guards (post-mortem). Keyed by contract FAMILY (see contractFamily),
	// not exact contract_id, so a strike/expiry ladder (e.g. KXINXY-…-B8300 vs -B5900)
	// shares one loss budget and cooldown instead of getting a fresh one per strike.
	// contractRealizedPnL accumulates realized PnL per family; lastExit records when a
	// family was last closed, for the re-entry cooldown. Both mutated under mu.
	contractRealizedPnL map[string]float64
	lastExit            map[string]time.Time
	// categoryRealizedPnL mirrors contractRealizedPnL at correlation-category grain
	// (contractCategory), so a whole bleeding cluster (e.g. crypto longshots,
	// -$5,054 on 2026-07-03) gets its entries paused once past CategoryLossLimit.
	// Uncategorized ("") contracts are not tracked. Mutated under mu.
	categoryRealizedPnL map[string]float64
	// markCache is the latest LIVE per-contract mark (never a fallback price),
	// timestamped and upserted by snapshotAccount, so the risk gate can fold
	// OPEN-position unrealized PnL into the loss limit without doing Redis I/O
	// while holding the lock, and so realizations can fall back to a recent real
	// price when a book briefly disappears. Entries for closed positions pruned
	// by snapshotAccount.
	markCache map[string]markEntry
	// Mark fallback policy for realizations with no live book (2026-07-03
	// post-mortem: force-exits closed "flat" at entry on missing books, hiding
	// losses): use a cached live mark no older than markStaleMaxAge, else value
	// the position at entry × markHaircut.
	markHaircut     float64
	markStaleMaxAge time.Duration

	// Sim-settlement YES-probability source ("mark" or "entry"); see Config.SettlePrior.
	settlePrior string

	// Minimum per-persona/book sample count before Sharpe/Sortino are reported (below
	// it the scorecard shows "—" rather than a noisy ratio). See Config.MetricsMinSamples.
	metricsMinSamples int

	// Contract expiry window (paper run settlement timing).
	expiryMin time.Duration
	expiryMax time.Duration
	// forceExitWindow: close open positions this long before resolution rather than
	// letting them settle to $0/$1 (post-mortem #2). 0 disables.
	forceExitWindow time.Duration
	// Position stop-loss (2026-07-03 post-mortem): close a position once its unrealized
	// loss reaches stopLossPct% of entry cost OR stopLossUSD dollars, whichever trips
	// first. Evaluated each settlement tick against LIVE marks only — a position with no
	// live book is never stopped out on a stale/fallback price. 0 disables each limb.
	// stopLossMinAbs (2026-07-07 forensics: stops fired at median 24s on sub-$0.25
	// longshots from spread/tick noise): neither limb fires unless the per-contract
	// adverse move also exceeds this many dollars. 0 disables the floor.
	stopLossPct    float64
	stopLossUSD    float64
	stopLossMinAbs float64
	// Position take-profit (2026-07-11, harrier-toolkit idea #1): the mirror of the
	// stop-loss — bank a position once its unrealized GAIN reaches takeProfitPct% of
	// entry cost OR takeProfitUSD dollars, whichever trips first, evaluated on the
	// same settlement-tick LIVE marks. 0 disables each limb (both default off:
	// take-profit changes trading behavior and is operator opt-in).
	takeProfitPct float64
	takeProfitUSD float64
	// Trailing stop (breen-intelligence Phase 3, 2026-08-21). A position that ran at
	// least trailArmPct% above entry is banked once it gives trailRetracePct% of entry
	// back from its best live mark. Both limbs must be > 0 to arm the feature; either
	// at 0 disables it, which is the default — this changes exit behaviour and is an
	// operator opt-in, same discipline as take-profit. The tick-noise floor is shared
	// with the stop (stopLossMinAbs).
	trailArmPct     float64
	trailRetracePct float64
	// settlementHoldPersonas: personas whose edge realizes at RESOLUTION, not on the
	// mark. Their positions skip the percentage stop/take-profit, the trailing stop and
	// the force-exit window, keeping only the dollar backstops. Empty = policy off, and
	// every position is managed exactly as before. See settlementhold.go.
	settlementHoldPersonas map[string]bool
	// settlementHoldTPPrice: the one mark a settlement-hold position IS sold on — an
	// absolute bid at or above which the contract has re-priced to near-certainty and
	// the strategy's edge has already been paid out. 0 = hold to resolution, as before.
	settlementHoldTPPrice float64
	// shadowPersonas keep proposing but never fill: their proposals are recorded to the
	// shadow book and dropped before routing, so they cannot reach the risk gate or
	// influence a consolidated order. Empty = off. See shadow.go.
	shadowPersonas map[string]bool

	// shadowBarProposals is how many ACTIONABLE shadow proposals must be scored before
	// the re-activation bar can be met at all. See shadowscore.go.
	shadowBarProposals int
	// shadowBarMinContracts is the breadth half of the same bar: how many DISTINCT
	// contracts those scored proposals must span before the count means anything.
	// See buildShadowScorecard — a persona re-proposing one contract every tick can
	// otherwise reach any proposal threshold on a single outcome. 0 disables.
	shadowBarMinContracts int
	// familyBlacklist refuses new entries on named contract families outright. Set from
	// FAMILY_BLACKLIST, which Lou may append to at the `intervene` autonomy level — the
	// review's fix #6, giving a validated "this family is a repeat loser" lesson
	// somewhere to land. Keys are lowercased contractFamily() values; empty = off.
	familyBlacklist map[string]bool
	// Daily entry budget (desk review fix #4). entriesToday counts BUYs that actually
	// FILLED today — proposals rejected at the gate or unfilled at the book never spend
	// budget. entriesDay is the calendar day (local) those counts belong to; the counter
	// resets on the first entry of a new day rather than on a timer, so a quiet night
	// needs no goroutine. Both mutated under mu.
	entriesToday int
	entriesDay   string
	// familyEntriesToday counts filled entries per contract FAMILY on entriesDay, for
	// the one-round-trip-per-family-per-day cap (desk review fix #5). Family-keyed, so
	// it also blocks the sequential sibling-strike re-entry that ONE_STRIKE_PER_FAMILY
	// misses once the first strike has closed.
	familyEntriesToday map[string]int
	// perContractLossCloses: when a family's bleed (realized + live-marked unrealized)
	// passes caps.PerContractLossLimit, CLOSE its open positions instead of only
	// blocking re-entries via the risk gate.
	perContractLossCloses bool
	// Entry book-quality gate (2026-07-05 post-mortem: 31,900 dry fills fell back
	// to stale proposal prices and buys filled at wide asks instantly marked
	// -40..-100% against mid). A BUY only fills against a live two-sided book
	// (entryRequireLiveBook — no proposal-price fallback), no older than
	// bookFreshMaxAge, with top-of-book spread within the LOOSER of
	// entryMaxSpreadPct×mid and entryMaxSpreadAbs (absolute tolerance so
	// penny-wide books on cheap contracts still pass). bookFreshMaxAge also
	// bounds what markPriceLive treats as "live" now that the Redis book TTL is
	// longer than a poll cycle. Zero disables each limb; all set from config in
	// main (unit tests construct with the gate off).
	entryRequireLiveBook bool
	entryMaxSpreadPct    float64
	entryMaxSpreadAbs    float64
	bookFreshMaxAge      time.Duration
	// entryEdgeFrictionMult: a BUY must show modelled edge of at least this multiple of
	// the round-trip friction (spread crossed twice + fee on both legs) at the actual
	// fill. 0 = off. Needs StrategyProposal.FairValue; a proposal without one is never
	// blocked (desk review fix #4).
	entryEdgeFrictionMult float64

	// Venue fee model (breen-intelligence Phase 0). feeModel RECORDS the modelled fee
	// on every fill and close; feeCharge additionally DEBITS it from cash. They are
	// separate on purpose: recording alone shows the drag on the scorecard without
	// breaking P&L comparability with earlier runs, and charging is the honest-books
	// step to take before any live mode. feeRate is Kalshi's published coefficient.
	feeModel  bool
	feeCharge bool
	feeRate   float64

	// Calibration table (breen-intelligence Phase 0): stated persona confidence binned
	// against realized win rate, recomputed on each Redis sync and published for the
	// System tab. calibrationEnabled (Phase 1) additionally lets consensus and the
	// mid-band gate reason on the CALIBRATED confidence instead of the stated one;
	// with it off the table is display-only and no decision changes.
	// calibrationCache holds the latest computed table (a CalibrationTable) so the
	// hot paths — riskGate runs on every proposal — do a load instead of a full
	// recompute over the closed-trade history.
	calibrationBins       int
	calibrationEnabled    bool
	calibrationMinSamples int
	calibrationFactorMin  float64
	calibrationFactorMax  float64
	calibrationCache      atomic.Value

	// Optional run-log exporter (CSV/JSONL/summary). Nil when export disabled.
	exporter *RunExporter
}

// ClosedTrade is one settled/exited position, surfaced to the analytics UI.
type ClosedTrade struct {
	ContractID   string   `json:"contract_id"`
	Exchange     string   `json:"exchange"`
	Side         string   `json:"side"` // YES, NO (resolution) or EXIT (sell signal)
	EntryPrice   float64  `json:"entry_price"`
	ExitPrice    float64  `json:"exit_price"`
	Size         int      `json:"size"`
	RealizedPnL  float64  `json:"realized_pnl"`
	ReturnPct    float64  `json:"return_pct"`
	OpenedAtMs   int64    `json:"opened_at_ms"`
	ClosedAtMs   int64    `json:"closed_at_ms"`
	Status       string   `json:"status"`
	Persona      string   `json:"persona,omitempty"`      // order identity (e.g. "consensus")
	Contributors []string `json:"contributors,omitempty"` // personas credited for per-persona stats
	// PersonaScores is the per-persona outcome: each contributing persona scored on
	// its OWN proposal (entry/size) against this trade's actual close price. Drives
	// the strategy scorecard. Empty for legacy/replayed trades (those fall back to
	// crediting each Contributor with the trade-level result).
	PersonaScores []PersonaScore `json:"persona_scores,omitempty"`

	// Excursions (breen-intelligence Phase 0): how far the position ran in each
	// direction while open, as a percentage of the entry basis, from the LIVE marks
	// sampled each settlement tick. MFE ≥ 0 ≥ MAE for any position that was marked at
	// least once. Zero on legacy/replayed trades and on positions that never got a
	// live mark. Binary settlements do NOT fold their payout in — a YES/NO resolution
	// is a payout, not a price the position could have been exited at, and folding it
	// in would collapse every settled trade onto ±100%.
	MaxFavorableExcursionPct float64 `json:"mfe_pct,omitempty"`
	MaxAdverseExcursionPct   float64 `json:"mae_pct,omitempty"`

	// Costs (breen-intelligence Phase 0). SpreadCostUSD is DIAGNOSTIC: the half-spread
	// this trade crossed (entry side always; exit side only when the exit filled off a
	// live book) is ALREADY inside RealizedPnL — buys fill at the ask and mark at the
	// mid — so it decomposes the P&L rather than adding to it. Never subtract it again.
	// FeeUSD is the modelled venue fee, which nothing charged before this: it is a real
	// missing debit, reported here always and charged to cash only under
	// KALSHI_FEE_CHARGE.
	SpreadCostUSD float64 `json:"spread_cost_usd,omitempty"`
	FeeUSD        float64 `json:"fee_usd,omitempty"`
	// FeeCharged records whether FeeUSD was actually netted out of RealizedPnL (and the
	// persona scores) on this trade, rather than only recorded. Stamped per trade rather
	// than inferred from config so a run whose KALSHI_FEE_CHARGE was flipped part-way
	// still aggregates correctly, and so nothing downstream subtracts the fee twice.
	FeeCharged bool `json:"fee_charged,omitempty"`
}

// PersonaScore is one persona's scored outcome on a closed trade.
type PersonaScore struct {
	Persona     string  `json:"persona"`
	RealizedPnL float64 `json:"realized_pnl"`
	ReturnPct   float64 `json:"return_pct"`
	// Cost is the capital this persona deployed on the trade (entry × size), used
	// to compute capital-weighted return on the scorecard.
	Cost float64 `json:"cost"`
	// Confidence is the size-weighted mean conviction this persona stamped on its own
	// proposals for the trade. Carried to the close so the calibration table can bin
	// stated confidence against realized outcome (breen-intelligence Phase 0) — it was
	// previously dropped at close, leaving nothing to calibrate against. Zero on
	// legacy trades and on proposals that carried no confidence.
	Confidence float64 `json:"confidence,omitempty"`
}

type StrategyProposal struct {
	ContractID      string    `json:"contract_id"`
	Action          string    `json:"action"`
	AveragePrice    float64   `json:"average_price"`
	TotalSize       int       `json:"total_size"`
	SlippagePct     float64   `json:"slippage_pct"`
	LimitBreached   bool      `json:"limit_breached"`
	BreachReason    string    `json:"breach_reason,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	EventHash       string    `json:"event_hash"`
	SliceBoundaries []int     `json:"slice_boundaries,omitempty"`

	// Identity & conviction stamped by the emitting Gordon (Phase 1).
	StrategyID string  `json:"strategy_id,omitempty"`
	Persona    string  `json:"persona,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	// FairValue is the emitting strategy's own probability estimate, captured before it
	// anchored its limit price to the book. It is what the friction gate measures edge
	// against at the real fill (desk review fix #4). Zero = not supplied (older
	// producers, or a strategy that has no estimate), which leaves the gate inert for
	// that proposal rather than inventing a number.
	FairValue float64 `json:"fair_value,omitempty"`
	// Set by the consensus engine: the personas that contributed to a consolidated order.
	Contributors []string `json:"contributors,omitempty"`
	// Set by the consensus engine: each contributor's individual proposal, so the
	// resulting position can score personas on their own price/size at close.
	ContributorProposals []ContributorProposal `json:"contributor_proposals,omitempty"`

	// Bearish marks a signal-only proposal from gordon: a persona's conviction that a
	// market falls, emitted when it wanted to SELL a family it doesn't hold. Never an
	// order (the run is YES-only); the consensus engine weighs it against concurrent
	// BUYs on the same event (veto/shrink). See gordon's emitBearishSignal.
	Bearish bool `json:"bearish,omitempty"`

	// SettleResolution is a deterministic settlement outcome ("YES"/"NO") from a
	// strategy that already knows the result (the weather strategy on a cleared
	// bucket). Carried onto the opened Position for settleExpired.
	SettleResolution string `json:"settle_resolution,omitempty"`
}

// contributorProposalsForOpen returns the per-persona proposals to score an order
// by. A consensus order carries the individual persona proposals; a direct
// (non-consensus) order is scored as its single persona at the actual fill.
func contributorProposalsForOpen(p *StrategyProposal, fillPrice float64, fillSize int) []ContributorProposal {
	if len(p.ContributorProposals) > 0 {
		return p.ContributorProposals
	}
	return []ContributorProposal{{
		Persona:    personaOrDefault(p.Persona),
		EntryPrice: fillPrice,
		Size:       fillSize,
		Confidence: p.Confidence,
	}}
}

// proposalContributors returns the personas to credit for a proposal: the explicit
// consensus contributor list, else the single emitting persona.
func proposalContributors(p *StrategyProposal) []string {
	if len(p.Contributors) > 0 {
		return p.Contributors
	}
	if p.Persona != "" {
		return []string{p.Persona}
	}
	return nil
}

func NewPositionTracker(redisAddr string, ledger *ImmutableLedger, brokerClient *BrokerClient, dryMode bool, startingCapital float64, expiryMin, expiryMax time.Duration) *PositionTracker {
	var rClient *redis.Client
	if redisAddr != "" {
		rClient = redis.NewClient(&redis.Options{
			Addr:        redisAddr,
			DialTimeout: 2 * time.Second,
			ReadTimeout: 1 * time.Second,
		})
	}

	return &PositionTracker{
		positions:           make(map[string]*Position),
		redisClient:         rClient,
		ledger:              ledger,
		brokerClient:        brokerClient,
		dryMode:             dryMode,
		proposalChannel:     make(chan *StrategyProposal, 100),
		webCommandChannel:   make(chan string, 100),
		startingCapital:     startingCapital,
		cash:                startingCapital,
		peakNAV:             startingCapital,
		lastNAV:             startingCapital,
		closedTrades:        make([]ClosedTrade, 0),
		contractRealizedPnL: map[string]float64{},
		lastExit:            map[string]time.Time{},
		categoryRealizedPnL: map[string]float64{},
		markCache:           map[string]markEntry{},
		// Honest-mark fallback defaults (main.go overrides from config): realizations
		// with no live/cached quote book entry × 0.5, never a flat "at entry" fill.
		markHaircut:       0.5,
		markStaleMaxAge:   30 * time.Minute,
		settlePrior:       "mark",
		metricsMinSamples: 20,
		bookFreshMaxAge:   90 * time.Second,
		feeModel:          true,
		feeCharge:         false,
		feeRate:           defaultKalshiFeeRate,
		calibrationBins:   defaultCalibrationBins,
		// Calibration is display-only until an operator turns it on: it can only
		// ever shrink or grow conviction that the gates are tuned against, so it
		// ships inert (see calibratedConfidence).
		calibrationEnabled:    false,
		calibrationMinSamples: 20,
		calibrationFactorMin:  defaultCalibrationFactorMin,
		calibrationFactorMax:  defaultCalibrationFactorMax,
		expiryMin:             expiryMin,
		expiryMax:             expiryMax,
	}
}

// RiskCaps are portfolio-level limits enforced by the executive before a proposal
// executes. A zero value disables that particular check.
type RiskCaps struct {
	MaxContractExposure    float64 // max notional held in any single contract
	MaxContractExposurePct float64 // per-contract notional cap as a fraction of NAV; binds with MaxContractExposure (tighter wins)
	MaxCategoryExposure    float64 // max COMBINED notional across a correlation category (0 = off)
	MaxCategoryExposurePct float64 // category cap as a fraction of NAV; binds with MaxCategoryExposure (tighter wins)
	MaxTotalDeployed       float64 // max total capital deployed across all positions
	MaxOpenPositions       int     // max number of distinct open contracts
	RejectOnBreach         bool    // reject proposals Gordon already flagged LimitBreached

	// Post-mortem guards (0/empty disables each).
	PerContractLossLimit float64       // reject new entries once a contract's cumulative realized PnL ≤ -limit
	ExitCooldown         time.Duration // reject re-entry within this window after closing the contract
	MaxDrawdownHaltPct   float64       // reject ALL new entries once NAV is this far below starting capital
	SizingEnabled        bool          // shrink BUY fills as drawdown approaches the halt (breen/sizing.go)
	DrawdownEaseStart    float64       // drawdown % where the ramp starts
	DrawdownEaseFloor    float64       // scalar at the halt threshold
	CategoryLossLimit    float64       // reject new entries in a correlation category once its cumulative PnL ≤ -limit
	OneStrikePerFamily   bool          // reject a BUY on a family strike while a DIFFERENT strike of that family is already open

	// Mid-band conviction gate (2026-07-03 post-mortem: 0.35–0.65 entries lost
	// -$9,036 while everything else made +$2,749). BUYs priced inside
	// [MidBandLow, MidBandHigh] need Confidence ≥ MidBandMinConfidence.
	// MidBandMinConfidence 0 disables the gate.
	MidBandLow           float64
	MidBandHigh          float64
	MidBandMinConfidence float64

	// Friction gates (2026-08-25 desk review, fix #4). Friction is charged as a
	// PERCENTAGE of premium — ~2.5c of round-trip fee plus 2–4c of round-trip spread on
	// a 23c contract — so a tiny clip pays the same 15–20% of premium as a large one
	// while having no room to overcome it. The review measured 7.7% friction on the 204
	// clips under $25, against a median clip of $17.70.
	//
	// MinClipNotional rejects entries too small to be worth the round trip, and
	// MaxEntriesPerDay caps how many round trips the book takes at all — 341 trades in
	// 16 days is the churn that pays the vig over and over. Both 0 = off.
	MinClipNotional  float64
	MaxEntriesPerDay int
	// MaxFamilyEntriesPerDay caps round trips STARTED per contract family per calendar
	// day (desk review fix #5). The review's shape is 1: one round trip per family per
	// day. Deliberately no "unless the model inputs changed" escape — the only signal
	// breen has for that is StrategyProposal.EventHash, which is the hash of the
	// triggering ingest payload and therefore changes on essentially every poll, so an
	// escape keyed on it would leave the gate inert. 0 = off.
	MaxFamilyEntriesPerDay int
}

// SetRiskCaps configures the portfolio risk gate (called from main after config load).
func (pt *PositionTracker) SetRiskCaps(c RiskCaps) {
	pt.mu.Lock()
	pt.caps = c
	pt.mu.Unlock()
}

// contractFamily normalizes a contract_id to the correlated group its per-contract
// guards should share. Kalshi tickers are SERIES-EVENT-STRIKE (e.g.
// KXINXY-26DEC31H1600-B8300); dropping the trailing strike segment groups every strike
// of the same event so a ladder can't dodge the loss limit/cooldown by hopping strikes.
// Tickers with fewer than three segments (e.g. JPMCEOCHANGE-27, or POLY-<unique-token>,
// where the tail is the market's identity rather than a strike) are left whole so
// unrelated markets aren't collapsed together. This is the strike-ladder fix — broader
// cross-series correlation (e.g. all Fed-rate series) is still task #5.
func contractFamily(contractID string) string {
	parts := strings.Split(contractID, "-")
	if len(parts) >= 3 {
		return strings.Join(parts[:len(parts)-1], "-")
	}
	return contractID
}

// categoryKeywords maps an uppercased substring in a contract_id to a broad
// correlation category, first match wins. Contracts in one category move together on a
// single macro event — the post-mortem simultaneously held KXRATECUT / KXEFFR /
// KXLARGECUT / KXFEDDECISION / KXZERORATE, all hit by one rate move — so riskGate caps
// their COMBINED exposure. Heuristic and easily extended; a config-driven map could
// replace it later. This is the cross-series half of #5 (contractFamily covers the
// same-event strike ladder).
var categoryKeywords = []struct{ kw, cat string }{
	{"RATECUT", "macro"}, {"ZERORATE", "macro"}, {"LARGECUT", "macro"}, {"FEDDECISION", "macro"},
	{"FOMC", "macro"}, {"EFFR", "macro"}, {"CBDECISION", "macro"}, {"RATEHIKE", "macro"},
	{"RATE", "macro"}, {"CPI", "macro"}, {"PCE", "macro"}, {"INFLATION", "macro"}, {"NOTE10M", "macro"},
	{"NASDAQ", "equity-index"}, {"SP500", "equity-index"}, {"INXY", "equity-index"},
	{"INX", "equity-index"}, {"DJIA", "equity-index"}, {"RUSSELL", "equity-index"},
	{"HSI", "equity-index"}, {"NIKKEI", "equity-index"},
	// Crypto/exotic cluster extended after the 2026-07-03 run (-$5,054 across XRP /
	// DASH / OpenSea-FDV / BTC / ETH longshots). DASH can also match DoorDash-series
	// tickers — acceptable: grouping them errs conservative. LINK and ADA are
	// deliberately excluded (substring collisions: LINKEDIN, CANADA, NEVADA).
	{"BTC", "crypto"}, {"ETH", "crypto"}, {"SOL", "crypto"}, {"DOGE", "crypto"},
	{"XRP", "crypto"}, {"DASH", "crypto"}, {"OPENSEA", "crypto"}, {"FDV", "crypto"},
	{"NFT", "crypto"}, {"AVAX", "crypto"},
}

// contractCategory returns the broad correlation bucket for a contract, or "" when it
// belongs to no known cluster. Uncategorized contracts are deliberately NOT grouped, so
// unrelated single-name markets (e.g. TESLACEOCHANGE, JPMCEOCHANGE) don't share a budget.
func contractCategory(contractID string) string {
	u := strings.ToUpper(contractID)
	for _, e := range categoryKeywords {
		if strings.Contains(u, e.kw) {
			return e.cat
		}
	}
	return ""
}

// midBandConfidence is the conviction the mid-band gate judges: the strongest
// contributor confidence when the proposal is a consensus consolidation, else the
// proposal's own. Consensus carries Confidence = mean(weight × conf), so the persona
// weight shaved every single-contributor order below its raw conviction (0.97 × 0.72
// read as 0.70 at the gate) — and with the entry floor/ceiling confining all entries
// to the band, that closed the funnel entirely (2026-07-08 flatline).
//
// cal (Phase 1) rescales each contributor's stated confidence by that persona's own
// record at that confidence level before the max is taken, so the gate judges what a
// persona's conviction has been WORTH rather than what it claimed. nil = stated, the
// pre-Phase-1 behavior. Calibrating each contributor and then taking the max (rather
// than the reverse) means the loudest persona can't carry the gate on a claim its own
// record doesn't support.
func midBandConfidence(p *StrategyProposal, cal confidenceCalibrator) float64 {
	conf := cal.apply(personaOrDefault(p.Persona), p.Confidence)
	for _, cp := range p.ContributorProposals {
		if c := cal.apply(personaOrDefault(cp.Persona), cp.Confidence); c > conf {
			conf = c
		}
	}
	return conf
}

// riskGate decides whether a proposal may proceed to execution, considering CURRENT
// portfolio state (positions, deployed capital). SELL/exit actions always pass since
// they reduce exposure. Returns (allowed, reason). Centralizes risk here per the
// design decision — Gordons stay stateless opinion-generators.
func (pt *PositionTracker) riskGate(proposal *StrategyProposal) (bool, string) {
	action := strings.ToUpper(proposal.Action)
	if action == "SELL" {
		// An exit must match an OPEN position exactly. Family-level matching let
		// sibling-strike sells through (held KXGDP-...-T2.0 ⇒ 56 phantom sells of
		// T0.0/T0.5/T1.0 on 2026-07-12), each burning an execute cycle to no-op at
		// fill. Bearish signal-only proposals are opinions, never orders, so they
		// are rejected here too when consensus isn't running to absorb them.
		if proposal.Bearish {
			return false, "bearish signal is opinion-only, not an executable order"
		}
		pt.mu.RLock()
		_, heldExact := pt.positions[proposal.ContractID]
		pt.mu.RUnlock()
		if !heldExact {
			return false, "SELL of unheld contract (sibling strikes of a held family are not exits)"
		}
		return true, "" // exits only reduce exposure
	}

	// Persona kill-switch (#6): a benched persona's direct proposals are rejected
	// outright (consensus filters its contributions upstream).
	if pt.allocator != nil && pt.allocator.IsBenched(personaOrDefault(proposal.Persona)) {
		return false, fmt.Sprintf("persona %s benched by kill-switch", personaOrDefault(proposal.Persona))
	}

	family := contractFamily(proposal.ContractID)

	pt.mu.RLock()
	caps := pt.caps
	_, held := pt.positions[proposal.ContractID]
	category := contractCategory(proposal.ContractID)
	// Aggregate the family's open notional and unrealized PnL (marked off the cache so
	// the gate stays I/O-free), so a strike ladder is treated as one exposure/loss unit.
	// Also sum the correlation category's combined notional for the cross-market cap (#5),
	// and the total open notional for the deployed cap — "deployed" is capital tied up
	// in OPEN positions (entry × size), not startingCapital − cash, which conflated
	// realized losses with exposure and throttled entries harder the more a run lost.
	var familyNotional, familyUnrealized, categoryNotional, categoryUnrealized, deployed float64
	var familySiblingHeld bool // a DIFFERENT strike of this family is already open
	for id, p := range pt.positions {
		notional := p.EntryPrice * float64(p.Size)
		deployed += notional
		mark := p.EntryPrice // unknown mark ⇒ assume flat (0 unrealized)
		if m, ok := pt.markCache[id]; ok {
			mark = m.Price
		}
		if category != "" && contractCategory(id) == category {
			categoryNotional += notional
			categoryUnrealized += (mark - p.EntryPrice) * float64(p.Size)
		}
		if contractFamily(id) != family {
			continue
		}
		if id != proposal.ContractID {
			familySiblingHeld = true
		}
		familyNotional += notional
		familyUnrealized += (mark - p.EntryPrice) * float64(p.Size)
	}
	openCount := len(pt.positions)
	nav := pt.lastNAV
	if nav <= 0 {
		nav = pt.startingCapital
	}
	starting := pt.startingCapital
	familyPnL := pt.contractRealizedPnL[family]
	categoryPnL := pt.categoryRealizedPnL[category]
	lastExit, exited := pt.lastExit[family]
	entriesToday := pt.entriesTodayLocked()
	familyEntriesToday := pt.familyEntriesTodayLocked(family)
	pt.mu.RUnlock()

	if caps.RejectOnBreach && proposal.LimitBreached {
		return false, "gordon limit-breached and RejectOnBreach is set"
	}

	if pt.isBlacklistedFamily(family) {
		return false, fmt.Sprintf("family %s is blacklisted (FAMILY_BLACKLIST) — a validated repeat loser", family)
	}

	// Friction gates (desk review fix #4). Both are about the vig rather than the view:
	// a clip too small to carry its own round trip is a losing trade whatever the
	// signal says, and a book that takes 20 round trips a day pays that toll 20 times.
	if caps.MinClipNotional > 0 {
		if notional := proposal.AveragePrice * float64(proposal.TotalSize); notional < caps.MinClipNotional {
			return false, fmt.Sprintf("clip $%.2f below the $%.0f minimum — round-trip friction (~3.5%% of premium) would eat it",
				notional, caps.MinClipNotional)
		}
	}
	if caps.MaxEntriesPerDay > 0 && entriesToday >= caps.MaxEntriesPerDay {
		return false, fmt.Sprintf("daily entry budget spent (%d/%d filled today) — the book pays friction per round trip, not per day",
			entriesToday, caps.MaxEntriesPerDay)
	}
	// Churn cap (desk review fix #5). 89 contracts were traded more than once over the
	// period for 165 extra round trips and -$490 concentrated in the repeats;
	// KXHIGHLAX-26AUG22-T83 was entered eight times in a single day. The 30-minute
	// EXIT_COOLDOWN meters that, it doesn't stop it. Keyed by FAMILY, so it also closes
	// the sequential sibling-strike hole ONE_STRIKE_PER_FAMILY leaves open once the
	// first strike has closed (the GDP -$281 pair).
	if caps.MaxFamilyEntriesPerDay > 0 && familyEntriesToday >= caps.MaxFamilyEntriesPerDay {
		return false, fmt.Sprintf("family %s already took %d/%d round trip(s) today — re-entry needs a new day, not a new tick",
			family, familyEntriesToday, caps.MaxFamilyEntriesPerDay)
	}

	// Mid-band conviction gate (#3): coin-flip-priced entries were the losing trade
	// type (0.35–0.65 lost -$9,036 on 2026-07-03 while everything else was +$2,749).
	// Judged on the strongest contributor confidence, not the consensus number —
	// calibrated against each persona's own record when CALIBRATION_ENABLED is on.
	if caps.MidBandMinConfidence > 0 &&
		proposal.AveragePrice >= caps.MidBandLow && proposal.AveragePrice <= caps.MidBandHigh {
		if conf := midBandConfidence(proposal, pt.calibrator()); conf < caps.MidBandMinConfidence {
			return false, fmt.Sprintf("mid-band price %.2f∈[%.2f,%.2f] needs confidence ≥ %.2f (got %.2f)",
				proposal.AveragePrice, caps.MidBandLow, caps.MidBandHigh, caps.MidBandMinConfidence, conf)
		}
	}

	// Portfolio drawdown circuit breaker: once NAV has fallen past the halt threshold
	// below starting capital, allow only exits until the book recovers (#10).
	if caps.MaxDrawdownHaltPct > 0 && starting > 0 {
		ddPct := (starting - nav) / starting * 100.0
		if ddPct >= caps.MaxDrawdownHaltPct {
			return false, fmt.Sprintf("drawdown %.1f%% ≥ halt %.1f%% — new entries paused", ddPct, caps.MaxDrawdownHaltPct)
		}
	}

	// Per-family cumulative-loss limit: stop adding to a family once its realized PLUS
	// open unrealized PnL has bled past the limit this run (#4). Counting unrealized
	// closes the overshoot where a big underwater position hadn't booked its loss yet.
	if caps.PerContractLossLimit > 0 {
		if bleed := familyPnL + familyUnrealized; bleed <= -caps.PerContractLossLimit {
			return false, fmt.Sprintf("family %s PnL $%.0f (realized $%.0f + unreal $%.0f) past loss limit -$%.0f",
				family, bleed, familyPnL, familyUnrealized, caps.PerContractLossLimit)
		}
	}

	// Per-category cumulative-loss limit (#5): once a correlation cluster (realized
	// plus open cache-marked unrealized) has bled past the limit this run, pause all
	// new entries in it — the per-family limit missed serial small-cost catastrophes
	// spread across different crypto longshots.
	if caps.CategoryLossLimit > 0 && category != "" {
		if bleed := categoryPnL + categoryUnrealized; bleed <= -caps.CategoryLossLimit {
			return false, fmt.Sprintf("category %s PnL $%.0f (realized $%.0f + unreal $%.0f) past loss limit -$%.0f — entries paused",
				category, bleed, categoryPnL, categoryUnrealized, caps.CategoryLossLimit)
		}
	}

	// Re-entry cooldown: suppress churn by waiting after ANY exit in a family —
	// full or partial, held or flat — before buying any of its strikes (#6). The
	// old still-held exemption let a sell-partial/buy-back loop run inside one
	// position: on 2026-07-14 gemma-neutral sold POLY-25312457… five times in 70
	// minutes on book-imbalance while sentiment re-bought it minutes later, paying
	// the ~2% spread every cycle. An exit is a conviction the market is wrong to
	// hold; re-arming it minutes later on an opposing signal is spread bleed.
	if caps.ExitCooldown > 0 && exited {
		if since := time.Since(lastExit); since < caps.ExitCooldown {
			return false, fmt.Sprintf("cooldown: %v since %s exit < %v", since.Round(time.Second), family, caps.ExitCooldown)
		}
	}

	// One strike per family (#3, 2026-07-14 forensics): with the run YES-only, a second
	// strike of an event already held is the same directional bet — KXGDP-26JUL30 T2.0 +
	// T2.5 stopped out 25m apart on one GDP print. The family notional cap bounds their
	// combined size but not the correlated stacking; collapse the family to one active
	// strike so conviction concentrates. Adds to the SAME strike (held) still pass here.
	if caps.OneStrikePerFamily && familySiblingHeld {
		return false, fmt.Sprintf("family %s already holds a different strike (one strike per family)", family)
	}

	orderNotional := proposal.AveragePrice * float64(proposal.TotalSize)

	// Per-family notional cap: the tighter of the absolute cap and the NAV-percentage
	// cap (#1), summed across the whole strike ladder so laddering can't multiply
	// exposure. A single oversized contract was 48% of the post-mortem's losses.
	contractCap := caps.MaxContractExposure
	if caps.MaxContractExposurePct > 0 {
		pctCap := caps.MaxContractExposurePct * nav
		if contractCap <= 0 || pctCap < contractCap {
			contractCap = pctCap
		}
	}
	if contractCap > 0 && familyNotional+orderNotional > contractCap {
		return false, fmt.Sprintf("family %s exposure $%.0f would exceed cap $%.0f", family, familyNotional+orderNotional, contractCap)
	}

	// Per-category notional cap: bound the COMBINED exposure of correlated markets so one
	// macro move can't hit an oversized cluster all at once (#5). Tighter of abs and NAV-%.
	if category != "" {
		catCap := caps.MaxCategoryExposure
		if caps.MaxCategoryExposurePct > 0 {
			pctCap := caps.MaxCategoryExposurePct * nav
			if catCap <= 0 || pctCap < catCap {
				catCap = pctCap
			}
		}
		if catCap > 0 && categoryNotional+orderNotional > catCap {
			return false, fmt.Sprintf("category %s exposure $%.0f would exceed cap $%.0f", category, categoryNotional+orderNotional, catCap)
		}
	}

	if caps.MaxTotalDeployed > 0 && deployed+orderNotional > caps.MaxTotalDeployed {
		return false, fmt.Sprintf("total deployed $%.0f would exceed cap $%.0f", deployed+orderNotional, caps.MaxTotalDeployed)
	}
	if caps.MaxOpenPositions > 0 && !held && openCount >= caps.MaxOpenPositions {
		return false, fmt.Sprintf("open positions %d at cap %d", openCount, caps.MaxOpenPositions)
	}
	return true, ""
}

// entryBookBlock decides whether a BUY may fill against the given book. It returns
// a non-empty reason to refuse when: no two-sided live book exists (and requireLive
// is set), the book's as_of_ms stamp is older than maxAge, or the top-of-book
// spread exceeds BOTH maxSpreadPct×mid and maxSpreadAbs — i.e. the effective cap is
// the looser of the two, so penny-wide books on cheap contracts pass despite a
// large relative spread. Zero disables each limb. Unstamped books (mocks, older
// marketdata builds) skip the age check; the Redis key TTL still bounds them.
func entryBookBlock(ob *OrderBook, requireLive bool, maxSpreadPct, maxSpreadAbs float64, maxAge time.Duration, now time.Time) string {
	if ob == nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		if requireLive {
			return "no live two-sided order book — entry refused (fallback fills disabled)"
		}
		return ""
	}
	if maxAge > 0 && ob.AsOfMs > 0 {
		if age := now.Sub(time.UnixMilli(ob.AsOfMs)); age > maxAge {
			return fmt.Sprintf("order book stale (%v old > %v) — entry refused", age.Round(time.Second), maxAge)
		}
	}
	bid, ask := ob.Bids[0].Price, ob.Asks[0].Price
	spread := ask - bid
	mid := (bid + ask) / 2
	// 1e-9 epsilon: book prices are decimal cents parsed into binary floats, so an
	// exactly-at-cap spread (e.g. 0.11−0.09 vs $0.02) can land a hair above it.
	const eps = 1e-9
	if maxSpreadPct > 0 && mid > 0 && spread > maxSpreadPct*mid+eps && (maxSpreadAbs <= 0 || spread > maxSpreadAbs+eps) {
		return fmt.Sprintf("top-of-book spread %.3f is %.0f%% of mid %.3f (cap %.0f%%) — a fill at ask %.3f would mark straight down to mid",
			spread, spread/mid*100, mid, maxSpreadPct*100, ask)
	}
	return ""
}

// markHotTicker publishes a proposal's ticker to gekko:hot_tickers (score = unix ms),
// the demand-driven watch set marketdata refreshes every poll cycle. Marking on
// arrival — before consensus and the risk gate — means the book is being polled by
// the time a winning BUY reaches the entry book gate, instead of carrying whatever
// age the universe rotation left it at. Fire-and-forget: a miss only means this
// ticker waits for the rotation like before.
func (pt *PositionTracker) markHotTicker(contractID string) {
	if pt.redisClient == nil || contractID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = pt.redisClient.ZAdd(ctx, "gekko:hot_tickers", redis.Z{
		Score:  float64(time.Now().UnixMilli()),
		Member: contractID,
	}).Err()
}

// ProcessStrategyProposal applies the portfolio risk gate, then queues the proposal
// for review/auto-execution.
func (pt *PositionTracker) ProcessStrategyProposal(proposal *StrategyProposal) {
	if ok, reason := pt.riskGate(proposal); !ok {
		pt.proposalsRejected.Add(1)
		log.Printf("[RISK] Rejected %s %s (persona=%s): %s", proposal.Action, proposal.ContractID, proposal.Persona, reason)
		_, _ = pt.ledger.AppendTransaction(proposal.ContractID, proposal.Action, proposal.TotalSize, proposal.AveragePrice, "REJECTED", 0, reason)
		return
	}

	pt.mu.RLock()
	auto := pt.autoApprove
	pt.mu.RUnlock()

	if auto {
		// Auto-approved, bypass CLI queue and execute directly in background
		go pt.ExecuteOrder(proposal)
	} else {
		pt.proposalChannel <- proposal
	}
}

// ExecuteOrder runs the execution logic (Dry Mode matching Redis or Live Mode routing to Broker-Worker).
func (pt *PositionTracker) ExecuteOrder(proposal *StrategyProposal) {
	ctx := context.Background()

	// Guard: limit actions
	action := strings.ToUpper(proposal.Action)
	if action != "BUY" && action != "SELL" {
		log.Printf("[ERROR] Invalid strategy action: %s", action)
		return
	}
	pt.ordersExecuted.Add(1)
	log.Printf("[EXECUTE] %s %s size=%d @ %.4f | persona=%s conf=%.2f%s",
		action, proposal.ContractID, proposal.TotalSize, proposal.AveragePrice,
		personaOrDefault(proposal.Persona), proposal.Confidence,
		func() string {
			if proposal.BreachReason != "" {
				return " | " + proposal.BreachReason
			}
			return ""
		}())

	if pt.dryMode {
		// --- deterministic Dry Mode execution ---
		log.Printf("[DRY RUN] Executing dry order matching top-of-book for: %s", proposal.ContractID)

		// 1. Write DRY_OPEN ledger receipt
		blockOpen, err := pt.ledger.AppendTransaction(proposal.ContractID, action, proposal.TotalSize, proposal.AveragePrice, "DRY_OPEN", 0, "")
		if err != nil {
			log.Printf("[ERROR] Failed to write DRY_OPEN receipt: %v", err)
			return
		}

		// 2. Fetch price from local Redis order book to match top-of-book liquidity.
		// BUYs must pass the entry book-quality gate; SELLs keep the proposal-price
		// fallback since exits only reduce exposure and refusing them would trap
		// positions whenever a book blinks out.
		execPrice := proposal.AveragePrice
		ob, obErr := pt.fetchRedisOrderBook(ctx, proposal.ContractID)
		if obErr != nil {
			ob = nil
		}
		if action == "BUY" {
			if reason := entryBookBlock(ob, pt.entryRequireLiveBook, pt.entryMaxSpreadPct, pt.entryMaxSpreadAbs, pt.bookFreshMaxAge, time.Now()); reason != "" {
				pt.proposalsRejected.Add(1)
				log.Printf("[RISK] Rejected BUY %s at fill: %s", proposal.ContractID, reason)
				_, _ = pt.ledger.AppendTransaction(proposal.ContractID, action, proposal.TotalSize, execPrice, "REJECTED", 0, reason)
				return
			}
			// Edge-vs-friction re-check (desk review fix #4), at the book we are about
			// to fill on rather than the one gordon saw seconds ago — and unlike
			// gordon's requiredEdge, this one prices the venue fee too.
			if reason := entryEdgeBlock(proposal.ContractID, proposal.FairValue, ob,
				pt.entryEdgeFrictionMult, pt.feeRate, pt.feeModel); reason != "" {
				pt.proposalsRejected.Add(1)
				log.Printf("[RISK] Rejected BUY %s at fill: %s", proposal.ContractID, reason)
				_, _ = pt.ledger.AppendTransaction(proposal.ContractID, action, proposal.TotalSize, execPrice, "REJECTED", 0, reason)
				return
			}
			if ob != nil && len(ob.Asks) > 0 {
				execPrice = ob.Asks[0].Price
			} else {
				log.Printf("[WARNING] No live book for %s — dry BUY filling at proposal price %.2f (entry gate disabled).", proposal.ContractID, execPrice)
			}
		} else if ob != nil && len(ob.Bids) > 0 {
			execPrice = ob.Bids[0].Price
		} else {
			log.Printf("[WARNING] No live book for %s — dry exit filling at proposal price %.2f.", proposal.ContractID, execPrice)
		}

		// Re-check the mid-band conviction gate at the RESOLVED fill price: riskGate
		// only saw the proposal's average price, and a top-of-book fill can slide a
		// near-band proposal into the coin-flip zone (#3).
		pt.mu.RLock()
		caps := pt.caps
		pt.mu.RUnlock()
		if action == "BUY" && caps.MidBandMinConfidence > 0 &&
			execPrice >= caps.MidBandLow && execPrice <= caps.MidBandHigh {
			if conf := midBandConfidence(proposal, pt.calibrator()); conf < caps.MidBandMinConfidence {
				pt.proposalsRejected.Add(1)
				log.Printf("[RISK] Rejected %s %s at fill: mid-band price %.2f∈[%.2f,%.2f] needs confidence ≥ %.2f (got %.2f)",
					action, proposal.ContractID, execPrice, caps.MidBandLow, caps.MidBandHigh, caps.MidBandMinConfidence, conf)
				_, _ = pt.ledger.AppendTransaction(proposal.ContractID, action, proposal.TotalSize, execPrice, "REJECTED", 0, "mid-band at fill")
				return
			}
		}

		// 3. Update Position Map + paper-trading account
		var dryYield float64
		var dryClose bool
		filledSize := proposal.TotalSize
		// Cost accounting (Phase 0): the half-spread crossed on this fill, taken from
		// the same book the fill priced off. Already inside the P&L — a BUY at the ask
		// marks straight down to the mid — so this only gives it a name.
		halfSpread := halfSpreadPerContract(ob)
		pt.mu.Lock()
		if action == "BUY" {
			// Graduated de-risking: shrink the fill as drawdown nears the halt. Applied at the
			// fill, so every cap riskGate cleared at full size stays cleared (it only shrinks).
			if c := pt.caps; c.SizingEnabled && pt.startingCapital > 0 && pt.lastNAV > 0 {
				ddPct := (pt.startingCapital - pt.lastNAV) / pt.startingCapital * 100.0
				if s := drawdownScalar(ddPct, c.DrawdownEaseStart, c.MaxDrawdownHaltPct, c.DrawdownEaseFloor); s < 1.0 {
					scaled := int(float64(filledSize) * s)
					if scaled < 1 {
						scaled = 1
					}
					log.Printf("[SIZE] %s drawdown %.1f%% → size ×%.2f (%d → %d)", proposal.ContractID, ddPct, s, filledSize, scaled)
					filledSize = scaled
				}
			}
			// Buying-power check: cap the order to available cash.
			cost := execPrice * float64(filledSize)
			if cost > pt.cash {
				affordable := 0
				if execPrice > 0 {
					affordable = int(pt.cash / execPrice)
				}
				if affordable <= 0 {
					pt.mu.Unlock()
					log.Printf("[DRY RUN] Insufficient cash ($%.2f) to open %s @ %.4f — skipping.", pt.cash, proposal.ContractID, execPrice)
					return
				}
				filledSize = affordable
				cost = execPrice * float64(filledSize)
			}
			pt.cash -= cost

			entrySpread := halfSpread * float64(filledSize)
			entryFee := kalshiFee(proposal.ContractID, execPrice, filledSize, pt.feeRate, pt.feeModel)
			if pt.feeCharge {
				pt.cash -= entryFee
			}

			pos, exists := pt.positions[proposal.ContractID]
			if exists {
				newSize := pos.Size + filledSize
				pos.EntryPrice = ((pos.EntryPrice * float64(pos.Size)) + (execPrice * float64(filledSize))) / float64(newSize)
				pos.Size = newSize
				// Accumulate this order's per-persona proposals for close-time scoring.
				pos.ContributorProposals = append(pos.ContributorProposals, contributorProposalsForOpen(proposal, execPrice, filledSize)...)
				pos.EntrySpreadCostUSD += entrySpread
				pos.EntryFeeUSD += entryFee
				// The add re-averaged the basis, so the excursion record restarts from it.
				pos.resetExcursions()
			} else {
				pt.positions[proposal.ContractID] = &Position{
					ContractID:           proposal.ContractID,
					EntryPrice:           execPrice,
					Size:                 filledSize,
					Timestamp:            time.Now(),
					Expiry:               pt.resolveExpiry(proposal.ContractID),
					Persona:              proposal.Persona,
					StrategyID:           proposal.StrategyID,
					Contributors:         proposalContributors(proposal),
					ContributorProposals: contributorProposalsForOpen(proposal, execPrice, filledSize),
					SettleResolution:     proposal.SettleResolution,
					BestMark:             execPrice,
					WorstMark:            execPrice,
					EntrySpreadCostUSD:   entrySpread,
					EntryFeeUSD:          entryFee,
				}
			}
			// Only a real fill spends the day's entry budget (fixes #4/#5).
			pt.noteEntryFilledLocked(proposal.ContractID)
			log.Printf("[DRY RUN] Position OPEN/ADD. Contract=%s | EntryPrice=%.4f | Size=%d | Cash=$%.2f", proposal.ContractID, execPrice, pt.positions[proposal.ContractID].Size, pt.cash)
		} else {
			pos, exists := pt.positions[proposal.ContractID]
			if exists {
				sellSize := proposal.TotalSize
				if sellSize > pos.Size {
					sellSize = pos.Size
				}
				dryYield = (execPrice - pos.EntryPrice) * float64(sellSize)
				// Credit proceeds and realize PnL.
				pt.cash += execPrice * float64(sellSize)
				pt.realizedPnL += dryYield
				// The only exit that crosses a spread: this one fills at the bid, while
				// stop/take-profit/force-exit closes are marked at the mid and
				// settlements pay a binary. 2026-07-07 forensics had DRY_CLOSED
				// averaging −$4/trade of pure spread — now it is recorded as such.
				pt.recordClosedTradeLocked(pos, execPrice, sellSize, "EXIT", "DRY_CLOSED", halfSpread)
				log.Printf("[DRY RUN] Position CLOSE via Exit Signal. Contract=%s | Entry=%.4f | Exit=%.4f | Size=%d | P/L Yield=$%.2f | Cash=$%.2f",
					proposal.ContractID, pos.EntryPrice, execPrice, sellSize, dryYield, pt.cash)
				pos.Size -= sellSize
				if pos.Size <= 0 {
					delete(pt.positions, proposal.ContractID)
				}
				dryClose = true
			} else {
				log.Printf("[DRY RUN] Exit signal received for %s but no active position held.", proposal.ContractID)
			}
		}
		pt.mu.Unlock() // release before any I/O or syncToRedis to avoid deadlock

		if dryClose {
			_, _ = pt.ledger.AppendTransaction(proposal.ContractID, action, proposal.TotalSize, execPrice, "DRY_CLOSED", dryYield, "")
			pt.syncToRedis(ctx)
			return
		}

		// 4. Write DRY_FILLED ledger receipt
		_, err = pt.ledger.AppendTransaction(proposal.ContractID, action, filledSize, execPrice, "DRY_FILLED", 0, blockOpen.Hash)
		if err != nil {
			log.Printf("[ERROR] Failed to write DRY_FILLED receipt: %v", err)
		}

	} else {
		// --- Live Mode routing via broker-worker Unix socket ---
		log.Printf("[LIVE] Routing live order intent to broker-worker for: %s", proposal.ContractID)

		// 1. Write LIVE_OPEN ledger receipt
		_, err := pt.ledger.AppendTransaction(proposal.ContractID, action, proposal.TotalSize, proposal.AveragePrice, "LIVE_OPEN", 0, "")
		if err != nil {
			log.Printf("[ERROR] Failed to write LIVE_OPEN receipt: %v", err)
			return
		}

		// 2. Request execution from broker-worker over Unix socket
		res, err := pt.brokerClient.ForwardToBroker(ctx, proposal.ContractID, action, proposal.TotalSize, proposal.AveragePrice)
		if err != nil {
			log.Printf("[ERROR] Broker execution failed: %v", err)
			return
		}

		if !res.Success {
			log.Printf("[ERROR] Broker returned execution failure: %s", res.Message)
			return
		}

		// 3. Update Position Map from what actually filled, not what was asked for.
		fillSize, fillPrice := liveFill(proposal, res)
		var liveYield float64
		var liveClose bool
		pt.mu.Lock()
		if action == "BUY" {
			pos, exists := pt.positions[proposal.ContractID]
			if exists {
				newSize := pos.Size + fillSize
				pos.EntryPrice = ((pos.EntryPrice * float64(pos.Size)) + (fillPrice * float64(fillSize))) / float64(newSize)
				pos.Size = newSize
			} else {
				pt.positions[proposal.ContractID] = &Position{
					ContractID:   proposal.ContractID,
					EntryPrice:   fillPrice,
					Size:         fillSize,
					Timestamp:    time.Now(),
					Persona:      proposal.Persona,
					StrategyID:   proposal.StrategyID,
					Contributors: proposalContributors(proposal),
				}
			}
			log.Printf("[LIVE] Position OPEN/ADD success. Contract=%s | Filled=%d of %d | EntryPrice=%.4f | BrokerTx=%s",
				proposal.ContractID, fillSize, proposal.TotalSize, fillPrice, res.BrokerTxID)
		} else {
			pos, exists := pt.positions[proposal.ContractID]
			if exists {
				sellSize := fillSize
				if sellSize > pos.Size {
					sellSize = pos.Size
				}
				liveYield = (fillPrice - pos.EntryPrice) * float64(sellSize)
				log.Printf("[LIVE] Position CLOSE success. Contract=%s | Filled=%d of %d | Entry=%.4f | Exit=%.4f | P/L Yield=$%.2f | BrokerTx=%s",
					proposal.ContractID, fillSize, proposal.TotalSize, pos.EntryPrice, fillPrice, liveYield, res.BrokerTxID)
				pos.Size -= sellSize
				if pos.Size <= 0 {
					delete(pt.positions, proposal.ContractID)
				}
				liveClose = true
			} else {
				log.Printf("[LIVE] Sell signal received for %s but no active position held.", proposal.ContractID)
			}
		}
		pt.mu.Unlock() // release before any I/O or syncToRedis to avoid deadlock

		if liveClose {
			_, _ = pt.ledger.AppendTransaction(proposal.ContractID, action, fillSize, fillPrice, "LIVE_CLOSED", liveYield, res.BrokerTxID)
			pt.syncToRedis(ctx)
			return
		}

		// 4. Write LIVE_FILLED ledger receipt
		_, err = pt.ledger.AppendTransaction(proposal.ContractID, action, fillSize, fillPrice, "LIVE_FILLED", 0, res.BrokerTxID)
		if err != nil {
			log.Printf("[ERROR] Failed to write LIVE_FILLED receipt: %v", err)
		}
	}
	pt.syncToRedis(ctx)
}

// ragFeedbackKey is the Redis list onto which the executive pushes realized trade
// outcomes for Gordon's RAG layer to learn from. A durable list (not pub/sub) so an
// outcome isn't lost if Gordon happens to be restarting when a position closes.
const ragFeedbackKey = "gekko:rag_feedback"

// RAGFeedback is the realized outcome of a closed position, published so Gordon can
// embed it and store it in Qdrant. This closes the learning loop: without it Gordon's
// RAG only ever sees the hardcoded seed examples and never gets smarter from real trades.
type RAGFeedback struct {
	ContractID string  `json:"contract_id"`
	EntryPrice float64 `json:"entry_price"`
	ExitPrice  float64 `json:"exit_price"`
	ReturnPct  float64 `json:"return_pct"`
	Action     string  `json:"action"`  // entry action (tracked positions are long ⇒ BUY)
	Outcome    string  `json:"outcome"` // WIN | LOSS
	Text       string  `json:"text"`    // human-readable summary, embedded for future matches
}

// ragFeedbackSummary renders a one-line, human-readable description of a closed trade,
// e.g. "Bought FED-RATE-2026 at 0.65, resolved YES at 1.00 — +54% return". This text is
// what Gordon embeds, so it must read naturally for semantic matching against headlines.
func ragFeedbackSummary(t ClosedTrade) string {
	var closing string
	switch t.Side {
	case "YES":
		closing = fmt.Sprintf("resolved YES at %.2f", t.ExitPrice)
	case "NO":
		closing = fmt.Sprintf("resolved NO at %.2f", t.ExitPrice)
	default: // EXIT / sell-signal close
		closing = fmt.Sprintf("sold at %.2f", t.ExitPrice)
	}
	return fmt.Sprintf("Bought %s at %.2f, %s — %+.0f%% return", t.ContractID, t.EntryPrice, closing, t.ReturnPct)
}

// publishRAGFeedback pushes a closed trade's realized outcome onto the RAG feedback
// queue so Gordon can learn from it. Best-effort: the Redis write runs on its own
// goroutine, so this is safe to call while holding pt.mu and never blocks the caller
// or fails a settlement if Redis is down.
func (pt *PositionTracker) publishRAGFeedback(trade ClosedTrade) {
	if pt.redisClient == nil {
		return
	}
	fb := RAGFeedback{
		ContractID: trade.ContractID,
		EntryPrice: trade.EntryPrice,
		ExitPrice:  trade.ExitPrice,
		ReturnPct:  trade.ReturnPct,
		Action:     "BUY", // tracked positions are long entries; the close realizes the BUY
		Outcome:    "LOSS",
		Text:       ragFeedbackSummary(trade),
	}
	if trade.ReturnPct > 0 {
		fb.Outcome = "WIN"
	}
	data, err := json.Marshal(fb)
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := pt.redisClient.LPush(ctx, ragFeedbackKey, data).Err(); err != nil {
			log.Printf("[RAG-FEEDBACK] failed to publish outcome for %s: %v", trade.ContractID, err)
			return
		}
		// Cap the backlog so an unrun/disconnected Gordon can't grow the list unbounded.
		pt.redisClient.LTrim(ctx, ragFeedbackKey, 0, 999)
	}()
}

// TrackSettlement parses raw news headlines to spot contract close-outs/settlements.
func (pt *PositionTracker) TrackSettlement(rawContent string) {
	content := strings.ToLower(rawContent)
	if !strings.Contains(content, "closed yes") && !strings.Contains(content, "closed no") {
		return
	}

	// Format: "Closed yes AAPL-UP-200 market status update"
	// Find outcome
	isYes := strings.Contains(content, "closed yes")
	settlementValue := 0.0
	if isYes {
		settlementValue = 1.0 // YES settles at $1.00
	}

	// Extract contract ticker ID
	words := strings.Fields(rawContent)
	var ticker string
	for _, word := range words {
		// Look for standard uppercase contract patterns: e.g. AAPL-UP-200, BTC-100K-DEC, FED-RATE-2026
		if strings.Contains(word, "-") {
			ticker = strings.Trim(word, ".,:;\"'")
			break
		}
	}

	if ticker == "" {
		return
	}

	// Settle under the lock, then do I/O (ledger, Redis) after releasing it —
	// syncToRedis takes pt.mu.RLock, so calling it under pt.mu.Lock deadlocks.
	pt.mu.Lock()
	pos, exists := pt.positions[ticker]
	if !exists {
		pt.mu.Unlock()
		return
	}

	// Compute settlement yield delta: (settlementValue - entryPrice) * size
	yield := (settlementValue - pos.EntryPrice) * float64(pos.Size)
	status := "DRY_CLOSED"
	if !pt.dryMode {
		status = "LIVE_CLOSED"
	}

	// Settle the account like every other close path: credit the settlement proceeds
	// and realize the yield. This used to just delete the position — the entry debit
	// was never paid back, silently draining NAV on every news-driven settlement.
	pt.cash += settlementValue * float64(pos.Size)
	pt.realizedPnL += yield
	side := "NO"
	if isYes {
		side = "YES"
	}
	pt.recordClosedTradeLocked(pos, settlementValue, pos.Size, side, "SETTLED", 0)
	settled := *pos
	delete(pt.positions, ticker)
	pt.mu.Unlock()

	log.Printf("[SETTLEMENT] Contract %s Settled to %s! EntryPrice=%.4f | SettlementValue=%.2f | Size=%d | Net Yield Delta=$%.2f P/L",
		ticker, map[bool]string{true: "YES ($1.00)", false: "NO ($0.00)"}[isYes], settled.EntryPrice, settlementValue, settled.Size, yield)

	// Log to immudb ledger
	_, err := pt.ledger.AppendTransaction(ticker, "CLOSE", settled.Size, settlementValue, status, yield, "SETTLED")
	if err != nil {
		log.Printf("[ERROR] Failed to log settlement transaction: %v", err)
	}

	pt.syncToRedis(context.Background())
}

func (pt *PositionTracker) fetchRedisOrderBook(ctx context.Context, contractID string) (*OrderBook, error) {
	if pt.redisClient == nil {
		if pt.allowMockBook {
			return pt.generateMockBook(contractID), nil
		}
		return nil, nil
	}

	val, err := pt.redisClient.Get(ctx, "orderbook:"+contractID).Result()
	if err != nil {
		// No live book (key missing/expired, or marketdata isn't covering this
		// ticker). Fall through to nil so markPrice uses the position's entry
		// price as a stable, non-fabricated fallback. Mock books are only for
		// keyless dev/tests, behind ALLOW_MOCK_ORDERBOOK.
		if pt.allowMockBook {
			return pt.generateMockBook(contractID), nil
		}
		return nil, nil
	}

	var ob OrderBook
	if err := json.Unmarshal([]byte(val), &ob); err != nil {
		return nil, err
	}
	return &ob, nil
}

func (pt *PositionTracker) generateMockBook(contractID string) *OrderBook {
	basePrice := 0.50
	if len(contractID) > 0 {
		basePrice = 0.30 + float64(len(contractID)%40)/100.0
	}

	bids := make([]PriceLevel, 5)
	asks := make([]PriceLevel, 5)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < 5; i++ {
		bidPrice := basePrice - float64(i+1)*0.01
		bidVol := 300 + rng.Intn(400) - (i * 50)
		if bidVol < 50 {
			bidVol = 50
		}
		bids[i] = PriceLevel{Price: math.Round(bidPrice*100) / 100, Volume: bidVol}

		askPrice := basePrice + float64(i)*0.01
		askVol := 250 + rng.Intn(350) - (i * 40)
		if askVol < 50 {
			askVol = 50
		}
		asks[i] = PriceLevel{Price: math.Round(askPrice*100) / 100, Volume: askVol}
	}

	return &OrderBook{
		ContractID: contractID,
		Bids:       bids,
		Asks:       asks,
	}
}

func (pt *PositionTracker) syncToRedis(ctx context.Context) {
	if pt.redisClient == nil {
		return
	}

	// Sync active positions list
	pt.mu.RLock()
	posList := make([]*Position, 0, len(pt.positions))
	for _, pos := range pt.positions {
		posList = append(posList, pos)
	}
	pt.mu.RUnlock()

	posData, err := json.Marshal(posList)
	if err == nil {
		_ = pt.redisClient.Set(ctx, "gekko:positions", string(posData), 0).Err()
	}

	// Sync auto-approve (supervised/unsupervised) state
	autoVal := "false"
	pt.mu.RLock()
	if pt.autoApprove {
		autoVal = "true"
	}
	pt.mu.RUnlock()
	_ = pt.redisClient.Set(ctx, "gekko:auto_approve", autoVal, 0).Err()

	// Sync Ledger summary
	blocks, err := pt.ledger.GetBlocks()
	if err == nil {
		// Limit to last 15 blocks
		startIdx := 0
		if len(blocks) > 15 {
			startIdx = len(blocks) - 15
		}
		recentBlocks := blocks[startIdx:]
		if recentBlocks == nil {
			// An empty ledger yields a nil slice, which marshals to JSON null;
			// the frontend expects "blocks" to always be an array.
			recentBlocks = []TransactionBlock{}
		}

		summary := map[string]interface{}{
			"verified":     true,
			"block_count":  len(blocks),
			"last_updated": time.Now().Format(time.RFC3339),
			"blocks":       recentBlocks,
		}
		ledgerData, err := json.Marshal(summary)
		if err == nil {
			_ = pt.redisClient.Set(ctx, "gekko:ledger_summary", string(ledgerData), 0).Err()
		}
	}

	// Sync paper-trading account snapshot + closed-trade history.
	pt.syncAccountToRedis(ctx)

	// Publish the compact risk-state mirror for Gordon's pre-LLM veto.
	pt.publishRiskState(ctx)
}

func (pt *PositionTracker) syncPendingProposal(ctx context.Context, p *StrategyProposal) {
	if pt.redisClient == nil {
		return
	}
	data, err := json.Marshal(p)
	if err == nil {
		_ = pt.redisClient.Set(ctx, "gekko:pending_proposal", string(data), 0).Err()
	}
}

func (pt *PositionTracker) clearPendingProposal(ctx context.Context) {
	if pt.redisClient == nil {
		return
	}
	_ = pt.redisClient.Del(ctx, "gekko:pending_proposal").Err()
}

// liveFill is the size and price to book for a live order: the broker's confirmed fill when it
// reports one, otherwise the proposal's own (exchanges that do not report fills yet). The fill
// can only be smaller than the ask: a larger reported fill is clamped to the proposal.
func liveFill(p *StrategyProposal, res *TradeResult) (size int, price float64) {
	size, price = p.TotalSize, p.AveragePrice
	if res == nil {
		return
	}
	if res.FilledSize > 0 && res.FilledSize < size {
		size = res.FilledSize
	}
	if res.FilledPrice > 0 {
		price = res.FilledPrice
	}
	return
}
