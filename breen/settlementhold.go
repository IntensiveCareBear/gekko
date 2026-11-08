package main

import (
	"fmt"
	"strings"
)

// Settlement-hold policy (2026-08-25 desk review, fix #2).
//
// Some strategies have no opinion about the mark — their edge is realized only when
// the contract resolves. The weather gordon is the clear case: it buys a bucket
// because the day's high is likely to clear a threshold, and whether the book wobbles
// at 23¢ or 19¢ in the meantime says nothing about whether that will happen.
//
// Breen was managing those positions like a day trader: a 15%-of-entry mark stop
// (3.5¢ on a 23¢ contract — inside the bid-ask bounce), a 40% take-profit, and a
// force-exit an hour before resolution. The review re-scored 284 KXHIGH trades against
// the realized station highs: **−$428 as actually traded, +$2,950 held to settlement**
// on the same entries, and still +$878 with the five best excluded. The entry model was
// right more often than the market priced, and the risk layer sold the winners back
// before they could pay.
//
// So for a listed persona:
//
//   - the PERCENTAGE limbs (stop-loss %, take-profit %) and the trailing stop are
//     skipped — every one of them is a mark-based judgement;
//   - the DOLLAR limbs (STOP_LOSS_USD, TAKE_PROFIT_USD) and the family loss limit
//     still apply — they are a real risk budget rather than a view on the mark;
//   - force-exit-before-expiry is skipped, so the position reaches resolution.
//
// The binary itself is the stop: the position is sized to survive a 100% loss, which is
// what the exposure caps already enforce. This is deliberately NOT "no risk control on
// weather" — it is "risk control in dollars, not in ticks".
//
// Empty set (the default) disables the whole policy and every position is managed
// exactly as before.

// parsePersonaSet turns a comma-separated env value into a persona lookup set,
// lowercased and trimmed. Empty entries are dropped, so "weather," and " weather " both
// mean the same thing as "weather". Shared by every persona-list knob
// (SETTLEMENT_HOLD_PERSONAS, SHADOW_PERSONAS).
func parsePersonaSet(csv string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(csv, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out[p] = true
		}
	}
	return out
}

// holdsToSettlement reports whether a position belongs to a settlement-edge strategy
// and should therefore be exempt from the mark-based exits.
//
// The check runs over the CREDITED personas, not pos.Persona alone: with consensus
// enabled every order is stamped Persona="consensus" and carries the real author in
// Contributors, so matching on Persona alone would never fire in production — the live
// weather fills log as `persona=consensus … [weather(w1.14×c0.64)]`.
//
// Every credited persona must be in the hold set. A consensus order that mixes a
// settlement strategy with a mark-trading one is not a pure settlement play, and the
// conservative reading — keep managing it — is the pre-existing behaviour. In practice
// the fleet skips the weather series entirely (GORDON_SKIP_SERIES), so weather orders
// are single-contributor and this is not a live distinction.
func (pt *PositionTracker) holdsToSettlement(pos *Position) bool {
	if pos == nil || len(pt.settlementHoldPersonas) == 0 {
		return false
	}
	credited := creditedPersonas(pos.Persona, pos.Contributors)
	if len(credited) == 0 {
		return false
	}
	for _, p := range credited {
		if !pt.settlementHoldPersonas[strings.ToLower(p)] {
			return false
		}
	}
	return true
}

// settlementTakeProfit reports whether a settlement-hold position should be banked
// anyway because the market has already paid out its edge (2026-09-03 forensics).
//
// The hold policy above answers "is this mark worth reacting to?" and its answer is
// almost always no — a 23¢ contract wobbling to 19¢ says nothing about tomorrow's
// station high. But there is one mark that is not noise: the one where the market has
// come all the way over to our side. At a 95¢ bid the contract is priced as near
// certain, which leaves 5¢ of upside against 95¢ of downside. Holding for that last
// nickel is not collecting a settlement edge; it is writing a very short-odds option
// on an outcome we have already been paid for.
//
// It is an ABSOLUTE price, deliberately, and that is what makes it compatible with the
// hold policy rather than a reversal of it. A percentage limb asks how far the position
// has travelled from OUR entry, which is a fact about us; this asks where the contract
// is trading, which is a fact about the market. The 2026-08-25 desk review disabled the
// former for exactly the right reason and this does not re-enable it.
//
// The forensics: on the 38 weather contracts resolved between 08-26 and 09-02, ten
// reached a mark of 0.97 or better and SIX of those ten settled at $0.00 — daily-high
// buckets that the temperature sat inside all afternoon before climbing one more degree
// into the next band. Banking at 0.95 turns those 38 trades from +$96.63 to +$467.30.
// The four genuine winners give up $10.46 between them, because a contract that reaches
// 0.99 pays 1.00 and the nickel is the whole cost; the six zeros hand back $381.13.
//
// mark must exceed the entry basis: without that, a position opened at 0.97 would be
// "taken profit" at a 0.95 loss the moment it was marked. The guard costs nothing on
// the real cases — the worst of the six, KXHIGHCHI-26AUG30-T86, was entered at 0.890
// and still clears a 0.95 threshold comfortably.
func settlementTakeProfit(entry, mark float64, size int, tpPrice float64) (bool, string) {
	if tpPrice <= 0 || mark < tpPrice || mark <= entry {
		return false, ""
	}
	return true, fmt.Sprintf("bid %.2f ≥ settlement take-profit %.2f (entry %.3f, +$%.2f) — the market has already priced the edge in",
		mark, tpPrice, entry, (mark-entry)*float64(size))
}

// isBlacklistedFamily reports whether new entries on this contract family are refused.
// Compared case-insensitively: Lou writes the names lowercased into overrides.env while
// contractFamily() yields the ticker's own casing.
func (pt *PositionTracker) isBlacklistedFamily(family string) bool {
	if len(pt.familyBlacklist) == 0 {
		return false
	}
	return pt.familyBlacklist[strings.ToLower(family)]
}
