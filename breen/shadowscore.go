package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"
)

// Shadow scoring (2026-08-30) — the follow-on job shadow.go describes.
//
// Shadow mode suspended the LLM personas behind a written re-activation bar: positive
// net-of-fee expectancy over 200 shadow proposals. Nothing computed that number, so the
// bar was undecidable and the suspension was permanent in practice — the personas could
// not earn their way back no matter what they proposed.
//
// Two things were in the way. The first is arithmetic: the shadow book records what a
// persona WOULD have traded, and scoring it means finding out what that trade would have
// been worth. This file does that against real settlements (a contract that resolved is
// final evidence) and, for still-open contracts, a provisional mark.
//
// The second is the denominator. Of the first 55 recorded proposals, 33 were bearish
// signal-only emissions — sizeless, priceless conviction that a market falls, which
// gordon emits when a persona wants to sell a family it does not hold. Those are real
// signals but they are not trades: they have no entry, so they have no expectancy, and
// counting them toward "200 proposals" would let a persona reach the bar without ever
// proposing something scoreable. The bar counts ACTIONABLE proposals only. Signals are
// still recorded and still reported, just under their own heading.
//
// The scorer never re-activates anything by itself. It publishes a scorecard and says
// whether the bar is met; taking a persona out of SHADOW_PERSONAS stays a human act.

const (
	shadowSettledKey   = "gekko:shadow_settled"   // fingerprint -> frozen ShadowScore
	shadowScorecardKey = "gekko:shadow_scorecard" // latest published scorecard
)

// shadowOutcome is how much a scored proposal's exit value can be trusted.
const (
	shadowSettled = "settled" // the contract resolved — final, frozen
	shadowMarked  = "marked"  // still open — provisional mark-to-market
	shadowPending = "pending" // no outcome observable yet
)

// ShadowScore is one shadow proposal carried through to a P&L.
type ShadowScore struct {
	Fingerprint string   `json:"fingerprint"`
	ContractID  string   `json:"contract_id"`
	Personas    []string `json:"personas"`
	EntryPrice  float64  `json:"entry_price"`
	Size        int      `json:"size"`
	ExitValue   float64  `json:"exit_value"`
	NotionalUSD float64  `json:"notional_usd"`
	NetPnLUSD   float64  `json:"net_pnl_usd"` // net of modelled round-trip fees
	ReturnPct   float64  `json:"return_pct"`
	Outcome     string   `json:"outcome"`
	ProposedAt  int64    `json:"proposed_at_ms"`
	ScoredAt    int64    `json:"scored_at_ms"`
}

// ShadowPersonaScore is one persona's slice of the scorecard.
type ShadowPersonaScore struct {
	Persona       string  `json:"persona"`
	Actionable    int     `json:"actionable"`
	Signals       int     `json:"signals"`
	Scored        int     `json:"scored"`
	Wins          int     `json:"wins"`
	NetPnLUSD     float64 `json:"net_pnl_usd"`
	NotionalUSD   float64 `json:"notional_usd"`
	ExpectancyPct float64 `json:"expectancy_pct"`
	BarMet        bool    `json:"bar_met"`
}

// ShadowFamilyScore is one contract series' slice of the scorecard. Published because
// the 2026-09-03 forensics found the whole book's edge living in a single one.
type ShadowFamilyScore struct {
	Family        string  `json:"family"`
	Scored        int     `json:"scored"`
	Contracts     int     `json:"contracts"`
	Wins          int     `json:"wins"`
	NetPnLUSD     float64 `json:"net_pnl_usd"`
	NotionalUSD   float64 `json:"notional_usd"`
	ExpectancyPct float64 `json:"expectancy_pct"`
}

// ShadowScorecard is the published verdict on the shadow book.
type ShadowScorecard struct {
	GeneratedAt   int64                         `json:"generated_at_ms"`
	Recorded      int                           `json:"recorded"`   // everything in the book
	Actionable    int                           `json:"actionable"` // would-be trades: the bar's denominator
	Signals       int                           `json:"signals"`    // bearish signal-only, not scoreable
	Scored        int                           `json:"scored"`
	SettledCount  int                           `json:"settled"`
	MarkedCount   int                           `json:"marked"`
	PendingCount  int                           `json:"pending"`
	Wins          int                           `json:"wins"`
	WinRatePct    float64                       `json:"win_rate_pct"`
	NetPnLUSD     float64                       `json:"net_pnl_usd"`
	NotionalUSD   float64                       `json:"notional_usd"`
	ExpectancyPct float64                       `json:"expectancy_pct"`
	BarThreshold  int                           `json:"bar_threshold"`
	BarMet        bool                          `json:"bar_met"`
	Verdict       string                        `json:"verdict"`
	PerPersona    map[string]ShadowPersonaScore `json:"per_persona"`

	// Breadth (2026-09-03). The bar counted proposals, but evidence lives in
	// CONTRACTS: on 09-03 the book's 118 actionable proposals spanned 27 distinct
	// contracts, 65 of them one weekly Brent series, and the fleet was on course to
	// re-activate on a single directional oil move counted sixty-five times.
	Contracts       int                 `json:"contracts"`         // distinct scored contracts
	MinContracts    int                 `json:"min_contracts"`     // breadth the bar requires
	TopFamily       string              `json:"top_family"`        // largest net-P&L contributor
	ExpectancyExTop float64             `json:"expectancy_ex_top"` // expectancy with that family removed
	PerFamily       []ShadowFamilyScore `json:"per_family"`        // sorted by |net P&L|, descending
}

// actionable reports whether a recorded proposal is a would-be TRADE — something with an
// entry price and a size, which can therefore be carried to a P&L. Bearish signals are
// not: gordon emits them sizeless because they are never orders.
func (sp ShadowProposal) actionable() bool {
	return !sp.Bearish && strings.EqualFold(sp.Action, "BUY") && sp.Size > 0 && sp.Price > 0
}

// shadowFingerprint is a proposal's stable identity, so a settled verdict can be frozen
// once and matched back on later passes. Contract + author + proposal time: the same
// persona re-proposing the same contract on a later tick is a different proposal.
func shadowFingerprint(sp ShadowProposal) string {
	return fmt.Sprintf("%s|%s|%d", sp.ContractID, strings.Join(sp.Personas, "+"), sp.ProposedAt)
}

// scoreShadowProposal carries one would-be trade to a net P&L at the given exit value.
//
// Fees are charged on both legs, the same model a real fill pays: the entry fee it would
// have paid to get in, and the exit fee to get out. A settlement exit pays no exit fee —
// kalshiFee is zero at 0 and 1 — which is correct for Kalshi, where settlement is free
// and only trading is billed. Pure.
func scoreShadowProposal(sp ShadowProposal, exitValue, feeRate float64, feeEnabled bool) (netPnL, notional float64) {
	notional = sp.Price * float64(sp.Size)
	gross := (exitValue - sp.Price) * float64(sp.Size)
	fees := kalshiFee(sp.ContractID, sp.Price, sp.Size, feeRate, feeEnabled) +
		kalshiFee(sp.ContractID, exitValue, sp.Size, feeRate, feeEnabled)
	return gross - fees, notional
}

// shadowSeries is a contract's SERIES root — the concentration unit the bar cares
// about. Deliberately coarser than contractFamily(), which keeps the event date and so
// would read KXBRENTW-26SEP0417 and KXBRENTW-26SEP1117 as unrelated evidence when they
// are the same weekly oil trade re-expressed. Everything before the first dash.
func shadowSeries(contractID string) string {
	if i := strings.Index(contractID, "-"); i > 0 {
		return contractID[:i]
	}
	return contractID
}

// buildShadowScorecard aggregates scored rows into the published verdict. Pure, so the
// bar's arithmetic is testable without Redis, a clock, or a market.
//
// Expectancy is net P&L over deployed notional rather than a mean of per-trade returns:
// a persona should not be able to reach a positive number by being right on tiny
// proposals and wrong on large ones.
//
// minContracts is the breadth half of the bar (2026-09-03). A count of proposals is not
// a count of evidence: the same persona re-proposing the same contract every tick as a
// position moves its way produces a hundred rows that all resolve on one outcome. The
// bar therefore also requires the scored rows to span minContracts distinct contracts,
// and — separately — to still show positive expectancy once the single best-performing
// SERIES is removed. That second test is the 2026-08-25 desk review's own instinct
// ("still +$878 with the five best excluded") turned into a rule.
func buildShadowScorecard(rows []ShadowScore, recorded, signals int, perPersonaCounts map[string]struct{ Actionable, Signals int }, threshold, minContracts int) ShadowScorecard {
	sc := ShadowScorecard{
		GeneratedAt:  time.Now().UnixMilli(),
		Recorded:     recorded,
		Signals:      signals,
		BarThreshold: threshold,
		MinContracts: minContracts,
		PerPersona:   map[string]ShadowPersonaScore{},
	}
	for name, c := range perPersonaCounts {
		sc.PerPersona[name] = ShadowPersonaScore{Persona: name, Actionable: c.Actionable, Signals: c.Signals}
	}
	fams := map[string]*ShadowFamilyScore{}
	famContracts := map[string]map[string]bool{}
	contracts := map[string]bool{}
	for _, r := range rows {
		switch r.Outcome {
		case shadowPending:
			sc.PendingCount++
			continue
		case shadowSettled:
			sc.SettledCount++
		case shadowMarked:
			sc.MarkedCount++
		}
		sc.Scored++
		sc.NetPnLUSD += r.NetPnLUSD
		sc.NotionalUSD += r.NotionalUSD
		won := r.NetPnLUSD > 0
		if won {
			sc.Wins++
		}
		contracts[r.ContractID] = true
		series := shadowSeries(r.ContractID)
		f, ok := fams[series]
		if !ok {
			f = &ShadowFamilyScore{Family: series}
			fams[series] = f
			famContracts[series] = map[string]bool{}
		}
		f.Scored++
		f.NetPnLUSD += r.NetPnLUSD
		f.NotionalUSD += r.NotionalUSD
		if won {
			f.Wins++
		}
		famContracts[series][r.ContractID] = true
		for _, name := range r.Personas {
			p := sc.PerPersona[name]
			p.Persona = name
			p.Scored++
			p.NetPnLUSD += r.NetPnLUSD
			p.NotionalUSD += r.NotionalUSD
			if won {
				p.Wins++
			}
			sc.PerPersona[name] = p
		}
	}
	sc.Actionable = len(rows)
	sc.Contracts = len(contracts)
	if sc.NotionalUSD > 0 {
		sc.ExpectancyPct = round2(sc.NetPnLUSD / sc.NotionalUSD * 100)
	}

	// Per-family, ordered by how much of the book each one moves — the top row is the
	// concentration story, so it should never need looking for.
	for series, f := range fams {
		f.Contracts = len(famContracts[series])
		if f.NotionalUSD > 0 {
			f.ExpectancyPct = round2(f.NetPnLUSD / f.NotionalUSD * 100)
		}
		sc.PerFamily = append(sc.PerFamily, *f)
	}
	sort.Slice(sc.PerFamily, func(i, j int) bool {
		a, b := math.Abs(sc.PerFamily[i].NetPnLUSD), math.Abs(sc.PerFamily[j].NetPnLUSD)
		if a != b {
			return a > b
		}
		return sc.PerFamily[i].Family < sc.PerFamily[j].Family
	})

	// Expectancy with the single biggest NET CONTRIBUTOR removed. Biggest by net P&L,
	// not by |net P&L|: the question this answers is "is the book still good news
	// without its best story", so the family to strike out is the one carrying the
	// good news. A book with only one family has nothing left to judge, which is
	// itself a failure of breadth — leave it at zero and let MinContracts speak.
	if len(sc.PerFamily) > 1 {
		top := ""
		best := 0.0
		for _, f := range sc.PerFamily {
			if f.NetPnLUSD > best {
				best, top = f.NetPnLUSD, f.Family
			}
		}
		if top != "" {
			sc.TopFamily = top
			netEx, notionalEx := 0.0, 0.0
			for _, f := range sc.PerFamily {
				if f.Family == top {
					continue
				}
				netEx += f.NetPnLUSD
				notionalEx += f.NotionalUSD
			}
			if notionalEx > 0 {
				sc.ExpectancyExTop = round2(netEx / notionalEx * 100)
			}
		}
	}
	if sc.Scored > 0 {
		sc.WinRatePct = round2(float64(sc.Wins) / float64(sc.Scored) * 100)
	}
	for name, p := range sc.PerPersona {
		if p.NotionalUSD > 0 {
			p.ExpectancyPct = round2(p.NetPnLUSD / p.NotionalUSD * 100)
		}
		p.BarMet = p.Scored >= threshold && p.ExpectancyPct > 0
		sc.PerPersona[name] = p
	}

	// The bar. Volume, then edge, then breadth, then that the edge is not one story:
	// every clause, or it is not met.
	//
	// Both breadth clauses hang off minContracts, so SHADOW_BAR_MIN_CONTRACTS=0 restores
	// the pre-2026-09-03 count-and-edge bar exactly. They are one policy — "volume is
	// not evidence" — and an operator turning it off should get all of it off, not a
	// half-applied bar whose failures are hard to attribute.
	breadthOn := minContracts > 0
	broad := !breadthOn || sc.Contracts >= minContracts
	robust := !breadthOn || sc.TopFamily == "" || sc.ExpectancyExTop > 0
	sc.BarMet = sc.Scored >= threshold && sc.ExpectancyPct > 0 && broad && robust
	switch {
	case sc.BarMet:
		sc.Verdict = fmt.Sprintf("BAR MET: %+.2f%% net-of-fee expectancy over %d scored proposals across %d contracts, and still %+.2f%% without %s — re-activation is now a judgement call, not a blocked one",
			sc.ExpectancyPct, sc.Scored, sc.Contracts, sc.ExpectancyExTop, sc.TopFamily)
	case sc.Scored >= threshold && sc.ExpectancyPct > 0 && !broad:
		sc.Verdict = fmt.Sprintf("BAR NOT MET: %+.2f%% over %d proposals, but they span only %d distinct contracts (need %d) — that is one trade re-counted, not %d pieces of evidence",
			sc.ExpectancyPct, sc.Scored, sc.Contracts, minContracts, sc.Scored)
	case sc.Scored >= threshold && sc.ExpectancyPct > 0 && !robust:
		sc.Verdict = fmt.Sprintf("BAR NOT MET: %+.2f%% overall, but %s carries it — strip that one series out and the book is %+.2f%%; the fleet would be re-activated on a single crowded trade",
			sc.ExpectancyPct, sc.TopFamily, sc.ExpectancyExTop)
	case sc.Scored < threshold:
		// Marked rows are entry-priced at what the persona proposed (at or near the ask)
		// and valued at the mid, so a freshly recorded proposal starts a half-spread
		// underwater by construction — the same "marks straight down to mid" the entry
		// risk gate refuses fills over. That cost is real and a real fill would pay it,
		// but it resolves as contracts settle, so a scorecard with no settled evidence
		// yet reads pessimistically. Say so rather than letting the number stand alone.
		sc.Verdict = fmt.Sprintf("insufficient evidence: %d/%d scored proposals (%+.2f%% so far)",
			sc.Scored, threshold, sc.ExpectancyPct)
		if sc.SettledCount == 0 && sc.MarkedCount > 0 {
			sc.Verdict += " — all provisional marks, no settled outcomes yet; expect this to read low until contracts resolve"
		}
	default:
		sc.Verdict = fmt.Sprintf("BAR NOT MET: %+.2f%% net-of-fee expectancy over %d scored proposals — stays shadowed",
			sc.ExpectancyPct, sc.Scored)
	}
	return sc
}

// loadFrozenShadowScores reads the settled verdicts recorded on earlier passes.
//
// Freezing matters because the evidence is perishable: once a contract settles, its
// market: key eventually expires and its book disappears, so a verdict that is not
// captured at the time is lost. Provisional marks are deliberately NOT frozen — they
// change until the contract resolves.
func (pt *PositionTracker) loadFrozenShadowScores(ctx context.Context) map[string]ShadowScore {
	out := map[string]ShadowScore{}
	if pt.redisClient == nil {
		return out
	}
	raw, err := pt.redisClient.HGetAll(ctx, shadowSettledKey).Result()
	if err != nil {
		return out
	}
	for fp, v := range raw {
		var s ShadowScore
		if json.Unmarshal([]byte(v), &s) == nil {
			out[fp] = s
		}
	}
	return out
}

// scoreShadowBook walks the recorded shadow book and produces the current scorecard,
// freezing any verdict that has become final.
func (pt *PositionTracker) scoreShadowBook(ctx context.Context) ShadowScorecard {
	proposals := pt.shadowProposalsSnapshot(ctx, maxShadowProposals)
	frozen := pt.loadFrozenShadowScores(ctx)

	counts := map[string]struct{ Actionable, Signals int }{}
	signals := 0
	var rows []ShadowScore
	var newlyFrozen []ShadowScore

	for _, sp := range proposals {
		if !sp.actionable() {
			signals++
			for _, name := range sp.Personas {
				c := counts[name]
				c.Signals++
				counts[name] = c
			}
			continue
		}
		for _, name := range sp.Personas {
			c := counts[name]
			c.Actionable++
			counts[name] = c
		}

		fp := shadowFingerprint(sp)
		if s, ok := frozen[fp]; ok {
			rows = append(rows, s)
			continue
		}

		row := ShadowScore{
			Fingerprint: fp,
			ContractID:  sp.ContractID,
			Personas:    sp.Personas,
			EntryPrice:  sp.Price,
			Size:        sp.Size,
			ProposedAt:  sp.ProposedAt,
			ScoredAt:    time.Now().UnixMilli(),
			Outcome:     shadowPending,
		}

		if resolved, settleVal := pt.realResolution(ctx, sp.ContractID); resolved {
			row.ExitValue = settleVal
			row.Outcome = shadowSettled
		} else if mark, quality := pt.markQuote(ctx, sp.ContractID); quality != quoteNone && mark > 0 {
			row.ExitValue = mark
			row.Outcome = shadowMarked
		}

		if row.Outcome != shadowPending {
			row.NetPnLUSD, row.NotionalUSD = scoreShadowProposal(sp, row.ExitValue, pt.feeRate, pt.feeCharge)
			row.NetPnLUSD = round2(row.NetPnLUSD)
			if row.NotionalUSD > 0 {
				row.ReturnPct = round2(row.NetPnLUSD / row.NotionalUSD * 100)
			}
		}
		if row.Outcome == shadowSettled {
			newlyFrozen = append(newlyFrozen, row)
		}
		rows = append(rows, row)
	}

	if len(newlyFrozen) > 0 && pt.redisClient != nil {
		pipe := pt.redisClient.Pipeline()
		for _, s := range newlyFrozen {
			if data, err := json.Marshal(s); err == nil {
				pipe.HSet(ctx, shadowSettledKey, s.Fingerprint, string(data))
			}
		}
		_, _ = pipe.Exec(ctx)
	}

	// Prune verdicts whose proposal has aged out of the (capped) shadow list, so the
	// frozen hash cannot outgrow the book it annotates. Guarded on a non-empty read:
	// a transient Redis miss returns no proposals, and pruning on that would throw away
	// every settled verdict we hold.
	if len(proposals) > 0 && len(frozen) > 0 && pt.redisClient != nil {
		live := make(map[string]bool, len(proposals))
		for _, sp := range proposals {
			live[shadowFingerprint(sp)] = true
		}
		var stale []string
		for fp := range frozen {
			if !live[fp] {
				stale = append(stale, fp)
			}
		}
		if len(stale) > 0 {
			_ = pt.redisClient.HDel(ctx, shadowSettledKey, stale...).Err()
		}
	}

	sc := buildShadowScorecard(rows, len(proposals), signals, counts, pt.shadowBarProposals, pt.shadowBarMinContracts)
	if pt.redisClient != nil {
		if data, err := json.Marshal(sc); err == nil {
			_ = pt.redisClient.Set(ctx, shadowScorecardKey, string(data), 0).Err()
		}
	}
	return sc
}

// logShadowScorecard prints the scorecard, personas worst-first so the reason the book
// is suspended stays the first thing on the line.
func logShadowScorecard(sc ShadowScorecard) {
	log.Printf("[SHADOW-SCORE] %d recorded = %d actionable + %d signal-only | scored %d over %d contracts (need %d) — %d settled, %d marked, %d pending | net $%.2f on $%.2f notional | expectancy %+.2f%% | win rate %.1f%% | %s",
		sc.Recorded, sc.Actionable, sc.Signals, sc.Scored, sc.Contracts, sc.MinContracts,
		sc.SettledCount, sc.MarkedCount, sc.PendingCount,
		sc.NetPnLUSD, sc.NotionalUSD, sc.ExpectancyPct, sc.WinRatePct, sc.Verdict)

	// Per-family before per-persona: concentration is the thing that decides whether the
	// per-persona numbers below mean anything (2026-09-03).
	for i, f := range sc.PerFamily {
		lead := "  "
		if i == 0 && f.Family == sc.TopFamily {
			lead = " *" // the series the ex-top expectancy strikes out
		}
		log.Printf("[SHADOW-SCORE] %s %-14s scored=%-4d contracts=%-4d net=$%-9.2f expectancy=%+.2f%%",
			lead, f.Family, f.Scored, f.Contracts, f.NetPnLUSD, f.ExpectancyPct)
	}
	if sc.TopFamily != "" {
		log.Printf("[SHADOW-SCORE]   without %s the book is %+.2f%% (overall %+.2f%%)",
			sc.TopFamily, sc.ExpectancyExTop, sc.ExpectancyPct)
	}

	names := make([]string, 0, len(sc.PerPersona))
	for name := range sc.PerPersona {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return sc.PerPersona[names[i]].ExpectancyPct < sc.PerPersona[names[j]].ExpectancyPct
	})
	for _, name := range names {
		p := sc.PerPersona[name]
		log.Printf("[SHADOW-SCORE]   %-14s actionable=%-4d signals=%-4d scored=%-4d net=$%-9.2f expectancy=%+.2f%% bar=%v",
			p.Persona, p.Actionable, p.Signals, p.Scored, p.NetPnLUSD, p.ExpectancyPct, p.BarMet)
	}
}

// runShadowScorer scores the shadow book on a timer. No-op when nothing is shadowed:
// an empty book would otherwise log a scorecard about nobody every interval.
func (pt *PositionTracker) runShadowScorer(ctx context.Context, interval time.Duration) {
	if len(pt.shadowPersonas) == 0 || pt.redisClient == nil || interval <= 0 {
		return
	}
	log.Printf("[INFO] Shadow scorer active (every %v). Re-activation bar: positive net-of-fee expectancy over %d actionable proposals; bearish signal-only proposals do not count toward it.",
		interval, pt.shadowBarProposals)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	logShadowScorecard(pt.scoreShadowBook(ctx))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logShadowScorecard(pt.scoreShadowBook(ctx))
		}
	}
}
