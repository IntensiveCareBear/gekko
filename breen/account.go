package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// AccountState is the paper-trading account snapshot surfaced to the UI.
type AccountState struct {
	StartingCapital float64 `json:"starting_capital"`
	Cash            float64 `json:"cash"`
	NAV             float64 `json:"nav"`
	RealizedPnL     float64 `json:"realized_pnl"`
	UnrealizedPnL   float64 `json:"unrealized_pnl"`
	OpenPositions   int     `json:"open_positions"`
	PeakNAV         float64 `json:"peak_nav"`
	DrawdownPct     float64 `json:"drawdown_pct"`     // instantaneous peak-to-trough drawdown right now
	MaxDrawdownPct  float64 `json:"max_drawdown_pct"` // worst drawdown seen over the whole run (#11)
	UpdatedAtMs     int64   `json:"updated_at_ms"`
}

// EquityPoint is one sample of the equity curve over the run.
type EquityPoint struct {
	TimestampMs   int64   `json:"timestamp_ms"`
	NAV           float64 `json:"nav"`
	Cash          float64 `json:"cash"`
	RealizedPnL   float64 `json:"realized_pnl"`
	UnrealizedPnL float64 `json:"unrealized_pnl"`
	OpenPositions int     `json:"open_positions"`
	DrawdownPct   float64 `json:"drawdown_pct"`
	// BenchmarkNAV is the buy-and-hold value of the seed money in the comparison
	// index (default SPY) at this sample. Zero/omitted when the benchmark is off
	// or not yet anchored.
	BenchmarkNAV float64 `json:"benchmark_nav,omitempty"`
}

const (
	equityCurveKey      = "gekko:equity_curve"
	accountKey          = "gekko:account"
	tradesKey           = "gekko:trades"
	personaStatsKey     = "gekko:persona_stats"
	bookMetricsKey      = "gekko:book_metrics"
	calibrationKey      = "gekko:calibration"
	riskStateKey        = "gekko:risk_state"
	maxEquityPoints     = 5000 // ~14 days at 5 min
	maxClosedTrades     = 1000
	maxTradesSyncedJSON = 500
)

// randomExpiry returns a random duration within the configured expiry window.
func (pt *PositionTracker) randomExpiry() time.Duration {
	min, max := pt.expiryMin, pt.expiryMax
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int63n(int64(max-min)))
}

// inferExchange guesses the venue from the contract ticker (mirrors broker-worker's
// routeExchange): "POLY-" tickers are Polymarket, everything else Kalshi. This used to
// match "btc"/"eth" anywhere, which labelled Kalshi's KXBTCD/KXETHD as Polymarket and
// zeroed their Kalshi fees.
func inferExchange(contractID string) string {
	if strings.HasPrefix(strings.ToUpper(contractID), "POLY-") {
		return "polymarket"
	}
	return "kalshi"
}

// markEntry is one cached LIVE mark (never a fallback price) with its capture time.
type markEntry struct {
	Price float64
	At    time.Time
}

// resolveMark picks the price a position is valued at when no live book may exist:
// live mid → one-sided quote → cached mark no older than maxAge → floor. The floor
// depends on what the price is for: NAV snapshots use entry (0 unrealized — a haircut
// there would oscillate NAV and could falsely trip the drawdown halt), while
// realizations (force-exit, liquidation) use entry × haircut so a close with no live
// quote can never book a flattering "flat at entry" fill (2026-07-03 post-mortem). The
// source tag ("live", "onesided", "cached", "entry", "haircut") feeds logs and ledger
// refs.
//
// A one-sided quote outranks the cached tier on purpose: it is a price observed NOW,
// where the cache is a mid observed up to maxAge ago. When a contract is dying, that
// ordering is the whole point — the stale mid is exactly the number that flatters it.
func resolveMark(live float64, quality quoteQuality, cached markEntry, cachedOK bool, entry, haircut float64, maxAge time.Duration, forRealization bool) (float64, string) {
	if quality == quoteTwoSided {
		return live, "live"
	}
	if quality == quoteOneSided && live > 0 {
		return live, "onesided"
	}
	if cachedOK && maxAge > 0 && time.Since(cached.At) <= maxAge {
		return cached.Price, "cached"
	}
	if forRealization {
		return entry * haircut, "haircut"
	}
	return entry, "entry"
}

// cachedMark returns the last live mark snapshotAccount stored for a contract.
func (pt *PositionTracker) cachedMark(contractID string) (markEntry, bool) {
	pt.mu.RLock()
	defer pt.mu.RUnlock()
	m, ok := pt.markCache[contractID]
	return m, ok
}

// resolveMarkFor fetches a live mark and applies the fallback tiering for one contract.
func (pt *PositionTracker) resolveMarkFor(ctx context.Context, contractID string, entry float64, forRealization bool) (float64, string) {
	live, quality := pt.markQuote(ctx, contractID)
	cached, cachedOK := pt.cachedMark(contractID)
	return resolveMark(live, quality, cached, cachedOK, entry, pt.markHaircut, pt.markStaleMaxAge, forRealization)
}

// quoteQuality ranks how much of a book's top-of-book is actually there (2026-08-30).
//
// The distinction exists because a book with one side quoted is not the same thing as
// no book at all, and the old code conflated them. A decided intraday contract loses
// its bid long before it expires — nobody bids for a weather strike the temperature has
// already settled — so the book goes ask-only while the position rots to zero. Treating
// that as "no quote" sent valuation down the fallback ladder to ENTRY, which marks a
// dying position flat at cost. The 2026-08-30 weather book is the receipt: five
// contracts that resolved at $0.00 recorded max adverse excursions of −0.56%, 0.00% and
// −3.33%, because their marks never left entry.
type quoteQuality int

const (
	quoteNone     quoteQuality = iota // no usable top-of-book (absent, empty, or stale)
	quoteOneSided                     // one side quoted — a real price, but a bound rather than a mid
	quoteTwoSided                     // both sides quoted — a true mid
)

// markQuote returns the best price a contract can be valued at right now, with the
// quality of the evidence behind it.
//
// For a long position (every position here is long — a bought YES or a bought NO):
//   - both sides quoted → the mid, as before.
//   - bids only → the best bid. That is the price the position could actually be sold
//     at, so it is a real, conservative mark.
//   - asks only → the best ask. Nobody is bidding, so this is an upper bound on what
//     the contract is worth, not an exit price — but a contract offered at 0.02 is
//     provably not worth the 0.89 it was bought for, and the bound is the honest number.
func (pt *PositionTracker) markQuote(ctx context.Context, contractID string) (float64, quoteQuality) {
	ob, err := pt.fetchRedisOrderBook(ctx, contractID)
	if err != nil {
		return 0, quoteNone
	}
	return quoteFromBook(ob, pt.bookFreshMaxAge, time.Now())
}

// quoteFromBook is the pure top-of-book → (price, quality) decision behind markQuote.
func quoteFromBook(ob *OrderBook, freshMaxAge time.Duration, now time.Time) (float64, quoteQuality) {
	if ob == nil {
		return 0, quoteNone
	}
	// A stamped book older than freshMaxAge is not "live": the Redis book TTL
	// now outlives a poll cycle, so key existence alone no longer bounds staleness.
	// Unstamped books (mocks, older marketdata builds) keep TTL semantics.
	if freshMaxAge > 0 && ob.AsOfMs > 0 && now.Sub(time.UnixMilli(ob.AsOfMs)) > freshMaxAge {
		return 0, quoteNone
	}
	switch {
	case len(ob.Bids) > 0 && len(ob.Asks) > 0:
		return (ob.Bids[0].Price + ob.Asks[0].Price) / 2.0, quoteTwoSided
	case len(ob.Bids) > 0:
		return ob.Bids[0].Price, quoteOneSided
	case len(ob.Asks) > 0:
		return ob.Asks[0].Price, quoteOneSided
	}
	return 0, quoteNone
}

// markPriceLive returns the current mid-price for a contract from the mock/Redis
// book, with ok=false unless a fresh TWO-SIDED book exists. Callers that must never
// act on a fabricated price require ok=true. Valuation callers use markQuote directly
// and accept quoteOneSided; callers that need to SELL use exitQuote.
func (pt *PositionTracker) markPriceLive(ctx context.Context, contractID string) (float64, bool) {
	price, q := pt.markQuote(ctx, contractID)
	return price, q == quoteTwoSided
}

// exitQuote returns the price a LONG position could actually be sold at right now —
// the best bid — with ok=false when nothing is bidding (2026-09-03).
//
// This replaces a two-sided-book requirement that was half right. Every position here
// is long, so an exit crosses the BID; the ask side is irrelevant to it. Grading both
// "bids only" and "asks only" as quoteOneSided and refusing to trade either conflated
// the two opposite cases:
//
//   - asks only, no bid — genuinely unexitable. Nobody will buy it from us. Correctly
//     refused, before and now.
//   - bids only, no ask — a contract the market wants so badly nobody will offer it.
//     Maximally exitable, and refused anyway.
//
// The second case is not a corner: it is exactly what a weather bucket looks like once
// the temperature is sitting inside it. On 2026-09-03 KXHIGHCHI-26SEP03-B92.5 (entered
// at 0.15, 150 lots) carried 34,616 contracts bid at 0.99 and zero asks — $126 of
// deep, executable exit that the two-sided rule made unreachable. Six of the ten
// weather positions that reached a 0.97+ mark went on to settle at $0.00.
//
// The price is the bid rather than the mid on purpose. It is what the fill would be,
// it is the same leg SpreadCostUSD already assumes on the DRY_CLOSED path, and it is
// the same discipline as resolveMark's realization haircut: an exit must never book at
// a price better than the book would have given us.
func (pt *PositionTracker) exitQuote(ctx context.Context, contractID string) (float64, bool) {
	ob, err := pt.fetchRedisOrderBook(ctx, contractID)
	if err != nil {
		return 0, false
	}
	return bidFromBook(ob, pt.bookFreshMaxAge, time.Now())
}

// bidFromBook is the pure top-of-book → best-bid decision behind exitQuote. Staleness
// is judged exactly as quoteFromBook judges it, so valuation and exit never disagree
// about whether a book is live.
func bidFromBook(ob *OrderBook, freshMaxAge time.Duration, now time.Time) (float64, bool) {
	if ob == nil {
		return 0, false
	}
	if freshMaxAge > 0 && ob.AsOfMs > 0 && now.Sub(time.UnixMilli(ob.AsOfMs)) > freshMaxAge {
		return 0, false
	}
	if len(ob.Bids) == 0 || ob.Bids[0].Price <= 0 {
		return 0, false
	}
	return ob.Bids[0].Price, true
}

// markPrice returns the current mid-price for a contract, or fallback when no live
// book exists.
func (pt *PositionTracker) markPrice(ctx context.Context, contractID string, fallback float64) float64 {
	if mid, ok := pt.markPriceLive(ctx, contractID); ok {
		return mid
	}
	return fallback
}

// excursionPct returns a position's max favorable / max adverse excursion as a
// percentage of its entry basis, from the LIVE marks observed while it was open.
// exitPrice folds in as one more observation when it is a price the position could
// actually have been exited at — pass 0 for a binary settlement, whose payout is not
// a tradeable price and would otherwise collapse every settled trade onto ±100%.
// Unmarked positions (zero extremes) fall back to the entry basis. Pure.
func excursionPct(pos *Position, exitPrice float64) (mfe, mae float64) {
	if pos == nil || pos.EntryPrice <= 0 {
		return 0, 0
	}
	best, worst := pos.BestMark, pos.WorstMark
	if best <= 0 {
		best = pos.EntryPrice
	}
	if worst <= 0 {
		worst = pos.EntryPrice
	}
	if exitPrice > 0 {
		if exitPrice > best {
			best = exitPrice
		}
		if exitPrice < worst {
			worst = exitPrice
		}
	}
	mfe = (best - pos.EntryPrice) / pos.EntryPrice * 100.0
	mae = (worst - pos.EntryPrice) / pos.EntryPrice * 100.0
	return round2(mfe), round2(mae)
}

// recordClosedTradeLocked appends a closed trade. Caller must hold pt.mu.
// exitSpreadPerContract is the half-spread actually crossed to get out: nonzero only
// when the exit filled off a live book (the signal-driven DRY_CLOSED path), zero for
// exits marked at the mid and for binary settlements.
func (pt *PositionTracker) recordClosedTradeLocked(pos *Position, exitPrice float64, size int, side, status string, exitSpreadPerContract float64) {
	// Costs first: from the 2026-08-25 desk review, a charged fee has to be inside every
	// number this function goes on to produce — the trade record, the persona scores AND
	// the per-family/category accruals the risk guards read.
	costs := closeCosts(pos, exitPrice, size, exitSpreadPerContract, pt.feeRate, pt.feeModel)
	// Draw the apportioned entry-side costs down as they are consumed, so a second
	// partial close can't bill the same fill's spread and fee twice.
	consumeEntryCosts(pos, size)

	// Score each contributing persona on its OWN proposal vs the actual close.
	personaScores := personaScoresForClose(pos, exitPrice, size)

	realized := (exitPrice - pos.EntryPrice) * float64(size)
	ret := 0.0
	if pos.EntryPrice > 0 {
		ret = (exitPrice - pos.EntryPrice) / pos.EntryPrice * 100.0
	}
	// Truth scoreboard (desk review fix #1). Charging the fee has to mean charging it
	// EVERYWHERE, not just to cash: the allocator, Lou and the persona scorecard all
	// optimize against RealizedPnL/ReturnPct, so leaving those gross would keep every
	// learner tuning a flattered number — worst on exactly the small mid-price clips
	// where 7%·P(1−P) bites hardest. When the fee is NOT charged nothing here moves and
	// a run stays comparable with every run before it.
	feeCharged := pt.feeCharge && costs.FeeUSD > 0
	if feeCharged {
		realized -= costs.FeeUSD
		ret = 0
		if cost := pos.EntryPrice * float64(size); cost > 0 {
			ret = realized / cost * 100.0
		}
		applyFeeToPersonaScores(personaScores, costs.FeeUSD)
	}

	// Accrue per-FAMILY realized PnL and stamp the close time, feeding the loss limit
	// and re-entry cooldown guards in riskGate. Keying on family (not exact contract_id)
	// keeps a strike ladder on one shared budget/cooldown. Uses the same net figure: a
	// family that bled $300 of P&L plus $60 of fees has done $360 of damage.
	fam := contractFamily(pos.ContractID)
	if pt.contractRealizedPnL != nil {
		pt.contractRealizedPnL[fam] += realized
	}
	if pt.lastExit != nil {
		pt.lastExit[fam] = time.Now()
	}
	// Same accrual at correlation-category grain for the category loss limit (#5).
	if cat := contractCategory(pos.ContractID); cat != "" && pt.categoryRealizedPnL != nil {
		pt.categoryRealizedPnL[cat] += realized
	}

	// Excursions: a binary resolution (side YES/NO) contributes no exit observation —
	// see excursionPct. Every other close exits at an actual price.
	excursionExit := exitPrice
	if side != "EXIT" {
		excursionExit = 0
	}
	mfe, mae := excursionPct(pos, excursionExit)

	pt.closedTrades = append(pt.closedTrades, ClosedTrade{
		ContractID:               pos.ContractID,
		Exchange:                 inferExchange(pos.ContractID),
		Side:                     side,
		EntryPrice:               pos.EntryPrice,
		ExitPrice:                exitPrice,
		Size:                     size,
		RealizedPnL:              realized,
		ReturnPct:                ret,
		OpenedAtMs:               pos.Timestamp.UnixMilli(),
		ClosedAtMs:               time.Now().UnixMilli(),
		Status:                   status,
		Persona:                  pos.Persona,
		Contributors:             pos.Contributors,
		PersonaScores:            personaScores,
		MaxFavorableExcursionPct: mfe,
		MaxAdverseExcursionPct:   mae,
		SpreadCostUSD:            costs.SpreadUSD,
		FeeUSD:                   costs.FeeUSD,
		FeeCharged:               feeCharged,
	})
	if pt.feeCharge {
		// Cash loses only the EXIT leg here — the entry leg already left cash at fill.
		// Realized P&L books the WHOLE round trip now, because this is when the trade's
		// result is struck.
		pt.cash -= kalshiFee(pos.ContractID, exitPrice, size, pt.feeRate, pt.feeModel)
		pt.realizedPnL -= costs.FeeUSD
	}
	if len(pt.closedTrades) > maxClosedTrades {
		pt.closedTrades = pt.closedTrades[len(pt.closedTrades)-maxClosedTrades:]
	}
	// Feed the adaptive allocator (Phase 4) each persona's OWN scored return, so the
	// bandit learns from how each persona actually called it — not the identical
	// consolidated outcome credited to everyone.
	if pt.allocator != nil {
		for _, s := range personaScores {
			pt.allocator.Observe(s.Persona, s.ReturnPct)
		}
	}
	// Append to the run-log export (own mutex; small append).
	if pt.exporter != nil {
		pt.exporter.AppendTrade(pt.closedTrades[len(pt.closedTrades)-1])
	}
	// Close the RAG learning loop: publish this realized outcome so Gordon embeds it
	// and stores it in Qdrant. Off-lock Redis write; safe to call while holding pt.mu.
	pt.publishRAGFeedback(pt.closedTrades[len(pt.closedTrades)-1])
}

// snapshotAccount computes the current account state (cash + mark-to-market NAV).
func (pt *PositionTracker) snapshotAccount(ctx context.Context) AccountState {
	// Copy positions + scalars under the lock, then mark off-lock.
	pt.mu.RLock()
	cash := pt.cash
	realized := pt.realizedPnL
	starting := pt.startingCapital
	type pm struct {
		id    string
		entry float64
		size  int
	}
	open := make([]pm, 0, len(pt.positions))
	for _, p := range pt.positions {
		open = append(open, pm{p.ContractID, p.EntryPrice, p.Size})
	}
	pt.mu.RUnlock()

	marketValue := 0.0
	entryCost := 0.0
	now := time.Now()
	liveMarks := make(map[string]markEntry, len(open))
	noQuote := 0
	oneSided := 0
	for _, p := range open {
		mark, source := pt.resolveMarkFor(ctx, p.id, p.entry, false)
		switch source {
		case "live":
			liveMarks[p.id] = markEntry{Price: mark, At: now}
		case "onesided":
			// Cached as well: a one-sided top-of-book is an observed price, so it is
			// better evidence for the next snapshot than the entry basis would be.
			liveMarks[p.id] = markEntry{Price: mark, At: now}
			oneSided++
		default:
			noQuote++
		}
		marketValue += mark * float64(p.size)
		entryCost += p.entry * float64(p.size)
	}
	pt.noQuoteMarks.Store(int64(noQuote))
	pt.oneSidedMarks.Store(int64(oneSided))
	// Reported separately because they mean different things: a one-sided position is
	// being valued off a real (if thin) quote, while a no-quote position is being valued
	// off cache or cost and its P&L is not being observed at all.
	if noQuote > 0 || oneSided > 0 {
		log.Printf("[MARK] %d/%d positions on one-sided books, %d/%d with no quote at all (valued at cached/entry marks).",
			oneSided, len(open), noQuote, len(open))
	}
	nav := cash + marketValue
	unrealized := marketValue - entryCost

	// Update running peak + drawdown under the lock. Cache NAV for the risk gate's
	// drawdown circuit breaker and keep the running worst drawdown for reporting (#11).
	pt.mu.Lock()
	if nav > pt.peakNAV {
		pt.peakNAV = nav
	}
	peak := pt.peakNAV
	drawdown := 0.0
	if peak > 0 {
		drawdown = (peak - nav) / peak * 100.0
	}
	if drawdown > pt.maxDrawdownPct {
		pt.maxDrawdownPct = drawdown
	}
	maxDD := pt.maxDrawdownPct
	pt.lastNAV = nav
	// Upsert LIVE marks into the cache the risk gate and realization fallbacks read
	// (fallback prices are never cached), and prune entries for closed positions.
	openSet := make(map[string]struct{}, len(open))
	for _, p := range open {
		openSet[p.id] = struct{}{}
	}
	for id, m := range liveMarks {
		pt.markCache[id] = m
	}
	for id := range pt.markCache {
		if _, stillOpen := openSet[id]; !stillOpen {
			delete(pt.markCache, id)
		}
	}
	pt.mu.Unlock()

	return AccountState{
		StartingCapital: starting,
		Cash:            cash,
		NAV:             nav,
		RealizedPnL:     realized,
		UnrealizedPnL:   unrealized,
		OpenPositions:   len(open),
		PeakNAV:         peak,
		DrawdownPct:     drawdown,
		MaxDrawdownPct:  maxDD,
		UpdatedAtMs:     time.Now().UnixMilli(),
	}
}

// PersonaStat is the per-persona scorecard surfaced to the UI (Phase 2 tracking).
type PersonaStat struct {
	Persona     string  `json:"persona"`
	Trades      int     `json:"trades"`
	Wins        int     `json:"wins"`
	Losses      int     `json:"losses"`
	WinRate     float64 `json:"win_rate"`     // percentage
	RealizedPnL float64 `json:"realized_pnl"` // USD
	// ReturnOnCapitalPct is the capital-weighted return: total P&L / total capital
	// deployed (entry × size). Unlike a mean of per-trade percentages, this
	// reconciles with realized P&L — binary contracts have uncapped winning % but a
	// −100% loss floor, so a per-trade average is wildly skewed by cheap winners.
	ReturnOnCapitalPct float64 `json:"return_on_capital_pct"`
	// Risk-adjusted metrics (expectancy, profit factor, per-trade Sharpe/Sortino, max
	// drawdown) computed from this persona's own time-ordered outcome series. Embedded
	// so its fields flatten into the persona JSON the scorecard already consumes.
	RiskMetrics
}

// creditedPersonas returns the personas to attribute a trade to: the explicit
// contributor list (consensus orders), else the single persona, else "unattributed".
func creditedPersonas(persona string, contributors []string) []string {
	if len(contributors) > 0 {
		return contributors
	}
	if persona == "" {
		return []string{"unattributed"}
	}
	return []string{persona}
}

// personaScoresForClose scores each contributing persona on its OWN proposal
// (size-weighted entry price) against the actual close price, sized to its
// PROPORTIONAL SHARE of the quantity actually being closed. Normalizing to the
// real closed size (rather than the raw proposed sizes) matters because proposals
// routinely exceed what executed: consensus consolidation scales the order by the
// conviction multiplier, the buying-power check caps the fill, and partial closes
// shrink the position while the stored proposals keep their full size. Scoring on
// raw proposal sizes inflated attributed PnL ~9x over the 2026-07-13/14 run
// (personas charged -$6,531 on a book that lost $728), poisoning the persona
// scorecard, kill-switch weights, and lou's return-on-capital analyzer. With the
// share normalization, per-persona PnL sums to the trade's PnL (up to each
// persona's own entry-price differentiation). Falls back to the position's own
// basis credited to each contributor when no individual proposals were captured
// (legacy/replayed orders).
func personaScoresForClose(pos *Position, exitPrice float64, size int) []PersonaScore {
	type agg struct {
		size float64
		cost float64 // size-weighted entry numerator (Σ price·size)
		conf float64 // size-weighted confidence numerator (Σ confidence·size)
	}
	by := map[string]*agg{}
	order := []string{}
	add := func(name string, price float64, sz float64, conf float64) {
		a := by[name]
		if a == nil {
			a = &agg{}
			by[name] = a
			order = append(order, name)
		}
		a.size += sz
		a.cost += price * sz
		a.conf += conf * sz
	}
	for _, cp := range pos.ContributorProposals {
		add(personaOrDefault(cp.Persona), cp.EntryPrice, float64(cp.Size), cp.Confidence)
	}
	if len(order) == 0 {
		// No captured proposals — credit each contributor at the position's basis.
		// Confidence is unrecoverable here, so these score at 0 and the calibration
		// table skips them rather than binning them as "claimed nothing".
		for _, name := range creditedPersonas(pos.Persona, pos.Contributors) {
			add(name, pos.EntryPrice, float64(pos.Size), 0)
		}
	}

	totalProposed := 0.0
	for _, a := range by {
		totalProposed += a.size
	}
	if totalProposed <= 0 {
		return nil
	}

	scores := make([]PersonaScore, 0, len(order))
	for _, name := range order {
		a := by[name]
		if a.size <= 0 {
			continue
		}
		entry := a.cost / a.size
		scaledSize := a.size / totalProposed * float64(size)
		ret := 0.0
		if entry > 0 {
			ret = (exitPrice - entry) / entry * 100.0
		}
		scores = append(scores, PersonaScore{
			Persona:     name,
			RealizedPnL: math.Round((exitPrice-entry)*scaledSize*100) / 100,
			ReturnPct:   math.Round(ret*100) / 100,
			Cost:        math.Round(entry*scaledSize*100) / 100,
			Confidence:  math.Round(a.conf/a.size*1000) / 1000,
		})
	}
	return scores
}

// computePersonaStats aggregates per-persona scorecard stats from closed trades.
// Each trade carries PersonaScores — each contributing persona scored on its own
// proposal vs the actual outcome — so personas differentiate even on consensus
// orders. Legacy/replayed trades without scores fall back to crediting each
// Contributor with the trade-level result. Unattributed trades bucket together.
// capitalBase is the drawdown-% denominator: personas share the book, so this is
// the run's starting capital (there are no per-persona capital budgets).
func computePersonaStats(trades []ClosedTrade, capitalBase float64, minSamples int) []PersonaStat {
	type acc struct {
		n, wins    int
		pnl, cost  float64
		orderedPnL []float64 // per-scored-trade P&L, in close-time order (for risk metrics)
		orderedCst []float64 // capital deployed on each, aligned with orderedPnL
	}
	by := map[string]*acc{}
	order := []string{} // first-seen order, for stable output among equal P&L
	add := func(name string, pnl, cost float64) {
		a := by[name]
		if a == nil {
			a = &acc{}
			by[name] = a
			order = append(order, name)
		}
		a.n++
		a.pnl += pnl
		a.cost += cost
		a.orderedPnL = append(a.orderedPnL, pnl)
		a.orderedCst = append(a.orderedCst, cost)
		if pnl > 0 {
			a.wins++
		}
	}

	// Walk trades in close-time order so each persona's series is chronological
	// (drawdown/Sharpe depend on ordering); the raw slice is roughly append-order
	// already, but replay/backfill can interleave, so sort explicitly.
	ordered := make([]ClosedTrade, len(trades))
	copy(ordered, trades)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ClosedAtMs < ordered[j].ClosedAtMs })

	for _, t := range ordered {
		if len(t.PersonaScores) > 0 {
			for _, s := range t.PersonaScores {
				cost := s.Cost
				if cost == 0 && s.ReturnPct != 0 {
					cost = s.RealizedPnL / (s.ReturnPct / 100.0) // back-compat: scores written before Cost existed
				}
				add(s.Persona, s.RealizedPnL, math.Abs(cost))
			}
			continue
		}
		// Legacy/replayed trade: credit each contributor with the trade-level result.
		cost := math.Abs(t.EntryPrice * float64(t.Size))
		for _, p := range creditedPersonas(t.Persona, t.Contributors) {
			add(p, t.RealizedPnL, cost)
		}
	}

	out := make([]PersonaStat, 0, len(by))
	for _, name := range order {
		a := by[name]
		ps := PersonaStat{
			Persona: name, Trades: a.n, Wins: a.wins, Losses: a.n - a.wins,
			RealizedPnL: round2(a.pnl),
			RiskMetrics: computeRiskMetrics(a.orderedPnL, a.orderedCst, capitalBase, minSamples),
		}
		if a.n > 0 {
			ps.WinRate = math.Round(float64(a.wins)/float64(a.n)*10000) / 100
		}
		if a.cost > 0 {
			ps.ReturnOnCapitalPct = math.Round(a.pnl/a.cost*10000) / 100
		}
		out = append(out, ps)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RealizedPnL > out[j].RealizedPnL })
	return out
}

// personaStatsSnapshot returns current per-persona stats from a copy of the closed
// trades (safe to call concurrently; used by the consensus engine for weighting).
func (pt *PositionTracker) personaStatsSnapshot() []PersonaStat {
	pt.mu.RLock()
	trades := make([]ClosedTrade, len(pt.closedTrades))
	copy(trades, pt.closedTrades)
	pt.mu.RUnlock()
	return computePersonaStats(trades, pt.startingCapital, pt.metricsMinSamples)
}

// syncAccountToRedis writes the account snapshot, closed-trade history, and
// per-persona scorecard to Redis.
func (pt *PositionTracker) syncAccountToRedis(ctx context.Context) {
	// Refresh the calibration cache BEFORE the Redis check: from Phase 1 the table
	// feeds decisions, not just the UI, so it has to stay current on a keyless/offline
	// run too. Cheap, and every close path lands here.
	cal := pt.refreshCalibration()

	if pt.redisClient == nil {
		return
	}

	acct := pt.snapshotAccount(ctx)
	if data, err := json.Marshal(acct); err == nil {
		_ = pt.redisClient.Set(ctx, accountKey, string(data), 0).Err()
	}

	pt.mu.RLock()
	trades := make([]ClosedTrade, len(pt.closedTrades))
	copy(trades, pt.closedTrades)
	pt.mu.RUnlock()
	if len(trades) > maxTradesSyncedJSON {
		trades = trades[len(trades)-maxTradesSyncedJSON:]
	}
	if data, err := json.Marshal(trades); err == nil {
		_ = pt.redisClient.Set(ctx, tradesKey, string(data), 0).Err()
	}

	if data, err := json.Marshal(computePersonaStats(trades, pt.startingCapital, pt.metricsMinSamples)); err == nil {
		_ = pt.redisClient.Set(ctx, personaStatsKey, string(data), 0).Err()
	}

	// Book-level ("breen") scorecard: same risk-adjusted block for the whole account
	// plus win-rate-by-entry-band, published for the System-tab card.
	if data, err := json.Marshal(computeBookMetrics(trades, pt.startingCapital, pt.metricsMinSamples)); err == nil {
		_ = pt.redisClient.Set(ctx, bookMetricsKey, string(data), 0).Err()
	}

	// Confidence calibration: stated conviction vs realized win rate, per persona.
	// Always published for the System tab; consumed by consensus and the mid-band gate
	// only when CALIBRATION_ENABLED is on (Phase 1).
	if data, err := json.Marshal(cal); err == nil {
		_ = pt.redisClient.Set(ctx, calibrationKey, string(data), 0).Err()
	}
}

// runSettlementLoop resolves expired positions to YES ($1) / NO ($0) probabilistically.
func (pt *PositionTracker) runSettlementLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	log.Printf("[INFO] Settlement loop active (expiry window %v–%v).", pt.expiryMin, pt.expiryMax)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Stop out bleeding positions first, then force-exit imminent
			// resolutions (#2), then settle anything that genuinely reached
			// expiry this tick.
			pt.checkStopLosses(ctx)
			pt.forceExitNearExpiry(ctx)
			pt.settleExpired(ctx)
		}
	}
}

// stopLossBreached reports whether a position's unrealized loss has tripped the
// stop, on the FIRST of: pct% of entry cost, or usd dollars. A zero limb is
// disabled; both zero means no stop. minAbs is an absolute floor on the
// per-contract adverse move: below it neither limb fires, so a one-tick wiggle
// on a cheap contract can't stop the position out (0 disables).
func stopLossBreached(entry, mark float64, size int, pct, usd, minAbs float64) (bool, string) {
	if minAbs > 0 && entry-mark < minAbs {
		return false, ""
	}
	unreal := (mark - entry) * float64(size)
	cost := entry * float64(size)
	if pct > 0 && cost > 0 && unreal <= -pct/100.0*cost {
		return true, fmt.Sprintf("unrealized $%.2f ≤ -%.0f%% of $%.2f cost", unreal, pct, cost)
	}
	if usd > 0 && unreal <= -usd {
		return true, fmt.Sprintf("unrealized $%.2f ≤ -$%.0f", unreal, usd)
	}
	return false, ""
}

// takeProfitReached reports whether a position's unrealized GAIN has reached the
// take-profit target, on the FIRST of: pct% of entry cost, or usd dollars. A zero
// limb is disabled; both zero means no take-profit. The mirror image of
// stopLossBreached — banks winners that would otherwise ride to resolution risk or
// mean-revert (2026-07-11: run #2 entries <0.20 won 68% but only realize the win at
// settlement). No absolute floor: banking a tick early on noise just realizes a
// small gain, unlike a noise-triggered stop which crystallizes a loss.
func takeProfitReached(entry, mark float64, size int, pct, usd float64) (bool, string) {
	unreal := (mark - entry) * float64(size)
	cost := entry * float64(size)
	if pct > 0 && cost > 0 && unreal >= pct/100.0*cost {
		return true, fmt.Sprintf("unrealized $%.2f ≥ +%.0f%% of $%.2f cost", unreal, pct, cost)
	}
	if usd > 0 && unreal >= usd {
		return true, fmt.Sprintf("unrealized $%.2f ≥ +$%.0f", unreal, usd)
	}
	return false, ""
}

// trailingStopHit reports whether a position that has ALREADY RUN in our favour is
// now handing the run back, and should be banked before it round-trips to nothing.
//
// Two conditions, both required:
//
//   - Armed: the best live mark seen while open reached armPct% above entry. Below
//     that the position behaves exactly as it does today — the trail can never turn a
//     trade that was never winning into an early exit.
//   - Retraced: the mark has fallen retracePct% OF ENTRY from that best mark. Measuring
//     the give-back against the entry basis (not against the peak, and not as a share
//     of the gain) keeps it on the same scale as every other exit knob in this file,
//     which are all percentages of entry cost.
//
// minAbs is the same absolute floor stopLossBreached uses, and for the same reason: on
// a $0.03 contract a single tick is a huge percentage move, so without it an armed
// position would be stopped out by one tick of book noise. Arming alone does not
// protect against that — a position can arm on a real move and then be knocked out by
// noise — which is why the floor is applied here too rather than relying on the arm.
//
// best ≤ 0 means the position was never marked (a replayed position): treat the entry
// as the high, which leaves it unarmed. Pure.
//
// Evidence for this existing at all (2026-08-21, one day of Phase 0 excursion data):
// 15 of 31 trades ran +10% or better at some point and collectively realized +$27.93,
// with 7 of the 15 closing red. Winners were routinely giving back double-digit
// excursions with nothing in place to harvest them.
func trailingStopHit(entry, best, mark float64, size int, armPct, retracePct, minAbs float64) (bool, string) {
	if armPct <= 0 || retracePct <= 0 || entry <= 0 {
		return false, "" // either limb zero ⇒ trail disabled
	}
	if best <= 0 {
		best = entry
	}
	if best < mark {
		best = mark // a new high this tick: nothing has been given back
	}
	// Epsilon on both thresholds: prices are whole cents parsed into binary floats, so
	// an exactly-at-threshold move lands a hair under it — (0.24−0.20)/0.20×100 is
	// 19.999999999999996, and a bare comparison would refuse to arm a clean +20% run.
	// Same reason entryBookBlock carries one.
	const eps = 1e-9
	gainPct := (best - entry) / entry * 100.0
	if gainPct < armPct-eps {
		return false, "" // never ran far enough to arm
	}
	giveback := best - mark
	if minAbs > 0 && giveback < minAbs-eps {
		return false, "" // inside the tick-noise floor
	}
	if giveback < retracePct/100.0*entry-eps {
		return false, ""
	}
	unreal := (mark - entry) * float64(size)
	return true, fmt.Sprintf("ran +%.0f%% to %.4f then gave back %.4f (≥%.0f%% of entry) — banking $%.2f",
		gainPct, best, giveback, retracePct, unreal)
}

// stopExit is one position closed by the stop-loss pass, carried off-lock for
// ledger/log I/O.
type stopExit struct {
	pos    Position
	mark   float64
	yield  float64
	status string // STOP_LOSS, TRAIL_STOP, TAKE_PROFIT (per-position), or LOSS_LIMIT (family bleed)
	reason string
}

// checkStopLosses marks every open position against LIVE books only and closes the
// ones past the stop (2026-07-03 post-mortem: with no position-level stop, 51 trades
// rode past -20% for -$8.5k while the loss "limit" only blocked re-entries).
// Positions with no live book are left for forceExitNearExpiry/settleExpired.
//
// It also folds this tick's marks into every position's excursion record, which is why
// the mark fetch is no longer behind the exit-limb check: MAE/MFE is measurement, not
// policy, and must keep accruing on a run with every exit limb switched off.
func (pt *PositionTracker) checkStopLosses(ctx context.Context) {
	pt.mu.RLock()
	// Every limb that can close a position, because the exit pass is skipped outright
	// when none is armed. The settlement take-profit belongs here for the same reason
	// the trail did: a run configured with only that knob would otherwise harvest
	// nothing at all, silently.
	exitsEnabled := pt.stopLossPct > 0 || pt.stopLossUSD > 0 ||
		pt.takeProfitPct > 0 || pt.takeProfitUSD > 0 ||
		(pt.trailArmPct > 0 && pt.trailRetracePct > 0) ||
		(len(pt.settlementHoldPersonas) > 0 && pt.settlementHoldTPPrice > 0) ||
		(pt.perContractLossCloses && pt.caps.PerContractLossLimit > 0)
	ids := make([]string, 0, len(pt.positions))
	for id := range pt.positions {
		ids = append(ids, id)
	}
	pt.mu.RUnlock()
	if len(ids) == 0 {
		return
	}

	// Two maps, because valuing and acting need different evidence (2026-08-30).
	// observed accepts one-sided books: a contract whose bid has vanished is still
	// telling us what it is worth, and that is precisely the tape MFE/MAE must record —
	// previously a dying position simply stopped being observed and its MAE froze at
	// entry.
	//
	// marks is what an exit may fire on, and as of 2026-09-03 that is the best BID
	// rather than a two-sided mid. Every position is long, so an exit crosses the bid
	// and the ask side has no say in whether it can happen — see exitQuote for why the
	// old two-sided rule blocked precisely the exits worth taking.
	observed := make(map[string]float64, len(ids))
	marks := make(map[string]float64, len(ids))
	for _, id := range ids {
		if price, quality := pt.markQuote(ctx, id); quality != quoteNone && price > 0 {
			observed[id] = price
		}
		if bid, ok := pt.exitQuote(ctx, id); ok {
			marks[id] = bid
		}
	}
	if len(observed) == 0 && len(marks) == 0 {
		return
	}

	// Before the exit passes, so a position closed by this very tick carries the
	// extreme that closed it.
	pt.updateExcursions(observed)
	if !exitsEnabled || len(marks) == 0 {
		return
	}

	exits := pt.applyStopLosses(marks)
	for _, e := range exits {
		tag := "STOP-LOSS"
		switch e.status {
		case "TAKE_PROFIT":
			tag = "TAKE-PROFIT"
		case "TRAIL_STOP":
			tag = "TRAIL-STOP"
		}
		log.Printf("[%s] %s closed @ %.4f (entry %.4f, size %d, P/L $%.2f) — %s | persona=%s",
			tag, e.pos.ContractID, e.mark, e.pos.EntryPrice, e.pos.Size, e.yield, e.reason,
			personaOrDefault(e.pos.Persona))
		// Ledger status stays DRY_CLOSED so replayLedger recovers the close; the
		// broker-tx ref carries the trigger (same trick as FORCE_EXIT).
		_, _ = pt.ledger.AppendTransaction(e.pos.ContractID, "CLOSE", e.pos.Size, e.mark, "DRY_CLOSED", e.yield, e.status)
	}
	if len(exits) > 0 {
		pt.syncToRedis(ctx)
	}
}

// updateExcursions folds one tick's OBSERVED marks into each open position's running
// best/worst mark — the raw material for the closed trade's MFE/MAE. Observed rather
// than two-sided-live: excursion is a record of what happened to the price, and a
// one-sided book is still the price happening. Cheap: the marks are already fetched for
// the stop passes, so this adds no I/O.
func (pt *PositionTracker) updateExcursions(marks map[string]float64) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	for id, mark := range marks {
		if pos, ok := pt.positions[id]; ok {
			pos.observeMark(mark)
		}
	}
}

// applyStopLosses closes positions per the supplied LIVE marks: first any position
// past its own stop (STOP_LOSS), then — when perContractLossCloses is on — every
// live-marked position in a family whose bleed (realized + live-marked unrealized)
// is past caps.PerContractLossLimit (LOSS_LIMIT), catching bleed spread across a
// strike ladder in slices each too small to trip the per-position stop. Pure state
// transition (no I/O); returns the exits for the caller to log/persist.
func (pt *PositionTracker) applyStopLosses(marks map[string]float64) []stopExit {
	var exits []stopExit
	pt.mu.Lock()
	defer pt.mu.Unlock()

	closeLocked := func(pos *Position, mark float64, status, reason string) {
		yield := (mark - pos.EntryPrice) * float64(pos.Size)
		pt.cash += mark * float64(pos.Size)
		pt.realizedPnL += yield
		pt.recordClosedTradeLocked(pos, mark, pos.Size, "EXIT", status, 0)
		exits = append(exits, stopExit{pos: *pos, mark: mark, yield: yield, status: status, reason: reason})
		delete(pt.positions, pos.ContractID)
	}

	// Pass 1: per-position stop-loss, trailing stop, then take-profit. The stop is
	// checked first because it is the only one of the three that is a loss and so the
	// urgent case. Trail and take-profit exit at the SAME mark when both would fire, so
	// their order decides only which label the close carries — trail first, since a
	// position that has already started giving back its run is the more specific story.
	for id, mark := range marks {
		pos, ok := pt.positions[id]
		if !ok {
			continue
		}
		// A settlement-edge position is judged only by its DOLLAR risk budget: the
		// percentage limbs and the trail are all mark-based, and marks are exactly what
		// this strategy is not trying to predict (see holdsToSettlement).
		stopPct, tpPct := pt.stopLossPct, pt.takeProfitPct
		hold := pt.holdsToSettlement(pos)
		if hold {
			stopPct, tpPct = 0, 0
		}
		if hit, reason := stopLossBreached(pos.EntryPrice, mark, pos.Size, stopPct, pt.stopLossUSD, pt.stopLossMinAbs); hit {
			closeLocked(pos, mark, "STOP_LOSS", reason)
			continue
		}
		// The single mark a settlement-hold position is sold on: the one where the
		// market has already priced the edge in. Checked before the percentage limbs
		// below because for a held position those are all disabled anyway, and after
		// the stop so a dollar backstop still wins.
		if hold {
			if hit, reason := settlementTakeProfit(pos.EntryPrice, mark, pos.Size, pt.settlementHoldTPPrice); hit {
				closeLocked(pos, mark, "TAKE_PROFIT", reason)
			}
			continue
		}
		// BestMark is current: updateExcursions folds this tick's marks in before
		// this pass runs, so a mark that just set a new high can never read as a
		// give-back. Held positions never reach here — the branch above returns.
		if hit, reason := trailingStopHit(pos.EntryPrice, pos.BestMark, mark, pos.Size, pt.trailArmPct, pt.trailRetracePct, pt.stopLossMinAbs); hit {
			closeLocked(pos, mark, "TRAIL_STOP", reason)
			continue
		}
		if hit, reason := takeProfitReached(pos.EntryPrice, mark, pos.Size, tpPct, pt.takeProfitUSD); hit {
			closeLocked(pos, mark, "TAKE_PROFIT", reason)
		}
	}

	// Pass 2: family cumulative-loss limit (realized — including pass-1 closes —
	// plus live-marked unrealized; unmarked positions contribute 0 and stay open).
	if pt.perContractLossCloses && pt.caps.PerContractLossLimit > 0 {
		bleed := make(map[string]float64)
		for fam, pnl := range pt.contractRealizedPnL {
			bleed[fam] = pnl
		}
		for id, pos := range pt.positions {
			if mark, ok := marks[id]; ok {
				bleed[contractFamily(id)] += (mark - pos.EntryPrice) * float64(pos.Size)
			}
		}
		for id, pos := range pt.positions {
			mark, ok := marks[id]
			if !ok {
				continue
			}
			fam := contractFamily(id)
			if b := bleed[fam]; b <= -pt.caps.PerContractLossLimit {
				closeLocked(pos, mark,
					"LOSS_LIMIT", fmt.Sprintf("family %s bleed $%.2f past loss limit -$%.0f", fam, b, pt.caps.PerContractLossLimit))
			}
		}
	}
	return exits
}

// forceExitNearExpiry closes positions that are within forceExitWindow of resolving,
// selling at the current mark instead of letting a NO decay to $0 (or a YES that has
// already drifted against us) settle at the binary outcome. A no-op when the window
// is disabled or no live mark is available beyond the position's own entry (in which
// case the close is flat and harmless). Post-mortem #2.
func (pt *PositionTracker) forceExitNearExpiry(ctx context.Context) {
	if pt.forceExitWindow <= 0 {
		return
	}
	now := time.Now()
	cutoff := now.Add(pt.forceExitWindow)

	// 1. Identify positions resolving within the window (but not yet expired — those
	//    are left to settleExpired) under the lock; mark them off-lock.
	pt.mu.RLock()
	type cand struct {
		id    string
		entry float64
		size  int
	}
	var cands []cand
	for id, pos := range pt.positions {
		if pos.Expiry.IsZero() {
			continue
		}
		// Settlement-edge positions are exempt: force-exit exists to stop a NO decaying
		// to $0 (post-mortem #2), but for a strategy whose whole edge IS the binary
		// outcome, selling back at the mark an hour before resolution is precisely the
		// behaviour that destroys it — the 2026-08-25 desk review priced that at
		// −$428 actual vs +$2,950 held on the same entries.
		if pt.holdsToSettlement(pos) {
			continue
		}
		if now.Before(pos.Expiry) && !pos.Expiry.After(cutoff) {
			cands = append(cands, cand{id, pos.EntryPrice, pos.Size})
		}
	}
	pt.mu.RUnlock()
	if len(cands) == 0 {
		return
	}

	marks := make(map[string]float64, len(cands))
	sources := make(map[string]string, len(cands))
	for _, c := range cands {
		marks[c.id], sources[c.id] = pt.resolveMarkFor(ctx, c.id, c.entry, true)
	}

	// 2. Close under the lock at the marked price.
	type exit struct {
		pos    Position
		mark   float64
		yield  float64
		source string
	}
	var exits []exit
	pt.mu.Lock()
	for _, c := range cands {
		pos, ok := pt.positions[c.id]
		if !ok || pos.Expiry.IsZero() || !now.Before(pos.Expiry) || pos.Expiry.After(cutoff) {
			continue
		}
		mark := marks[c.id]
		yield := (mark - pos.EntryPrice) * float64(pos.Size)
		pt.cash += mark * float64(pos.Size)
		pt.realizedPnL += yield
		pt.recordClosedTradeLocked(pos, mark, pos.Size, "EXIT", "FORCE_EXIT", 0)
		exits = append(exits, exit{pos: *pos, mark: mark, yield: yield, source: sources[c.id]})
		delete(pt.positions, c.id)
	}
	pt.mu.Unlock()

	for _, e := range exits {
		ref, suffix := "FORCE_EXIT", ""
		if e.source == "haircut" {
			ref, suffix = "FORCE_EXIT_NOQUOTE", " — NO LIVE QUOTE, haircut mark"
		}
		log.Printf("[FORCE-EXIT] %s closed pre-resolution @ %.4f (entry %.4f, size %d, P/L $%.2f) — %v to expiry | persona=%s%s",
			e.pos.ContractID, e.mark, e.pos.EntryPrice, e.pos.Size, e.yield,
			time.Until(e.pos.Expiry).Round(time.Second), personaOrDefault(e.pos.Persona), suffix)
		_, _ = pt.ledger.AppendTransaction(e.pos.ContractID, "CLOSE", e.pos.Size, e.mark, "DRY_CLOSED", e.yield, ref)
	}
	if len(exits) > 0 {
		pt.syncToRedis(ctx)
	}
}

// liquidateAllPositions closes every open position at its current mark, realizing
// the outcome into cash, the trade history and the run logs. Returns the number closed.
func (pt *PositionTracker) liquidateAllPositions(ctx context.Context) int {
	// 1. Snapshot open positions under the lock; mark them off-lock.
	pt.mu.RLock()
	type cand struct {
		id    string
		entry float64
	}
	cands := make([]cand, 0, len(pt.positions))
	for id, pos := range pt.positions {
		cands = append(cands, cand{id, pos.EntryPrice})
	}
	pt.mu.RUnlock()
	if len(cands) == 0 {
		return 0
	}

	marks := make(map[string]float64, len(cands))
	sources := make(map[string]string, len(cands))
	for _, c := range cands {
		marks[c.id], sources[c.id] = pt.resolveMarkFor(ctx, c.id, c.entry, true)
	}

	// 2. Close under the lock at the marked price.
	type exit struct {
		pos    Position
		mark   float64
		yield  float64
		source string
	}
	var exits []exit
	pt.mu.Lock()
	for _, c := range cands {
		pos, ok := pt.positions[c.id]
		if !ok {
			continue
		}
		mark := marks[c.id]
		yield := (mark - pos.EntryPrice) * float64(pos.Size)
		pt.cash += mark * float64(pos.Size)
		pt.realizedPnL += yield
		pt.recordClosedTradeLocked(pos, mark, pos.Size, "EXIT", "LIQUIDATED", 0)
		exits = append(exits, exit{pos: *pos, mark: mark, yield: yield, source: sources[c.id]})
		delete(pt.positions, c.id)
	}
	pt.mu.Unlock()

	for _, e := range exits {
		ref, suffix := "LIQUIDATE_ALL", ""
		if e.source == "haircut" {
			ref, suffix = "LIQUIDATE_NOQUOTE", " — NO LIVE QUOTE, haircut mark"
		}
		log.Printf("[LIQUIDATE] %s closed @ %.4f (entry %.4f, size %d, P/L $%.2f) | persona=%s%s",
			e.pos.ContractID, e.mark, e.pos.EntryPrice, e.pos.Size, e.yield, personaOrDefault(e.pos.Persona), suffix)
		_, _ = pt.ledger.AppendTransaction(e.pos.ContractID, "CLOSE", e.pos.Size, e.mark, "DRY_CLOSED", e.yield, ref)
	}
	if len(exits) > 0 {
		pt.syncToRedis(ctx)
		// Record a final all-cash equity sample and refresh the run summary so the
		// liquidation is reflected in the run logs (and any archive taken after it).
		pt.recordEquityPoint(ctx)
	}
	return len(exits)
}

func (pt *PositionTracker) settleExpired(ctx context.Context) {
	now := time.Now()
	type settlement struct {
		pos           Position
		settleVal     float64
		isYes         bool
		yield         float64
		realData      bool
		markPrior     bool // sim settlement used the freshest mark (vs entry price)
		deterministic bool // strategy-supplied deterministic outcome (e.g. weather)
		priorValue    float64
	}

	// 1. Collect expired tickers under the lock (no I/O while holding it).
	pt.mu.RLock()
	var expired []string
	for id, pos := range pt.positions {
		if !pos.Expiry.IsZero() && !now.Before(pos.Expiry) {
			expired = append(expired, id)
		}
	}
	pt.mu.RUnlock()
	if len(expired) == 0 {
		return
	}

	// 2. Look up real Kalshi resolutions (from marketdata, if running) off-lock, and
	// (when SETTLE_PRIOR=mark) resolve the freshest mark per ticker for the sim prior.
	// Both must be off-lock: resolveMarkFor/cachedMark take pt.mu.RLock, which would
	// self-deadlock under the settlement Lock below.
	realRes := make(map[string]float64)   // ticker -> settleVal when really resolved
	priorMark := make(map[string]float64) // ticker -> freshest mark for the sim prior
	for _, t := range expired {
		if resolved, val := pt.realResolution(ctx, t); resolved {
			realRes[t] = val
			continue
		}
		if pt.settlePrior != "entry" {
			// entry passed only as the last-resort fallback inside resolveMark; a
			// live/cached mark supersedes it. forRealization=true keeps the same
			// haircut discipline the stop/force-exit paths use.
			pt.mu.RLock()
			entry := 0.0
			if pos, ok := pt.positions[t]; ok {
				entry = pos.EntryPrice
			}
			pt.mu.RUnlock()
			if mark, _ := pt.resolveMarkFor(ctx, t, entry, true); mark > 0 {
				priorMark[t] = mark
			}
		}
	}

	// 3. Settle under the lock.
	var settled []settlement
	pt.mu.Lock()
	for _, id := range expired {
		pos, ok := pt.positions[id]
		if !ok || pos.Expiry.IsZero() || now.Before(pos.Expiry) {
			continue
		}
		var settleVal float64
		realData := false
		markPrior := false
		deterministic := false
		prior := pos.EntryPrice
		if v, has := realRes[id]; has {
			settleVal = v // real Kalshi outcome
			realData = true
		} else if r := strings.ToUpper(pos.SettleResolution); r == "YES" || r == "NO" {
			// Deterministic hint from a strategy that knows the outcome from real-world
			// ground truth (the weather strategy on an already-cleared bucket). Faithful
			// where the sim otherwise has no data, so we don't coin-flip a settled result.
			if r == "YES" {
				settleVal = 1.0
			}
			deterministic = true
		} else {
			// Fallback: resolve YES with probability = the sim prior. Default (mark)
			// uses the freshest mark resolved off-lock above so a position that moved
			// away from entry settles from where the market last had it, not from a
			// stale entry price; SETTLE_PRIOR=entry restores the old implied-prob model.
			if p, has := priorMark[id]; has {
				prior = p
				markPrior = true
			}
			if rand.Float64() < prior {
				settleVal = 1.0
			}
		}
		isYes := settleVal >= 0.5
		proceeds := settleVal * float64(pos.Size)
		cost := pos.EntryPrice * float64(pos.Size)
		yield := proceeds - cost

		pt.cash += proceeds
		pt.realizedPnL += yield
		side := "NO"
		if isYes {
			side = "YES"
		}
		// Flag resolutions distinctly from sell-signal exits so post-mortems can
		// separate binary settlements ($0/$1) from discretionary closes (#12).
		pt.recordClosedTradeLocked(pos, settleVal, pos.Size, side, "RESOLVED", 0)
		settled = append(settled, settlement{pos: *pos, settleVal: settleVal, isYes: isYes, yield: yield, realData: realData, markPrior: markPrior, deterministic: deterministic, priorValue: prior})
		delete(pt.positions, id)
	}
	pt.mu.Unlock()

	for _, s := range settled {
		src := "probabilistic(entry)"
		ledgerRef := "EXPIRY_SETTLED"
		switch {
		case s.realData:
			src = "KALSHI-LIVE"
			ledgerRef = "KALSHI_SETTLED"
		case s.deterministic:
			src = "DETERMINISTIC"
			ledgerRef = "DETERMINISTIC_SETTLED"
		case s.markPrior:
			src = fmt.Sprintf("probabilistic(mark %.4f)", s.priorValue)
		}
		log.Printf("[RESOLUTION] %s resolved %s ($%.2f, %s). Entry=%.4f Size=%d P/L=$%.2f | persona=%s",
			s.pos.ContractID, map[bool]string{true: "YES", false: "NO"}[s.isYes], s.settleVal, src, s.pos.EntryPrice, s.pos.Size, s.yield, personaOrDefault(s.pos.Persona))
		// Distinct ledger status so resolutions don't blend into DRY_CLOSED exits (#12).
		_, _ = pt.ledger.AppendTransaction(s.pos.ContractID, "CLOSE", s.pos.Size, s.settleVal, "DRY_RESOLVED", s.yield, ledgerRef)
	}
	if len(settled) > 0 {
		pt.syncToRedis(ctx)
	}
}

// realResolution reads a real Kalshi market resolution from Redis (written by the
// read-only marketdata service). Returns (true, 1.0|0.0) only when the market has
// actually settled/finalized; otherwise (false, 0) and the caller falls back to
// the probabilistic-at-expiry model.
func (pt *PositionTracker) realResolution(ctx context.Context, ticker string) (bool, float64) {
	if pt.redisClient == nil {
		return false, 0
	}
	val, err := pt.redisClient.Get(ctx, "market:"+ticker).Result()
	if err != nil {
		return false, 0
	}
	var m struct {
		Status string `json:"status"`
		Result string `json:"result"`
	}
	if json.Unmarshal([]byte(val), &m) != nil {
		return false, 0
	}
	st := strings.ToLower(m.Status)
	res := strings.ToLower(m.Result)
	if st == "settled" || st == "finalized" {
		if res == "yes" {
			return true, 1.0
		}
		if res == "no" {
			return true, 0.0
		}
	}
	return false, 0
}

// runEquityRecorder periodically appends an equity-curve sample to Redis.
func (pt *PositionTracker) runEquityRecorder(ctx context.Context, interval time.Duration) {
	if pt.redisClient == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("[INFO] Equity recorder active (interval %v).", interval)

	// Record an initial point immediately.
	pt.recordEquityPoint(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pt.recordEquityPoint(ctx)
		}
	}
}

func (pt *PositionTracker) recordEquityPoint(ctx context.Context) {
	acct := pt.snapshotAccount(ctx)
	pt.syncAccountToRedis(ctx)

	point := EquityPoint{
		TimestampMs:   time.Now().UnixMilli(),
		NAV:           acct.NAV,
		Cash:          acct.Cash,
		RealizedPnL:   acct.RealizedPnL,
		UnrealizedPnL: acct.UnrealizedPnL,
		OpenPositions: acct.OpenPositions,
		DrawdownPct:   acct.DrawdownPct,
	}
	if pt.benchmark != nil {
		if bnav, ok := pt.benchmark.BenchmarkNAV(); ok {
			point.BenchmarkNAV = math.Round(bnav*100) / 100
		}
	}

	// Run-log export (CSV/JSONL/summary).
	if pt.exporter != nil {
		pt.mu.RLock()
		trades := make([]ClosedTrade, len(pt.closedTrades))
		copy(trades, pt.closedTrades)
		pt.mu.RUnlock()
		pt.exporter.AppendEquity(point, acct, trades)
	}

	if pt.redisClient == nil {
		return
	}
	data, err := json.Marshal(point)
	if err != nil {
		return
	}
	_ = pt.redisClient.RPush(ctx, equityCurveKey, string(data)).Err()
	_ = pt.redisClient.LTrim(ctx, equityCurveKey, -maxEquityPoints, -1).Err()
}

// resetRun clears the account, positions, trade history and equity series back to $100k.
// Open positions are liquidated at their current marks first so their outcomes are
// realized into the trade history and archived run logs rather than discarded.
// A non-empty note is preserved in the archived run directory (run_note.txt).
func (pt *PositionTracker) resetRun(ctx context.Context, note string) {
	if n := pt.liquidateAllPositions(ctx); n > 0 {
		log.Printf("[INFO] Reset: liquidated %d open position(s) at current marks.", n)
	}

	pt.mu.Lock()
	pt.positions = make(map[string]*Position)
	pt.cash = pt.startingCapital
	pt.realizedPnL = 0
	pt.peakNAV = pt.startingCapital
	pt.lastNAV = pt.startingCapital
	pt.maxDrawdownPct = 0
	pt.closedTrades = make([]ClosedTrade, 0)
	pt.contractRealizedPnL = map[string]float64{}
	pt.lastExit = map[string]time.Time{}
	pt.categoryRealizedPnL = map[string]float64{}
	pt.markCache = map[string]markEntry{}
	pt.mu.Unlock()

	if pt.redisClient != nil {
		_ = pt.redisClient.Del(ctx, equityCurveKey).Err()
		_ = pt.redisClient.Del(ctx, tradesKey).Err()
	}
	// Re-anchor the buy-and-hold benchmark so the new run compares from now.
	if pt.benchmark != nil {
		pt.benchmark.Reset(ctx, pt.startingCapital)
	}
	if err := pt.ledger.Reset(); err != nil {
		log.Printf("[WARNING] Failed to reset ledger on run reset: %v", err)
	}
	// Always append a reset-boundary marker: if the truncation above failed, replay
	// (recovery.go) must not walk blocks from the previous run — that once rebuilt
	// cash=−$283k. On a successfully truncated ledger this is simply the new genesis.
	if _, err := pt.ledger.AppendTransaction("RUN_RESET", "RESET", 0, 0, statusRunReset, 0, note); err != nil {
		log.Printf("[WARNING] Failed to append run-reset marker to ledger: %v", err)
	}
	if pt.exporter != nil {
		pt.exporter.Archive(note)
	}
	log.Printf("[INFO] Paper run reset. Cash restored to $%.2f.", pt.startingCapital)
	pt.syncToRedis(ctx)
	pt.recordEquityPoint(ctx)
}

// reloadFromRedis restores account state after a restart so an in-flight run continues.
func (pt *PositionTracker) reloadFromRedis(ctx context.Context) {
	if pt.redisClient == nil {
		return
	}

	// Account scalars.
	if val, err := pt.redisClient.Get(ctx, accountKey).Result(); err == nil {
		var acct AccountState
		if json.Unmarshal([]byte(val), &acct) == nil && acct.StartingCapital > 0 {
			pt.mu.Lock()
			pt.cash = acct.Cash
			pt.realizedPnL = acct.RealizedPnL
			pt.startingCapital = acct.StartingCapital
			if acct.PeakNAV > 0 {
				pt.peakNAV = acct.PeakNAV
			}
			if acct.NAV > 0 {
				pt.lastNAV = acct.NAV
			}
			if acct.MaxDrawdownPct > pt.maxDrawdownPct {
				pt.maxDrawdownPct = acct.MaxDrawdownPct
			}
			pt.mu.Unlock()
		}
	}

	// Open positions (with expiries).
	if val, err := pt.redisClient.Get(ctx, "gekko:positions").Result(); err == nil {
		var posList []*Position
		if json.Unmarshal([]byte(val), &posList) == nil {
			pt.mu.Lock()
			for _, p := range posList {
				if p != nil && p.ContractID != "" {
					pt.positions[p.ContractID] = p
				}
			}
			pt.mu.Unlock()
		}
	}

	// Closed-trade history.
	if val, err := pt.redisClient.Get(ctx, tradesKey).Result(); err == nil {
		var trades []ClosedTrade
		if json.Unmarshal([]byte(val), &trades) == nil {
			pt.mu.Lock()
			pt.closedTrades = trades
			// Rebuild per-family guard state so the loss limit and re-entry cooldown
			// survive a restart instead of resetting mid-run.
			pt.contractRealizedPnL = map[string]float64{}
			pt.lastExit = map[string]time.Time{}
			pt.categoryRealizedPnL = map[string]float64{}
			for _, t := range trades {
				fam := contractFamily(t.ContractID)
				pt.contractRealizedPnL[fam] += t.RealizedPnL
				if ts := time.UnixMilli(t.ClosedAtMs); ts.After(pt.lastExit[fam]) {
					pt.lastExit[fam] = ts
				}
				if cat := contractCategory(t.ContractID); cat != "" {
					pt.categoryRealizedPnL[cat] += t.RealizedPnL
				}
			}
			pt.mu.Unlock()
		}
	}

	pt.mu.RLock()
	n := len(pt.positions)
	cash := pt.cash
	pt.mu.RUnlock()
	if n > 0 || cash != pt.startingCapital {
		log.Printf("[INFO] Reloaded paper run from Redis: cash=$%.2f, %d open positions.", cash, n)
	}
}
