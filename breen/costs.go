package main

import (
	"fmt"
	"math"
	"strings"
)

// Execution-cost accounting (breen-intelligence Phase 0). Two costs were invisible
// in the ledger, and they are NOT the same kind of thing:
//
//   - Half-spread is already paid. A dry BUY fills at the ask and marks at the mid,
//     so the cost is inside RealizedPnL from the first tick — it just never had a
//     name. Recording it DECOMPOSES the P&L ("how much of this was the round trip");
//     subtracting it anywhere would double-count.
//   - Venue fees were never charged at all. Kalshi's fee is real money the paper
//     account has been ignoring, so it is a genuine missing debit — recorded always,
//     debited from cash only under KALSHI_FEE_CHARGE.
//
// Every expectancy estimate downstream (the allocator, Lou, Phase 2's sizing) is
// biased by whatever is uncounted here, which is why this lands before anything
// learns from the numbers.

const (
	// defaultKalshiFeeRate is the coefficient in Kalshi's published trading-fee
	// formula, fee = ceil(rate × C × P × (1 − P)).
	defaultKalshiFeeRate = 0.07
)

// halfSpreadPerContract is the per-contract cost of crossing the given book: the
// distance from the mid to the side actually taken. Zero when the book is not
// two-sided (nothing to cross — the fill fell back to the proposal price) or when
// the quote is crossed/degenerate.
func halfSpreadPerContract(ob *OrderBook) float64 {
	if ob == nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return 0
	}
	bid, ask := ob.Bids[0].Price, ob.Asks[0].Price
	if ask <= bid {
		return 0
	}
	return (ask - bid) / 2.0
}

// kalshiFee returns the modelled venue fee in dollars for one leg (open or close) of
// `size` contracts at `price`, following Kalshi's published formula
// fee = ceil(rate × C × P × (1 − P)) rounded UP to the next cent. The fee peaks at
// coin-flip prices and vanishes at the tails, which is exactly the shape that matters
// here: the mid-band trades the guards already distrust are also the expensive ones.
//
// Returns 0 for non-Kalshi venues (Polymarket's fee schedule is different and the run
// is Kalshi-only today), for a disabled model, and for degenerate inputs. Pure.
func kalshiFee(contractID string, price float64, size int, rate float64, enabled bool) float64 {
	if !enabled || rate <= 0 || size <= 0 || price <= 0 || price >= 1 {
		return 0
	}
	if !strings.EqualFold(inferExchange(contractID), "kalshi") {
		return 0
	}
	raw := rate * float64(size) * price * (1 - price)
	// Round up to the next cent, with an epsilon so binary float noise can't do it for
	// us: 0.07×100×0.5×0.5 evaluates to 1.7500000000000002, and a bare ceiling would
	// bill that exact-cent fee as $1.76.
	const eps = 1e-9
	return math.Ceil(raw*100-eps) / 100
}

// roundTripFriction is what one full round trip on this contract costs, per contract:
// the spread crossed twice (in at the ask, out at the bid) plus the modelled venue fee
// on both legs. The exit leg's fee is priced at the CURRENT ask rather than an unknown
// future exit price — fees peak at 0.5 and this is the fee the position would pay if it
// closed right now, which is the honest estimate available at entry.
//
// This is the hurdle a signal has to clear before it is worth trading at all. On the
// review's median 23c contract it is ~2.5c of fee plus 2–4c of spread against a 5c
// MIN_EDGE: friction eats most of the minimum edge gordon requires, and nothing was
// checking it. Pure.
func roundTripFriction(contractID string, bid, ask float64, feeRate float64, feeEnabled bool) float64 {
	spread := 0.0
	if bid > 0 && ask > bid {
		spread = ask - bid
	}
	// kalshiFee is priced per lot; ask for one contract to get the per-contract cost.
	fees := kalshiFee(contractID, ask, 1, feeRate, feeEnabled) * 2
	return spread + fees
}

// entryEdgeBlock refuses a BUY whose modelled edge at the ACTUAL fill no longer clears a
// multiple of the round-trip friction. Gordon checks edge against the book it saw
// seconds earlier (requiredEdge); this is the executive-side re-check against the book
// breen is about to fill on, and it prices the FEE too, which gordon never did.
//
// Returns a non-empty reason to refuse. Inert when the multiple is 0, when the strategy
// supplied no fair value, or when there is no two-sided book to measure against.
func entryEdgeBlock(contractID string, fair float64, ob *OrderBook, multiple, feeRate float64, feeEnabled bool) string {
	if multiple <= 0 || fair <= 0 || ob == nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return ""
	}
	bid, ask := ob.Bids[0].Price, ob.Asks[0].Price
	edge := fair - ask
	friction := roundTripFriction(contractID, bid, ask, feeRate, feeEnabled)
	need := multiple * friction
	const eps = 1e-9
	if edge < need-eps {
		return fmt.Sprintf("edge %+.3f (fair %.3f vs ask %.3f) below %.1f× round-trip friction %.3f = %.3f — the vig eats it",
			edge, fair, ask, multiple, friction, need)
	}
	return ""
}

// tradeCosts is the cost decomposition of one closed trade: the half-spread crossed
// (entry side, plus the exit side when the exit filled off a book) and the modelled
// fee on both legs.
type tradeCosts struct {
	SpreadUSD float64
	FeeUSD    float64
}

// closingFraction is the share of a position's accumulated entry-side costs that
// belongs to a close of `size` contracts. A full (or over-) close takes all of it.
func closingFraction(pos *Position, size int) float64 {
	if pos == nil || pos.Size <= 0 || size <= 0 || size >= pos.Size {
		return 1.0
	}
	return float64(size) / float64(pos.Size)
}

// closeCosts apportions a position's accumulated entry-side costs to the fraction of
// it that is closing, and adds this close's own costs: the exit half-spread actually
// crossed (0 for exits marked at the mid or settled to a binary payout) and the exit
// leg's modelled fee.
func closeCosts(pos *Position, exitPrice float64, size int, exitSpreadPerContract, feeRate float64, feeEnabled bool) tradeCosts {
	frac := closingFraction(pos, size)
	c := tradeCosts{
		SpreadUSD: pos.EntrySpreadCostUSD*frac + exitSpreadPerContract*float64(size),
		FeeUSD:    pos.EntryFeeUSD*frac + kalshiFee(pos.ContractID, exitPrice, size, feeRate, feeEnabled),
	}
	c.SpreadUSD = math.Round(c.SpreadUSD*100) / 100
	c.FeeUSD = math.Round(c.FeeUSD*100) / 100
	return c
}

// applyFeeToPersonaScores nets a trade's round-trip fee across the personas that
// authored it, in proportion to the capital each deployed — fees scale with size × price,
// so capital share is the right split rather than an even one or a P&L-weighted one.
// Each persona's ReturnPct is recomputed from its netted P&L so the allocator, which
// learns from ReturnPct rather than dollars, sees the true number too. Mutates in place.
func applyFeeToPersonaScores(scores []PersonaScore, feeUSD float64) {
	var totalCost float64
	for _, s := range scores {
		totalCost += math.Abs(s.Cost)
	}
	if totalCost <= 0 || feeUSD <= 0 {
		return
	}
	for i := range scores {
		share := math.Abs(scores[i].Cost) / totalCost
		scores[i].RealizedPnL = math.Round((scores[i].RealizedPnL-feeUSD*share)*100) / 100
		if scores[i].Cost != 0 {
			scores[i].ReturnPct = math.Round(scores[i].RealizedPnL/math.Abs(scores[i].Cost)*10000) / 100
		}
	}
}

// consumeEntryCosts draws down the entry-side costs a close has just been billed for,
// leaving the remainder with the still-open contracts. Without it a position closed in
// two slices would bill the same fill's spread and fee on both.
func consumeEntryCosts(pos *Position, size int) {
	if pos == nil {
		return
	}
	remain := 1.0 - closingFraction(pos, size)
	pos.EntrySpreadCostUSD *= remain
	pos.EntryFeeUSD *= remain
}
