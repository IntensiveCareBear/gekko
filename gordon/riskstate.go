package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// riskState mirrors the JSON breen publishes to gekko:risk_state. Only the fields the
// pre-LLM veto needs are decoded.
type riskState struct {
	Halted              bool             `json:"halted"`
	HaltReason          string           `json:"halt_reason"`
	HeldFamilies        []string         `json:"held_families"`
	HeldContracts       []string         `json:"held_contracts"` // nil ⇒ old-format blob (breen predates the field)
	CooldownUntilMs     map[string]int64 `json:"cooldown_until_ms"`
	LossLimitedFamilies []string         `json:"loss_limited_families"`
}

// RiskStateReader reads breen's published risk state and answers "would a NEW ENTRY on
// this ticker be deterministically rejected right now?" so Gordon can skip the LLM call.
// It caches the blob for a short TTL to avoid hitting Redis on every event, and is
// fail-open: any missing key / Redis error / decode failure yields an empty state, so
// nothing is vetoed and behavior is identical to before this gate existed.
type RiskStateReader struct {
	rdb *redis.Client
	ttl time.Duration

	mu           sync.Mutex
	cached       riskState
	heldSet      map[string]bool
	heldContract map[string]bool // nil when the blob predates held_contracts
	lossSet      map[string]bool
	fetchedAt    time.Time
	haveOnce     bool
	loaded       bool // last refresh decoded a real snapshot (vs. absent key / error)
}

// NewRiskStateReader returns a reader, or nil when no Redis is configured (pre-gate off).
func NewRiskStateReader(redisAddr string) *RiskStateReader {
	if redisAddr == "" {
		return nil
	}
	return &RiskStateReader{
		rdb: redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second}),
		ttl: 3 * time.Second,
	}
}

// contractFamily strips the trailing strike/variant segment so a strike ladder shares
// one identity. Mirrors breen's contractFamily exactly (must stay in sync).
func contractFamily(contractID string) string {
	parts := strings.Split(contractID, "-")
	if len(parts) >= 3 {
		return strings.Join(parts[:len(parts)-1], "-")
	}
	return contractID
}

// refresh reloads the cached state from Redis when the TTL has elapsed. Caller holds mu.
func (r *RiskStateReader) refresh(ctx context.Context) {
	if r.haveOnce && time.Since(r.fetchedAt) < r.ttl {
		return
	}
	r.fetchedAt = time.Now()
	getCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	val, err := r.rdb.Get(getCtx, "gekko:risk_state").Result()
	if err != nil || val == "" {
		// Fail-open: keep no state (nothing vetoed). Do not clobber a prior good read
		// on a transient error — but do clear so a genuinely absent key stops vetoing.
		r.cached, r.heldSet, r.heldContract, r.lossSet, r.loaded = riskState{}, nil, nil, nil, false
		r.haveOnce = true
		return
	}
	var rs riskState
	if json.Unmarshal([]byte(val), &rs) != nil {
		r.cached, r.heldSet, r.heldContract, r.lossSet, r.loaded = riskState{}, nil, nil, nil, false
		r.haveOnce = true
		return
	}
	held := make(map[string]bool, len(rs.HeldFamilies))
	for _, f := range rs.HeldFamilies {
		held[f] = true
	}
	// held_contracts absent (nil) means an old-format blob: leave heldContract nil so
	// HeldContract falls back to family granularity instead of treating all exact
	// lookups as "not held" (which would suppress every valid exit).
	var heldC map[string]bool
	if rs.HeldContracts != nil {
		heldC = make(map[string]bool, len(rs.HeldContracts))
		for _, c := range rs.HeldContracts {
			heldC[c] = true
		}
	}
	loss := make(map[string]bool, len(rs.LossLimitedFamilies))
	for _, f := range rs.LossLimitedFamilies {
		loss[f] = true
	}
	r.cached, r.heldSet, r.heldContract, r.lossSet, r.haveOnce, r.loaded = rs, held, heldC, loss, true, true
}

// HeldFamily reports whether the ticker's family has an open position, and whether the
// answer is authoritative (a real risk-state snapshot was decoded, not an absent key or
// Redis error). Callers dropping proposals on "not held" — e.g. the SELL-means-sell
// gate — must treat known=false as "pass through", so a breen outage can't silently
// suppress valid exits.
func (r *RiskStateReader) HeldFamily(ctx context.Context, ticker string) (held, known bool) {
	if r == nil {
		return false, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh(ctx)
	if !r.loaded {
		return false, false
	}
	return r.heldSet[contractFamily(ticker)], true
}

// HeldContract reports whether this EXACT contract has an open position. The SELL-
// means-sell gate needs contract granularity: family-level matching let sells of
// sibling strikes through as "exits" (hold KXGDP-...-T2.0 ⇒ sells of T0.0/T0.5/T1.0
// executed and no-op'd all day, 2026-07-12). Falls back to family granularity when
// breen's blob predates held_contracts, and to known=false on outage — callers must
// pass proposals through in that case so exits are never suppressed.
func (r *RiskStateReader) HeldContract(ctx context.Context, ticker string) (held, known bool) {
	if r == nil {
		return false, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh(ctx)
	if !r.loaded {
		return false, false
	}
	if r.heldContract == nil {
		return r.heldSet[contractFamily(ticker)], true
	}
	return r.heldContract[ticker], true
}

// Evaluate reports whether Gordon holds no position in the ticker's family (held) and
// whether a new entry there would be deterministically rejected (blocked, with reason).
// When the family IS held, blocked is always false: a proposal there could be a valid
// exit (SELLs always pass breen's gate) or a bypass-cooldown add, so it must reach the
// LLM and let breen decide.
func (r *RiskStateReader) Evaluate(ctx context.Context, ticker string) (held, blocked bool, reason string) {
	if r == nil {
		return false, false, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh(ctx)

	fam := contractFamily(ticker)
	if r.heldSet[fam] {
		return true, false, ""
	}
	if r.cached.Halted {
		return false, true, "drawdown halt (" + r.cached.HaltReason + ")"
	}
	if until, ok := r.cached.CooldownUntilMs[fam]; ok && time.Now().UnixMilli() < until {
		return false, true, fmt.Sprintf("family %s in re-entry cooldown", fam)
	}
	if r.lossSet[fam] {
		return false, true, fmt.Sprintf("family %s past loss limit", fam)
	}
	return false, false, ""
}
