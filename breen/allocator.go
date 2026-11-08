package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const allocStateKey = "gekko:persona_alloc"

// AdaptiveAllocator is a bandit-style persona weighter (Phase 4). It keeps a
// decayed (EWMA) performance score per persona and maps it to a consensus weight,
// so allocation drifts toward personas that have actually been profitable — while
// an exploration floor and a min-trades gate keep under-sampled personas in play so
// the system doesn't prematurely starve a strategy of data. State persists to Redis
// (survives restarts now that Redis is durable).
//
// This evolves Phase 3a's static win-rate weighting into one that LEARNS over time
// (recency-decayed) and is gated behind enough settled trades to be meaningful.
type AdaptiveAllocator struct {
	rdb *redis.Client

	decay        float64 // EWMA alpha in (0,1]; higher = faster adaptation to recent trades
	gain         float64 // how strongly score maps to weight
	minTrades    int     // below this, a persona stays at neutral weight (pure explore)
	exploreFloor float64 // a persona's weight never drops below this (epsilon-explore)
	minW, maxW   float64 // weight clamp

	// Kill-switch (2026-07-03 post-mortem: personas at 12–23%% win rates kept trading
	// at ≥50%% weight): once a persona has ≥ benchMinTrades observations AND its EWMA
	// score is below benchScore, it is BENCHED — weight exactly 0, consensus drops its
	// proposals, riskGate rejects its direct orders — for benchDuration. On expiry its
	// score/count reset to 0 so it re-enters through the minTrades explore gate at
	// neutral weight (a benched persona settles no trades, so only time can free it).
	// benchMinTrades 0 disables. Manual reset: DEL gekko:persona_alloc in Redis.
	benchScore     float64
	benchMinTrades int
	benchDuration  time.Duration

	mu      sync.Mutex
	score   map[string]float64 // EWMA of per-trade outcome in [-1,1]
	count   map[string]int
	benched map[string]time.Time // persona → bench expiry
}

type allocPersisted struct {
	Score   map[string]float64 `json:"score"`
	Count   map[string]int     `json:"count"`
	Benched map[string]int64   `json:"benched,omitempty"` // persona → expiry unix ms
}

func NewAdaptiveAllocator(rdb *redis.Client, decay, gain, exploreFloor, minW, maxW float64, minTrades int) *AdaptiveAllocator {
	if decay <= 0 || decay > 1 {
		decay = 0.3
	}
	return &AdaptiveAllocator{
		rdb: rdb, decay: decay, gain: gain, minTrades: minTrades,
		exploreFloor: exploreFloor, minW: minW, maxW: maxW,
		score: map[string]float64{}, count: map[string]int{},
		benched: map[string]time.Time{},
	}
}

// SetBenchPolicy configures the persona kill-switch (benchMinTrades 0 disables).
func (a *AdaptiveAllocator) SetBenchPolicy(score float64, minTrades int, duration time.Duration) {
	a.mu.Lock()
	a.benchScore, a.benchMinTrades, a.benchDuration = score, minTrades, duration
	a.mu.Unlock()
}

// load restores persisted bandit state (best-effort).
func (a *AdaptiveAllocator) load(ctx context.Context) {
	if a.rdb == nil {
		return
	}
	val, err := a.rdb.Get(ctx, allocStateKey).Result()
	if err != nil {
		return
	}
	var p allocPersisted
	if json.Unmarshal([]byte(val), &p) != nil {
		return
	}
	a.mu.Lock()
	if p.Score != nil {
		a.score = p.Score
	}
	if p.Count != nil {
		a.count = p.Count
	}
	a.benched = map[string]time.Time{}
	for persona, expiryMs := range p.Benched {
		a.benched[persona] = time.UnixMilli(expiryMs)
	}
	n := len(a.score)
	a.mu.Unlock()
	if n > 0 {
		log.Printf("[INFO] Adaptive allocator restored %d persona scores from %s.", n, allocStateKey)
	}
}

func (a *AdaptiveAllocator) persist(ctx context.Context) {
	if a.rdb == nil {
		return
	}
	a.mu.Lock()
	p := allocPersisted{Score: a.score, Count: a.count}
	if len(a.benched) > 0 {
		p.Benched = make(map[string]int64, len(a.benched))
		for persona, expiry := range a.benched {
			p.Benched[persona] = expiry.UnixMilli()
		}
	}
	a.mu.Unlock()
	if data, err := json.Marshal(p); err == nil {
		_ = a.rdb.Set(ctx, allocStateKey, string(data), 0).Err()
	}
}

// Observe folds one closed trade's outcome into the persona's decayed score.
// returnPct is the trade's percentage return; it's bounded to ±100% so a single
// settlement can't dominate the EWMA.
func (a *AdaptiveAllocator) Observe(persona string, returnPct float64) {
	if persona == "" {
		persona = "unattributed"
	}
	outcome := math.Max(-1, math.Min(1, returnPct/100.0))
	a.mu.Lock()
	prev, seen := a.score[persona]
	if !seen {
		a.score[persona] = outcome
	} else {
		a.score[persona] = (1-a.decay)*prev + a.decay*outcome
	}
	a.count[persona]++
	// Kill-switch: bench a persona with enough history and a persistently bad score.
	if a.benchMinTrades > 0 && a.count[persona] >= a.benchMinTrades && a.score[persona] < a.benchScore {
		if _, already := a.benched[persona]; !already {
			until := time.Now().Add(a.benchDuration)
			a.benched[persona] = until
			log.Printf("[ADAPTIVE] persona %s BENCHED (score %.2f < %.2f after %d trades) until %s",
				persona, a.score[persona], a.benchScore, a.count[persona], until.Format(time.RFC3339))
		}
	}
	a.mu.Unlock()
	// Persist off the caller's goroutine (Observe runs under the tracker lock).
	go a.persist(context.Background())
}

// weightFor maps a persona's current score to a consensus weight. Pure given the
// snapshot; see Weights for the locked accessor.
func weightFor(score float64, count, minTrades int, gain, exploreFloor, minW, maxW float64) float64 {
	if count < minTrades {
		return 1.0 // not enough data — neutral (explore)
	}
	w := 1.0 + gain*score
	w = math.Max(minW, math.Min(maxW, w))
	return math.Max(exploreFloor, w)
}

// consensusWeights returns the persona weights for the consensus vote: the
// adaptive allocator's learned weights when enabled, else Phase 3a's static
// win-rate weighting from current closed-trade stats.
func (pt *PositionTracker) consensusWeights() map[string]float64 {
	if pt.allocator != nil {
		return pt.allocator.Weights()
	}
	return personaWeights(pt.personaStatsSnapshot())
}

// sweepExpiredBenchesLocked releases personas whose bench has expired, resetting
// score/count so they re-enter through the minTrades explore gate at neutral
// weight. Caller must hold a.mu.
func (a *AdaptiveAllocator) sweepExpiredBenchesLocked(now time.Time) {
	for persona, until := range a.benched {
		if now.After(until) {
			delete(a.benched, persona)
			a.score[persona] = 0
			a.count[persona] = 0
			log.Printf("[ADAPTIVE] persona %s bench expired — re-enabled at neutral weight (probation via min-trades gate)", persona)
		}
	}
}

// IsBenched reports whether a persona is currently benched by the kill-switch.
func (a *AdaptiveAllocator) IsBenched(persona string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepExpiredBenchesLocked(time.Now())
	_, benched := a.benched[persona]
	return benched
}

// Weights returns the current per-persona consensus weights. Benched personas get
// an EXPLICIT 0 (consensus must distinguish "benched" from "unknown → neutral").
func (a *AdaptiveAllocator) Weights() map[string]float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepExpiredBenchesLocked(time.Now())
	out := make(map[string]float64, len(a.score))
	for p, s := range a.score {
		out[p] = weightFor(s, a.count[p], a.minTrades, a.gain, a.exploreFloor, a.minW, a.maxW)
	}
	for p := range a.benched {
		out[p] = 0
	}
	return out
}
