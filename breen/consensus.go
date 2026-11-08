package main

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// ConsensusEngine buffers competing BUY proposals about the same contract over a
// short window, then consolidates them into ONE order weighted by each persona's
// performance weight × its conviction. Agreement (more/stronger personas) sizes the
// order up; a lone weak proposal sizes down; too little conviction is skipped.
// SELL/exit proposals bypass the window and act immediately (they only reduce risk).
type ConsensusEngine struct {
	tracker       *PositionTracker
	window        time.Duration
	minConviction float64
	bearVeto      bool          // weigh bearish signals against BUYs on the same event
	bearCacheTTL  time.Duration // how long a bearish signal keeps opposing later BUYs (0 = window-only)

	mu        sync.Mutex
	buffers   map[string]*consensusBuffer // keyed by eventKey
	bearCache []cachedBear                // recent bearish signals, pruned by TTL
}

type consensusBuffer struct {
	proposals []StrategyProposal
	timer     *time.Timer
}

// cachedBear is a bearish signal retained past its window so a BUY that flushes
// minutes later (persona skew) can still be vetoed by it. Matched to a winning BUY by
// EXACT contract or shared news event — never by family, so a bear on one strike of a
// ladder can't veto a bull on a different strike.
type cachedBear struct {
	persona    string
	contractID string
	eventHash  string
	confidence float64
	at         time.Time
}

func NewConsensusEngine(tracker *PositionTracker, window time.Duration, minConviction float64, bearVeto bool, bearCacheTTL time.Duration) *ConsensusEngine {
	if window <= 0 {
		window = 3 * time.Second
	}
	return &ConsensusEngine{
		tracker:       tracker,
		window:        window,
		minConviction: minConviction,
		bearVeto:      bearVeto,
		bearCacheTTL:  bearCacheTTL,
		buffers:       make(map[string]*consensusBuffer),
	}
}

// eventKey groups proposals that react to the same news event so personas are
// voted on together even when they pick slightly different tickers. Falls back to
// the contract when no EventHash is present.
func eventKey(p *StrategyProposal) string {
	if p.EventHash != "" {
		return "evt:" + p.EventHash
	}
	return "ct:" + p.ContractID
}

// Submit routes a proposal: a real SELL/exit (held family) goes straight through;
// BUY opinions and bearish veto signals are buffered per news event until the window
// elapses, then consolidated. Bearish signals never execute — they only oppose the
// event's BUYs at flush time (and are inert when the veto is disabled).
func (c *ConsensusEngine) Submit(p *StrategyProposal) {
	if p.Bearish {
		if !c.bearVeto {
			return
		}
		c.buffer(p)
		return
	}
	if strings.ToUpper(p.Action) != "BUY" {
		c.tracker.ProcessStrategyProposal(p)
		return
	}
	c.buffer(p)
}

// buffer appends a proposal to its event window, starting the flush timer on first
// arrival for that event.
func (c *ConsensusEngine) buffer(p *StrategyProposal) {
	cp := *p // copy: the caller's value is a loop variable
	key := eventKey(p)
	c.mu.Lock()
	buf := c.buffers[key]
	if buf == nil {
		buf = &consensusBuffer{}
		buf.timer = time.AfterFunc(c.window, func() { c.flush(key) })
		c.buffers[key] = buf
	}
	buf.proposals = append(buf.proposals, cp)
	n := len(buf.proposals)
	c.mu.Unlock()
	kind := "BUY"
	if p.Bearish {
		kind = "BEAR-SIGNAL"
	}
	log.Printf("[CONSENSUS] buffered %s %s from persona=%s conf=%.2f size=%d (%d in event window)",
		kind, p.ContractID, personaOrDefault(p.Persona), p.Confidence, p.TotalSize, n)
}

// weightedConviction sums persona-weight × confidence across proposals (the
// agreement measure used to pick the winning contract within an event). A persona
// MISSING from the weights map is neutral (1.0); one PRESENT with weight ≤ 0 is
// benched by the kill-switch and contributes nothing. cal rescales each persona's
// stated confidence by its own track record at that confidence level (nil = stated,
// the pre-Phase-1 behavior), so agreement becomes agreement among CALIBRATED beliefs:
// a persona that cries 0.9 and delivers 0.4 stops dominating the vote without waiting
// for the allocator's EWMA to bench it.
func weightedConviction(props []StrategyProposal, weights map[string]float64, cal confidenceCalibrator) float64 {
	var sum float64
	for _, p := range props {
		w, known := weights[p.Persona]
		if !known {
			w = 1.0
		} else if w <= 0 {
			continue // benched
		}
		conf := cal.apply(personaOrDefault(p.Persona), p.Confidence)
		if conf < 0.1 {
			conf = 0.1
		}
		sum += w * conf
	}
	return sum
}

// pickWinningContract groups proposals by contract and returns the contract with
// the highest weighted conviction, its proposals, and the number of distinct
// contracts seen. Pure (testable).
func pickWinningContract(props []StrategyProposal, weights map[string]float64, cal confidenceCalibrator) (string, []StrategyProposal, int) {
	groups := map[string][]StrategyProposal{}
	for _, p := range props {
		groups[p.ContractID] = append(groups[p.ContractID], p)
	}
	var winner string
	var bestConv float64
	for cid, subset := range groups {
		if conv := weightedConviction(subset, weights, cal); conv > bestConv {
			bestConv, winner = conv, cid
		}
	}
	return winner, groups[winner], len(groups)
}

func (c *ConsensusEngine) flush(key string) {
	c.mu.Lock()
	buf := c.buffers[key]
	delete(c.buffers, key)
	c.mu.Unlock()
	if buf == nil || len(buf.proposals) == 0 {
		return
	}

	weights := c.tracker.consensusWeights()
	// One calibrator per flush: every persona in this window is judged against the
	// same table, so a trade settling mid-flush can't shift the vote under it.
	cal := c.tracker.calibrator()
	now := time.Now()

	// Split the event's BUY opinions from the bearish veto signals.
	buys, bearish := splitByStance(buf.proposals)
	if len(buys) == 0 {
		// Only bearish signals (or an empty flush): the run is YES-only, so bears
		// alone can't open anything — but remember them so they can veto a BUY on the
		// same contract/event that flushes within the cache TTL (the whole point of
		// the cache: bear and bull almost never share the 3s window).
		if c.bearVeto && c.bearCacheTTL > 0 && len(bearish) > 0 {
			c.mu.Lock()
			c.rememberBears(bearish, now)
			c.mu.Unlock()
		}
		return
	}

	// Personas reacting to one event may pick different tickers; trade the one they
	// most agree on (highest weighted conviction).
	winner, winnerProps, groupCount := pickWinningContract(buys, weights, cal)
	dropped := groupCount - 1

	consolidated, summary, ok := consolidateProposals(winnerProps, weights, c.minConviction, cal)
	if !ok {
		c.tracker.consensusSkipped.Add(1)
		log.Printf("[CONSENSUS] %s: winner %s %s — skipped (below floor %.2f); %d other contract(s) dropped",
			key, winner, summary, c.minConviction, dropped)
		if c.bearVeto && c.bearCacheTTL > 0 && len(bearish) > 0 {
			c.mu.Lock()
			c.rememberBears(bearish, now)
			c.mu.Unlock()
		}
		return
	}

	// Fold in bearish signals cached from earlier windows (persona skew means bear and
	// bull rarely share the 3s window, so the in-window veto had never engaged). Match
	// by exact contract or shared event, one vote per persona, excluding personas
	// already bearish in this window. Then remember this window's bears for future BUYs.
	if c.bearVeto && c.bearCacheTTL > 0 {
		inWindow := map[string]bool{}
		for _, b := range bearish {
			inWindow[personaOrDefault(b.Persona)] = true
		}
		c.mu.Lock()
		cachedMatched, kept := selectCachedBears(c.bearCache, winner, consolidated.EventHash, inWindow, now.Add(-c.bearCacheTTL))
		c.bearCache = kept
		c.rememberBears(bearish, now)
		c.mu.Unlock()
		if len(cachedMatched) > 0 {
			bearish = append(bearish, cachedMatched...)
		}
	}

	// Bearish veto: weigh the event's bear conviction against the winning BUY's and
	// shrink (or cancel) the long accordingly.
	if len(bearish) > 0 {
		bullConv := weightedConviction(winnerProps, weights, cal)
		bearConv := weightedConviction(bearish, weights, cal)
		f := vetoFactor(bullConv, bearConv)
		newSize := int(math.Round(float64(consolidated.TotalSize) * f))
		if newSize < 1 {
			c.tracker.consensusSkipped.Add(1)
			log.Printf("[CONSENSUS] %s: winner %s vetoed by bears (bull %.2f ≤ bear %.2f, %d bearish signal(s)) — skipped",
				key, winner, bullConv, bearConv, len(bearish))
			return
		}
		if newSize != consolidated.TotalSize {
			log.Printf("[CONSENSUS] %s: bearish veto shrank %s size %d→%d (bull %.2f vs bear %.2f, %d bearish signal(s))",
				key, winner, consolidated.TotalSize, newSize, bullConv, bearConv, len(bearish))
			consolidated.TotalSize = newSize
			consolidated.BreachReason += fmt.Sprintf("; bear-veto ×%.2f (%d signal(s))", f, len(bearish))
		}
	}

	c.tracker.consensusOrders.Add(1)
	log.Printf("[CONSENSUS] %s: %d contract(s), winner %s | %s → %s %d @ %.3f (%d dropped)",
		key, groupCount, winner, summary, consolidated.Action, consolidated.TotalSize, consolidated.AveragePrice, dropped)
	c.tracker.ProcessStrategyProposal(consolidated)
}

// splitByStance partitions buffered proposals into executable BUYs and bearish veto
// signals. A non-BUY that isn't flagged bearish (shouldn't reach the buffer) is
// ignored. Pure.
func splitByStance(props []StrategyProposal) (buys, bearish []StrategyProposal) {
	for _, p := range props {
		switch {
		case p.Bearish:
			bearish = append(bearish, p)
		case strings.ToUpper(p.Action) == "BUY":
			buys = append(buys, p)
		}
	}
	return buys, bearish
}

// selectCachedBears extends the bearish veto past the consensus window. It prunes
// entries older than the TTL cutoff and returns (matched, kept):
//   - matched: synthetic bear proposals for cache entries opposing the winner (same
//     exact contract OR same news event), one per persona (the strongest), excluding
//     personas already represented by in-window bears (excludePersona) so no persona
//     votes twice. These flow into the same weightedConviction the in-window bears use.
//   - kept: the still-live cache (expired entries dropped).
//
// Pure: no clock, no locks — caller passes cutoff and holds the mutex.
func selectCachedBears(cache []cachedBear, winnerContract, winnerEvent string, excludePersona map[string]bool, cutoff time.Time) (matched []StrategyProposal, kept []cachedBear) {
	strongest := map[string]cachedBear{} // persona -> best matching cached bear
	for _, cb := range cache {
		if cb.at.Before(cutoff) {
			continue // expired — drop from kept
		}
		kept = append(kept, cb)
		if excludePersona[cb.persona] {
			continue // this persona already voted in-window
		}
		matches := cb.contractID == winnerContract || (winnerEvent != "" && cb.eventHash == winnerEvent)
		if !matches {
			continue
		}
		if best, ok := strongest[cb.persona]; !ok || cb.confidence > best.confidence {
			strongest[cb.persona] = cb
		}
	}
	for _, cb := range strongest {
		matched = append(matched, StrategyProposal{
			Persona:    cb.persona,
			ContractID: cb.contractID,
			Confidence: cb.confidence,
			Bearish:    true,
		})
	}
	return matched, kept
}

// maxBearCache bounds the cache so a flood of bearish signals with no matching BUY
// flush (which is what prunes on match) can't grow it without limit. Oldest-first
// eviction; ample for the fleet's signal rate over any sane TTL.
const maxBearCache = 512

// rememberBears appends this window's bearish signals to the cache so later BUYs on
// the same contract/event can be vetoed by them, then prunes entries past the TTL so
// the cache stays bounded even when no BUY flush is triggering selectCachedBears'
// pruning. Caller holds the mutex.
func (c *ConsensusEngine) rememberBears(bears []StrategyProposal, now time.Time) {
	for _, b := range bears {
		c.bearCache = append(c.bearCache, cachedBear{
			persona:    personaOrDefault(b.Persona),
			contractID: b.ContractID,
			eventHash:  b.EventHash,
			confidence: b.Confidence,
			at:         now,
		})
	}
	cutoff := now.Add(-c.bearCacheTTL)
	pruned := c.bearCache[:0]
	for _, cb := range c.bearCache {
		if !cb.at.Before(cutoff) {
			pruned = append(pruned, cb)
		}
	}
	if len(pruned) > maxBearCache {
		pruned = pruned[len(pruned)-maxBearCache:]
	}
	c.bearCache = pruned
}

// vetoFactor maps bull vs bear conviction to a [0,1] BUY-size multiplier: full size
// when bears are silent, linearly shrinking as bear conviction rises, and 0 (full
// veto) once bears match or outweigh the bulls. Pure.
func vetoFactor(bullConv, bearConv float64) float64 {
	if bearConv <= 0 || bullConv <= 0 {
		return 1.0
	}
	f := 1.0 - bearConv/bullConv
	if f < 0 {
		return 0
	}
	return f
}

// consolidateProposals merges buffered BUY proposals into one weighted order.
// Returns the consolidated proposal, a human summary, and ok=false when total
// conviction is below minConviction (caller skips). Pure function for testability.
// cal rescales each persona's stated confidence by its own record at that confidence
// level before it is weighted (nil = stated). Note that the per-persona
// ContributorProposal below keeps the RAW stated confidence — see the comment there.
func consolidateProposals(props []StrategyProposal, weights map[string]float64, minConviction float64, cal confidenceCalibrator) (*StrategyProposal, string, bool) {
	if len(props) == 0 {
		return nil, "no proposals", false
	}

	var sumW, sizeW, priceW float64
	contributors := make([]string, 0, len(props))
	seen := map[string]bool{}
	names := make([]string, 0, len(props))
	contribProps := make([]ContributorProposal, 0, len(props))
	dropped := 0
	for _, p := range props {
		w, known := weights[p.Persona]
		if !known {
			w = 1.0 // unknown persona → neutral weight
		} else if w <= 0 {
			dropped++ // benched by the kill-switch — its proposal doesn't count
			continue
		}
		conf := cal.apply(personaOrDefault(p.Persona), p.Confidence)
		if conf < 0.1 {
			conf = 0.1 // floor so a 0-confidence proposal still counts a little
		}
		wc := w * conf
		sumW += wc
		sizeW += wc * float64(p.TotalSize)
		priceW += wc * p.AveragePrice
		contributors = append(contributors, fmt.Sprintf("%s(w%.2f×c%.2f)", personaOrDefault(p.Persona), w, conf))
		if name := personaOrDefault(p.Persona); !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		// Retain each persona's own proposal so it can be scored individually at close.
		// Confidence here is the RAW stated value, never the calibrated one: this field
		// is what the calibration table itself bins (via PersonaScore.Confidence), so
		// recording a calibrated number would make the table self-referential — it would
		// be calibrating its own output and drift a little further every run.
		contribProps = append(contribProps, ContributorProposal{
			Persona:    personaOrDefault(p.Persona),
			EntryPrice: p.AveragePrice,
			Size:       p.TotalSize,
			Confidence: p.Confidence,
		})
	}
	sort.Strings(contributors)
	sort.Strings(names)
	summary := fmt.Sprintf("%d personas [%s] conviction %.2f", len(props)-dropped, strings.Join(contributors, ","), sumW)
	if dropped > 0 {
		summary += fmt.Sprintf(" (%d benched proposal(s) dropped)", dropped)
	}

	// All contributors benched → nothing to trade (also guards the ÷sumW below).
	if sumW <= 0 || sumW < minConviction {
		return nil, summary, false
	}

	baseSize := sizeW / sumW
	avgPrice := priceW / sumW
	// Agreement multiplier: total conviction maps to a 0.5×–2.0× sizing factor.
	mult := math.Max(0.5, math.Min(2.0, sumW))
	size := int(math.Round(baseSize * mult))
	if size < 1 {
		return nil, summary, false
	}

	return &StrategyProposal{
		ContractID:           props[0].ContractID,
		Action:               "BUY",
		AveragePrice:         avgPrice,
		TotalSize:            size,
		Persona:              "consensus",
		StrategyID:           "consensus",
		Contributors:         names,
		ContributorProposals: contribProps,
		Confidence:           math.Min(1.0, sumW/float64(len(props))),
		BreachReason:         summary,
		EventHash:            props[0].EventHash,
		Timestamp:            time.Now(),
	}, summary, true
}

func personaOrDefault(p string) string {
	if p == "" {
		return "unattributed"
	}
	return p
}

// personaWeights maps a persona to a performance multiplier in [0.5, 1.5], derived
// from its win rate with sample-size shrinkage (few trades → weight ≈ 1.0). This is
// the Tier-2 "reward": better track record → more say in consensus. Personas not in
// the stats get the neutral default (1.0) via the lookup miss.
func personaWeights(stats []PersonaStat) map[string]float64 {
	const n0 = 5.0 // shrinkage: trades needed before the record meaningfully moves weight
	out := make(map[string]float64, len(stats))
	for _, s := range stats {
		shrink := float64(s.Trades) / (float64(s.Trades) + n0)
		edge := s.WinRate/100.0 - 0.5 // -0.5..0.5
		w := 1.0 + 2.0*edge*shrink    // ±1.0 at full conviction before clamp
		w = math.Max(0.5, math.Min(1.5, w))
		out[s.Persona] = math.Round(w*1000) / 1000
	}
	return out
}
