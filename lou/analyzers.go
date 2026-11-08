package main

import (
	"fmt"
	"sort"
)

// Analyzers turn the cumulative trade corpus into candidate lessons. Each is a
// pure function; thresholds are deliberately conservative (sample-size floors)
// so Lou never claims a pattern off thin evidence — the improvement log shows
// how a handful of trades can mislead. Every analyzer recomputes from scratch
// over all runs, so evidence is cumulative by construction.

// agg accumulates the evidence counters for one bucket.
type agg struct {
	trades, wins, losses int
	pnl                  float64
	runs                 map[string]bool
}

func (a *agg) add(t ClosedTrade) {
	if a.runs == nil {
		a.runs = map[string]bool{}
	}
	a.trades++
	a.pnl += t.RealizedPnL
	switch {
	case t.RealizedPnL > 0.005:
		a.wins++
	case t.RealizedPnL < -0.005:
		a.losses++
	}
	a.runs[t.Run] = true
}

func (a *agg) winRatePct() float64 {
	if a.wins+a.losses == 0 {
		return 0
	}
	return 100 * float64(a.wins) / float64(a.wins+a.losses)
}

func (a *agg) runList() []string {
	runs := make([]string, 0, len(a.runs))
	for r := range a.runs {
		runs = append(runs, r)
	}
	sort.Strings(runs)
	return runs
}

func (a *agg) evidence(detail string) Evidence {
	return Evidence{
		Trades: a.trades, Wins: a.wins, Losses: a.losses,
		PnL: a.pnl, WinRatePct: a.winRatePct(), Runs: a.runList(), Detail: detail,
	}
}

// entryBands are the price buckets the post-mortems kept coming back to:
// sub-$0.10 longshots, the 0.50–0.65 revert band, etc.
var entryBands = []struct {
	lo, hi float64
	name   string
}{
	{0.00, 0.10, "0.00-0.10"},
	{0.10, 0.25, "0.10-0.25"},
	{0.25, 0.50, "0.25-0.50"},
	{0.50, 0.65, "0.50-0.65"},
	{0.65, 1.01, "0.65-1.00"},
}

// analyzeEntryBands flags price bands where entries reliably lose (win rate
// <25% and negative PnL) or reliably win (>55% and positive PnL).
func analyzeEntryBands(trades []ClosedTrade, minSample int) []*Lesson {
	byBand := map[string]*agg{}
	for _, t := range trades {
		for _, b := range entryBands {
			if t.EntryPrice >= b.lo && t.EntryPrice < b.hi {
				a := byBand[b.name]
				if a == nil {
					a = &agg{}
					byBand[b.name] = a
				}
				a.add(t)
				break
			}
		}
	}
	var out []*Lesson
	for _, b := range entryBands {
		a := byBand[b.name]
		if a == nil || a.trades < minSample {
			continue
		}
		switch {
		case a.winRatePct() < 25 && a.pnl < 0:
			out = append(out, &Lesson{
				ID: "entry-band/" + b.name,
				Pattern: fmt.Sprintf("Entries priced %s lose: %.0f%% win rate over %d trades, $%.0f total",
					b.name, a.winRatePct(), a.trades, a.pnl),
				Evidence:        a.evidence(""),
				SuggestedAction: bandAction(b.lo, b.hi),
			})
		case a.winRatePct() > 55 && a.pnl > 0:
			out = append(out, &Lesson{
				ID: "entry-band/" + b.name,
				Pattern: fmt.Sprintf("Entries priced %s win: %.0f%% win rate over %d trades, +$%.0f total",
					b.name, a.winRatePct(), a.trades, a.pnl),
				Evidence: a.evidence(""),
			})
		}
	}
	return out
}

// bandAction maps a losing band to the existing config knob that bounds it.
func bandAction(lo, hi float64) string {
	switch {
	case lo == 0:
		return fmt.Sprintf("consider raising MIN_ENTRY_PRICE toward $%.2f", hi)
	case hi > 0.60:
		return fmt.Sprintf("consider lowering MAX_ENTRY_PRICE toward $%.2f", lo)
	default:
		return ""
	}
}

// analyzeFastStops looks at how long stopped-out positions lived. Stops that
// fire within two minutes are microstructure noise (2026-07-07 forensics:
// median 24s), not adverse moves — the STOP_LOSS_MIN_ABS floor is the lever.
func analyzeFastStops(trades []ClosedTrade) []*Lesson {
	var holds []float64
	a := &agg{}
	fast := 0
	for _, t := range trades {
		if t.Status != "STOP_LOSS" {
			continue
		}
		a.add(t)
		h := t.HoldSeconds()
		holds = append(holds, h)
		if h > 0 && h < 120 {
			fast++
		}
	}
	if a.trades < 10 {
		return nil
	}
	sort.Float64s(holds)
	median := holds[len(holds)/2]
	if float64(fast)/float64(a.trades) < 0.5 {
		return nil
	}
	return []*Lesson{{
		ID: "stops/noise-stopouts",
		Pattern: fmt.Sprintf("Stop-losses fire on noise: %d of %d stops closed within 2min of entry (median hold %.0fs), $%.0f total",
			fast, a.trades, median, a.pnl),
		Evidence:        a.evidence(fmt.Sprintf("median hold %.0fs", median)),
		SuggestedAction: "verify STOP_LOSS_MIN_ABS covers the typical spread; consider raising it",
	}}
}

// shortExpiryCutoffSec is the "super short expiry" threshold: contracts entered
// with ≤1h of life left. These resolve on near-term noise before any thesis can
// play out — the run's single worst trade was a KXETH15M 15-minute market bought
// with ~2min to run (−$139, −68%). Matches gordon's NEAR_EXPIRY_SKIP default (1h).
const shortExpiryCutoffSec = 3600

// analyzeShortExpiry flags entries made on contracts with almost no life left.
// Unlike the price-band analyzers this uses a low sample floor: ultra-short expiry
// is a structural rule (coin-flip variance, no time to be right), not a subtle
// edge, so a handful of losers is enough to warrant the flag. Only trades whose
// expiry is parseable AND that had a real open timestamp are scored.
func analyzeShortExpiry(trades []ClosedTrade) []*Lesson {
	const minSample = 4
	a := &agg{}
	var worstPnL float64
	var worstID string
	for _, t := range trades {
		life, ok := t.LifeAtEntrySeconds()
		if !ok || life > shortExpiryCutoffSec {
			continue
		}
		a.add(t)
		if t.RealizedPnL < worstPnL {
			worstPnL, worstID = t.RealizedPnL, t.ContractID
		}
	}
	if a.trades < minSample || a.pnl >= 0 {
		return nil
	}
	return []*Lesson{{
		ID: "expiry/short",
		Pattern: fmt.Sprintf("Entries with ≤1h to resolution lose: %.0f%% win rate over %d trades, $%.0f total (worst %s $%.0f)",
			a.winRatePct(), a.trades, a.pnl, worstID, worstPnL),
		Evidence:        a.evidence(fmt.Sprintf("worst single %s $%.0f", worstID, worstPnL)),
		SuggestedAction: "widen NEAR_EXPIRY_SKIP (gordon) so these never enter; verify tickerExpiry parses the series",
	}}
}

// analyzeFamilyBurn flags contract families that soaked up repeated losses —
// the TESLACEOCHANGE-churn / KXFDVOPENSEA pattern the per-contract dashboard
// was supposed to catch live.
func analyzeFamilyBurn(trades []ClosedTrade) []*Lesson {
	byFam := map[string]*agg{}
	for _, t := range trades {
		fam := contractFamily(t.ContractID)
		a := byFam[fam]
		if a == nil {
			a = &agg{}
			byFam[fam] = a
		}
		a.add(t)
	}
	fams := make([]string, 0, len(byFam))
	for f := range byFam {
		fams = append(fams, f)
	}
	sort.Strings(fams)
	var out []*Lesson
	for _, f := range fams {
		a := byFam[f]
		// Floors: enough trades, a real cumulative loss, AND a real per-trade
		// bleed — 249 trades slowly grinding to −$252 is churn, not a lesson.
		if a.trades >= 8 && a.pnl <= -200 && a.pnl/float64(a.trades) <= -5 {
			out = append(out, &Lesson{
				ID: "family/" + f,
				Pattern: fmt.Sprintf("Family %s is a repeat loser: %d trades, %.0f%% win rate, $%.0f total",
					f, a.trades, a.winRatePct(), a.pnl),
				Evidence:        a.evidence(""),
				SuggestedAction: "check whether PER_CONTRACT_LOSS_LIMIT and EXIT_COOLDOWN are containing this family",
			})
		}
	}
	// Cap at the 10 worst by total loss: when the whole book bleeds, a lesson per
	// family drowns the digest — the point is the outliers, not the census.
	sort.Slice(out, func(i, j int) bool { return out[i].Evidence.PnL < out[j].Evidence.PnL })
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

// analyzePersonas scores each persona on its OWN proposals (persona_scores,
// breen's scorecard data) by return on capital = Σpnl/Σcost — the same metric
// the dashboard shows, so lessons reconcile with the scorecard.
func analyzePersonas(trades []ClosedTrade, minSample int) []*Lesson {
	type pstat struct {
		agg
		cost float64
	}
	byPersona := map[string]*pstat{}
	for _, t := range trades {
		for _, ps := range t.PersonaScores {
			p := byPersona[ps.Persona]
			if p == nil {
				p = &pstat{}
				byPersona[ps.Persona] = p
			}
			// Score the persona's own outcome, not the consolidated trade's.
			p.add(ClosedTrade{RealizedPnL: ps.RealizedPnL, Run: t.Run, ContractID: t.ContractID})
			p.cost += ps.Cost
		}
	}
	names := make([]string, 0, len(byPersona))
	for n := range byPersona {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*Lesson
	for _, n := range names {
		p := byPersona[n]
		if p.trades < minSample || p.cost <= 0 {
			continue
		}
		roc := 100 * p.pnl / p.cost
		if roc <= -5 {
			out = append(out, &Lesson{
				ID: "persona/" + n,
				Pattern: fmt.Sprintf("Persona %s loses on its own proposals: %.1f%% return on $%.0f deployed over %d scored trades",
					n, roc, p.cost, p.trades),
				Evidence:        p.evidence(fmt.Sprintf("return on capital %.1f%%", roc)),
				SuggestedAction: "confirm the adaptive allocator is down-weighting this persona",
			})
		} else if roc >= 5 {
			out = append(out, &Lesson{
				ID: "persona/" + n,
				Pattern: fmt.Sprintf("Persona %s earns on its own proposals: +%.1f%% return on $%.0f deployed over %d scored trades",
					n, roc, p.cost, p.trades),
				Evidence: p.evidence(fmt.Sprintf("return on capital %.1f%%", roc)),
			})
		}
	}
	return out
}

// analyzeVenues compares venues when both have a real sample; a lopsided PnL
// split (one venue profitable, the other bleeding) is worth knowing before
// tuning anything venue-blind.
func analyzeVenues(trades []ClosedTrade, minSample int) []*Lesson {
	byVenue := map[string]*agg{}
	for _, t := range trades {
		if t.Exchange == "" {
			continue
		}
		a := byVenue[t.Exchange]
		if a == nil {
			a = &agg{}
			byVenue[t.Exchange] = a
		}
		a.add(t)
	}
	// A venue lesson is only a lesson when the venues DIVERGE — if both bleed the
	// problem is the strategy, not the venue, and the entry-band/persona
	// analyzers own that. Requires exactly two venues with real samples.
	venues := make([]string, 0, len(byVenue))
	for v := range byVenue {
		if byVenue[v].trades >= minSample {
			venues = append(venues, v)
		}
	}
	if len(venues) != 2 {
		return nil
	}
	sort.Strings(venues)
	worse, better := byVenue[venues[0]], byVenue[venues[1]]
	worseName := venues[0]
	if better.winRatePct() < worse.winRatePct() {
		worse, better = better, worse
		worseName = venues[1]
	}
	if worse.pnl >= 0 || better.winRatePct()-worse.winRatePct() < 10 {
		return nil
	}
	return []*Lesson{{
		ID: "venue/" + worseName,
		Pattern: fmt.Sprintf("Venue %s underperforms: %.0f%% win rate / $%.0f over %d trades vs %.0f%% on the other venue",
			worseName, worse.winRatePct(), worse.pnl, worse.trades, better.winRatePct()),
		Evidence: worse.evidence(fmt.Sprintf("other venue win rate %.0f%%", better.winRatePct())),
	}}
}

// runAnalyzers is the full phase-1 battery.
func runAnalyzers(trades []ClosedTrade, minSample int) []*Lesson {
	var out []*Lesson
	out = append(out, analyzeEntryBands(trades, minSample)...)
	out = append(out, analyzeShortExpiry(trades)...)
	out = append(out, analyzeFastStops(trades)...)
	out = append(out, analyzeFamilyBurn(trades)...)
	out = append(out, analyzePersonas(trades, minSample)...)
	out = append(out, analyzeVenues(trades, minSample)...)
	return out
}
