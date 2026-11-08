package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// gordonPriorityKey is the Redis key liveview writes and every gordon polls to
// pace its proposal generation. It is a deliberately dumb, manual throttle: the
// fleet can only serve so many generate calls per second, so we split that budget
// across the running gordons and let the operator promote/demote individual ones
// from the UI. A demoted gordon simply waits longer between calls (and, at weight
// 0, is paused entirely), leaving more of the local Ollama's throughput for the
// promoted ones. It complements the concurrency semaphore in llmlimiter.go, which
// bounds SIMULTANEOUS calls but not their arrival rate.
const gordonPriorityKey = "gekko:gordon_priority"

// gordonFleetPrefix keys the presence heartbeats each gordon writes so the UI can
// list exactly the running fleet (whatever GORDON_PERSONAS launched) instead of a
// hardcoded registry. See RegisterPresence.
const gordonFleetPrefix = "gekko:gordon_fleet:"

// RegisterPresence heartbeats this gordon's identity (id → display label) into
// Redis with a short TTL, refreshing it periodically so a stopped gordon drops off
// the roster automatically. Blocks until ctx is cancelled; nil rdb ⇒ no-op.
func RegisterPresence(ctx context.Context, rdb *redis.Client, id, label string) {
	if rdb == nil {
		return
	}
	const ttl = 30 * time.Second
	key := gordonFleetPrefix + id
	write := func() {
		wc, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = rdb.Set(wc, key, label, ttl).Err()
		cancel()
	}
	write()
	t := time.NewTicker(ttl / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			write()
		}
	}
}

// ErrPersonaPaused / ErrRateBudget are returned by (*PriorityControls).Wait so the
// caller falls back to keyword rules instead of calling the model.
var (
	ErrPersonaPaused = errors.New("persona paused (weight 0)")
	ErrRateBudget    = errors.New("rate budget: next slot beyond max wait")
)

// gordonPriority is the JSON blob exchanged via Redis.
//
//   - FleetRate is the estimated sustainable generate calls/sec the WHOLE fleet's
//     Ollama can serve. 0 ⇒ each gordon auto-estimates it from its own concurrency
//     and proposal timeout (see NewPriorityControls' defaultRate).
//   - Weights is a per-persona multiplier on that gordon's share of the budget:
//     1 = normal, <1 = demoted (slower), >1 = promoted (faster), 0 = paused.
type gordonPriority struct {
	FleetRate   float64            `json:"fleet_rate"`
	Weights     map[string]float64 `json:"weights"`
	UpdatedAtMs int64              `json:"updated_at_ms"`
}

// PriorityControls polls gekko:gordon_priority and turns this gordon's weight into
// a minimum interval between generate calls, then enforces it as a rate gate.
//
// Baseline cadence = fleetSize / fleetRate seconds per call (the budget split
// evenly across the running gordons); the persona's weight scales it:
// effectiveInterval = baseline / weight. Nil-safe: a nil *PriorityControls is an
// open gate (no throttling), so callers need no nil checks.
type PriorityControls struct {
	rdb         *redis.Client
	personaID   string
	defaultRate float64 // auto-estimate used when the blob's fleet_rate is 0/absent
	defaultSize int     // assumed fleet size when the blob carries no weights yet

	mu        sync.RWMutex
	weight    float64
	fleetRate float64
	fleetSize int

	// rate-gate reservation clock (guarded by its own mutex so refresh never blocks
	// a call that is mid-wait).
	gmu         sync.Mutex
	nextAllowed time.Time
}

// NewPriorityControls builds a throttle for this persona. defaultRate is the
// fallback sustainable fleet rate (calls/sec) when the operator hasn't set one —
// estimate it from concurrency / proposal timeout at the call site. defaultSize is
// the assumed number of gordons before any weights are published. rdb may be nil
// (open gate). It starts at weight 1 (normal) so it is a no-op until told otherwise.
func NewPriorityControls(rdb *redis.Client, personaID string, defaultRate float64, defaultSize int) *PriorityControls {
	if defaultSize < 1 {
		defaultSize = 1
	}
	return &PriorityControls{
		rdb:         rdb,
		personaID:   personaID,
		defaultRate: defaultRate,
		defaultSize: defaultSize,
		weight:      1.0,
		fleetRate:   defaultRate,
		fleetSize:   defaultSize,
	}
}

// run polls Redis for priority updates until ctx is cancelled. Nil-safe.
func (pc *PriorityControls) run(ctx context.Context, interval time.Duration) {
	if pc == nil || pc.rdb == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	pc.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pc.refresh(ctx)
		}
	}
}

func (pc *PriorityControls) refresh(ctx context.Context) {
	weight, size, rate := 1.0, pc.defaultSize, pc.defaultRate

	val, err := pc.rdb.Get(ctx, gordonPriorityKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return // transient Redis error: keep the last weight so a pause can't silently lift
	}
	if err == nil {
		var gp gordonPriority
		if json.Unmarshal([]byte(val), &gp) == nil {
			if gp.FleetRate > 0 {
				rate = gp.FleetRate
			}
			if n := len(gp.Weights); n > 0 {
				size = n
			}
			if w, ok := gp.Weights[pc.personaID]; ok {
				weight = w
			}
		}
	}
	if weight < 0 {
		weight = 0
	}

	pc.mu.Lock()
	changed := weight != pc.weight || size != pc.fleetSize || rate != pc.fleetRate
	pc.weight, pc.fleetSize, pc.fleetRate = weight, size, rate
	pc.mu.Unlock()

	if changed {
		iv, paused := pc.interval()
		switch {
		case paused:
			log.Printf("[PRIORITY] Persona %q PAUSED (weight 0) — proposals fall back to rules.", pc.personaID)
		case iv <= 0:
			log.Printf("[PRIORITY] Persona %q unthrottled (weight %.2f, no fleet rate).", pc.personaID, weight)
		default:
			log.Printf("[PRIORITY] Persona %q weight %.2f → min %s between generate calls (fleet %.3f/s ÷ %d).",
				pc.personaID, weight, iv.Round(time.Millisecond), rate, size)
		}
	}
}

// interval returns the current minimum spacing between generate calls and whether
// this persona is paused. A zero interval means "no throttle" (unknown rate).
func (pc *PriorityControls) interval() (time.Duration, bool) {
	pc.mu.RLock()
	w, n, rate := pc.weight, pc.fleetSize, pc.fleetRate
	pc.mu.RUnlock()

	if w <= 0 {
		return 0, true // paused
	}
	if rate <= 0 {
		return 0, false // no throttle configured
	}
	if n < 1 {
		n = 1
	}
	secs := (float64(n) / rate) / w // baseline share ÷ weight
	return time.Duration(secs * float64(time.Second)), false
}

// Wait blocks until this persona is allowed to make its next generate call, or
// returns an error the caller treats as "skip the model, fall back to rules":
// ErrPersonaPaused (weight 0) or ErrRateBudget (the next slot is further out than
// maxWait, so waiting would blow the proposal deadline). A nil receiver or an
// unthrottled persona returns immediately. maxWait ≤ 0 means wait however long the
// spacing requires (still bounded by ctx).
func (pc *PriorityControls) Wait(ctx context.Context, maxWait time.Duration) error {
	if pc == nil {
		return nil
	}
	iv, paused := pc.interval()
	if paused {
		return ErrPersonaPaused
	}
	if iv <= 0 {
		return nil
	}

	// Reserve the next slot. Give up WITHOUT consuming it if it's beyond maxWait, so
	// a demoted gordon cleanly falls back rather than parking a slot it won't use.
	pc.gmu.Lock()
	now := time.Now()
	if pc.nextAllowed.Before(now) {
		pc.nextAllowed = now
	}
	wait := pc.nextAllowed.Sub(now)
	if maxWait > 0 && wait > maxWait {
		pc.gmu.Unlock()
		return ErrRateBudget
	}
	pc.nextAllowed = pc.nextAllowed.Add(iv)
	pc.gmu.Unlock()

	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Paused reports whether this persona is currently paused (weight 0). It is the
// cross-cutting half of the promote/demote knob: a paused persona skips the model
// on ANY backend and falls back to rules, independent of the per-backend rate
// spacing that Wait enforces. Nil-safe (a nil receiver is never paused).
func (pc *PriorityControls) Paused() bool {
	if pc == nil {
		return false
	}
	_, paused := pc.interval()
	return paused
}

// Snapshot returns the current weight, effective interval, and paused flag for
// health/telemetry. Nil-safe.
func (pc *PriorityControls) Snapshot() (weight float64, interval time.Duration, paused bool) {
	if pc == nil {
		return 1.0, 0, false
	}
	pc.mu.RLock()
	w := pc.weight
	pc.mu.RUnlock()
	iv, p := pc.interval()
	return w, iv, p
}
