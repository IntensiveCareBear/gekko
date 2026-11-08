package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// gordonConfigPrefix keys the per-gordon editable config the Gordons tab writes and
// each gordon polls. One JSON blob per persona id. Unlike gekko:model_controls (a
// single global blob) and gekko:gordon_priority (fleet weights), this holds one
// gordon's full personality override: prompt nudge + stance/risk/threshold + model.
//
// It is deliberately NOT TTL'd and NOT cleared on run reset (resetRun only truncates
// the ledger + equity/trades keys), so operator tuning survives resets and new dry
// runs — the Gordons tab "Reset to default" button is the only thing that deletes it.
const gordonConfigPrefix = "gekko:gordon_config:"

// GordonConfigKey returns the Redis key for a persona's editable config.
func GordonConfigKey(personaID string) string { return gordonConfigPrefix + personaID }

// gordonConfigBlob is the sparse override stored in Redis. Empty/zero fields fall
// through to the compiled persona registry defaults, so the blob only records what
// the operator actually changed.
type gordonConfigBlob struct {
	DisplayName    string  `json:"display_name"`
	Nudge          string  `json:"nudge"`
	Stance         string  `json:"stance"`
	RiskTier       string  `json:"risk_tier"`
	MatchThreshold float64 `json:"match_threshold"`
	Model          string  `json:"model"`
	Provider       string  `json:"provider"`
	UpdatedAtMs    int64   `json:"updated_at_ms"`
}

// GordonConfig polls gekko:gordon_config:<id> and exposes this gordon's live-editable
// personality (nudge/stance/risk/threshold). Model/provider are read once at startup
// (a backend swap needs a rebuild), so they are not surfaced by the live getters here.
//
// It seeds from the resolved Persona at construction, so an absent/малformed key ==
// exactly today's compiled behavior (fail-safe). When the nudge changes it calls
// onNudgeChange so the ProposalClient's prompt updates without a restart.
type GordonConfig struct {
	rdb *redis.Client
	key string
	def Persona // compiled defaults for fallback

	mu        sync.RWMutex
	nudgeSink func(string) // pushes nudge changes into the live prompt
	nudge     string
	// effective personality, already merged with defaults + sanitized.
	stance    string
	riskTier  string
	threshold float64
	// startup-only overrides (a backend swap needs a rebuild, so these are read once
	// at boot and not surfaced by the live getters). Empty ⇒ use env/registry.
	model    string
	provider string
}

// NewGordonConfig builds the poller seeded from the persona registry defaults. rdb
// may be nil (fully static: run() no-ops, getters return the compiled defaults).
func NewGordonConfig(redisAddr string, def Persona) *GordonConfig {
	var rdb *redis.Client
	if redisAddr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second})
	}
	return &GordonConfig{
		rdb:       rdb,
		key:       GordonConfigKey(def.ID),
		def:       def,
		nudge:     def.Nudge,
		stance:    def.Stance,
		riskTier:  def.RiskTier,
		threshold: def.Threshold,
	}
}

// LoadOnce does a single synchronous read so startup can resolve the model/provider
// override before the backends are built. Nil-safe.
func (gc *GordonConfig) LoadOnce(ctx context.Context) {
	if gc == nil || gc.rdb == nil {
		return
	}
	gc.refresh(ctx)
}

// SetNudgeSink registers the callback that installs the persona nudge into the live
// prompt, and fires it once with the current value. Call after the ProposalClient
// exists. Nil-safe.
func (gc *GordonConfig) SetNudgeSink(fn func(string)) {
	if gc == nil || fn == nil {
		return
	}
	gc.mu.Lock()
	gc.nudgeSink = fn
	nudge := gc.nudge
	gc.mu.Unlock()
	fn(nudge)
}

// Model returns the startup-only model override ("" ⇒ use env/registry). Nil-safe.
func (gc *GordonConfig) Model() string {
	if gc == nil {
		return ""
	}
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.model
}

// Provider returns the startup-only backend override ("" ⇒ use env/registry). Nil-safe.
func (gc *GordonConfig) Provider() string {
	if gc == nil {
		return ""
	}
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.provider
}

// run polls Redis for config updates until ctx is cancelled. Nil-safe.
func (gc *GordonConfig) run(ctx context.Context, interval time.Duration) {
	if gc == nil || gc.rdb == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	gc.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			gc.refresh(ctx)
		}
	}
}

func (gc *GordonConfig) refresh(ctx context.Context) {
	// Absent/unreadable key ⇒ revert to compiled defaults (covers a "nuke" delete).
	blob := gordonConfigBlob{}
	if val, err := gc.rdb.Get(ctx, gc.key).Result(); err == nil {
		if json.Unmarshal([]byte(val), &blob) != nil {
			return // malformed — keep current state rather than resetting on a bad write
		}
	}
	gc.apply(gc.merge(blob))
}

// merge layers the sparse override on top of the compiled defaults and sanitizes the
// result, so empty override fields keep the registry personality.
func (gc *GordonConfig) merge(b gordonConfigBlob) gordonConfigBlob {
	if b.Nudge == "" {
		b.Nudge = gc.def.Nudge
	}
	switch b.Stance {
	case StanceBearish, StanceNeutral, StanceBullish:
	default:
		b.Stance = gc.def.Stance
	}
	if _, ok := riskTiers[b.RiskTier]; !ok {
		b.RiskTier = gc.def.RiskTier
	}
	if b.MatchThreshold < 0.35 || b.MatchThreshold > 0.80 {
		b.MatchThreshold = gc.def.Threshold
	}
	return b
}

// apply installs merged state, logging + firing the nudge sink on change.
func (gc *GordonConfig) apply(b gordonConfigBlob) {
	gc.mu.Lock()
	nudgeChanged := b.Nudge != gc.nudge
	changed := nudgeChanged || b.Stance != gc.stance || b.RiskTier != gc.riskTier || b.MatchThreshold != gc.threshold
	gc.nudge, gc.stance, gc.riskTier, gc.threshold = b.Nudge, b.Stance, b.RiskTier, b.MatchThreshold
	gc.model, gc.provider = b.Model, b.Provider
	sink, nudge := gc.nudgeSink, gc.nudge
	gc.mu.Unlock()

	if nudgeChanged && sink != nil {
		sink(nudge)
	}
	if changed {
		log.Printf("[INFO] Gordon config updated for %q: stance=%s risk=%s threshold=%.2f (nudge %d chars)",
			gc.def.ID, b.Stance, b.RiskTier, b.MatchThreshold, len(b.Nudge))
	}
}

// Stance returns the live directional stance (falls back to the compiled default).
func (gc *GordonConfig) Stance() string {
	if gc == nil {
		return ""
	}
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.stance
}

// RiskTier returns the live risk tier.
func (gc *GordonConfig) RiskTier() string {
	if gc == nil {
		return ""
	}
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.riskTier
}

// Threshold returns the live news→market match threshold.
func (gc *GordonConfig) Threshold() float64 {
	if gc == nil {
		return 0
	}
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.threshold
}
