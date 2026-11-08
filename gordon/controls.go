package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// modelControlsKey is the Redis key liveview writes and gordon reads to steer the
// model live (no restart). See gordon/liveview handleModelControls.
const modelControlsKey = "gekko:model_controls"

// Stance values bias the trade direction Gordon is willing to take.
const (
	StanceBearish = "bearish"
	StanceNeutral = "neutral"
	StanceBullish = "bullish"
)

// Risk tiers map to portfolio size/exposure caps.
const (
	RiskConservative = "conservative"
	RiskCautious     = "cautious"
	RiskBalanced     = "balanced"
	RiskAssertive    = "assertive"
	RiskAggressive   = "aggressive"
)

// riskTiers maps a tier name to its position-size / exposure caps. Balanced
// mirrors the historical PORTFOLIO_MAX_* defaults; cautious and assertive fill
// the gaps so the operator can step exposure up gradually instead of doubling.
var riskTiers = map[string]struct {
	MaxSize int
	MaxExp  float64
}{
	RiskConservative: {MaxSize: 1000, MaxExp: 4000.0},
	RiskCautious:     {MaxSize: 1750, MaxExp: 6000.0},
	RiskBalanced:     {MaxSize: 2500, MaxExp: 8000.0},
	RiskAssertive:    {MaxSize: 3750, MaxExp: 12000.0},
	RiskAggressive:   {MaxSize: 5000, MaxExp: 16000.0},
}

// modelControls is the JSON blob exchanged via Redis.
type modelControls struct {
	Stance         string  `json:"stance"`
	RiskTier       string  `json:"risk_tier"`
	MatchThreshold float64 `json:"match_threshold"`
	MaxSlippagePct float64 `json:"max_slippage_pct"`
	UpdatedAtMs    int64   `json:"updated_at_ms"`
}

// RedisControls polls gekko:model_controls and exposes the current model knobs.
// It implements LimitsReader so the LiquidityEngine picks up the risk tier on
// every Evaluate with no further wiring. When the key is absent/malformed it
// falls back to the defaults seeded from env at construction.
type RedisControls struct {
	rdb        *redis.Client
	maxSlip    float64 // sanitize fallback for the slippage knob; from PORTFOLIO_MAX_SLIP
	defaultThr float64
	// slipOnly restricts refresh to the max-slippage field: static personas hold a
	// fixed personality (stance/risk/threshold) but slippage is a fleet-wide risk
	// cap, not a personality trait, so they still track that one knob.
	slipOnly bool

	// cfg, when set (named personas only), is the per-gordon live-editable config
	// from the Gordons tab. Its stance/risk/threshold override the static seed so an
	// operator can retune a persona without a restart. Nil for the default persona,
	// which tracks the global gekko:model_controls blob instead.
	cfg *GordonConfig

	mu    sync.RWMutex
	state modelControls
}

// SetGordonConfig attaches a per-gordon config so this control's stance/risk/threshold
// track the operator's live edits. Only meaningful for static (named) personas.
func (rc *RedisControls) SetGordonConfig(cfg *GordonConfig) { rc.cfg = cfg }

// NewRedisControls seeds defaults from env-derived values and starts a poller.
// defaultSize/defaultExp pick the starting risk tier (nearest match, else balanced).
func NewRedisControls(redisAddr string, defaultSize int, defaultExp, maxSlip, defaultThreshold float64) *RedisControls {
	var rdb *redis.Client
	if redisAddr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second})
	}
	rc := &RedisControls{
		rdb:        rdb,
		maxSlip:    maxSlip,
		defaultThr: defaultThreshold,
		state: modelControls{
			Stance:         StanceNeutral,
			RiskTier:       tierForLimits(defaultSize, defaultExp),
			MatchThreshold: defaultThreshold,
			MaxSlippagePct: maxSlip,
		},
	}
	return rc
}

// NewStaticControls returns a controls object with a fixed personality seeded from
// a persona's stance/risk/threshold. Used by non-default Gordon personas. Unlike
// the default persona it ignores the global stance/risk/threshold knobs, but it
// still polls gekko:model_controls for the max-slippage cap (slipOnly) — slippage
// is a fleet-wide risk limit, not a personality trait. Empty redisAddr ⇒ fully
// static (run() no-ops).
func NewStaticControls(redisAddr, stance, riskTier string, threshold float64) *RedisControls {
	var rdb *redis.Client
	if redisAddr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second})
	}
	rc := &RedisControls{
		rdb:        rdb,
		maxSlip:    0.03,
		defaultThr: threshold,
		slipOnly:   true,
		state: modelControls{
			Stance:         stance,
			RiskTier:       riskTier,
			MatchThreshold: threshold,
			MaxSlippagePct: 0.03,
		},
	}
	rc.state = rc.sanitize(rc.state)
	return rc
}

// run polls Redis for control updates until ctx is cancelled.
func (rc *RedisControls) run(ctx context.Context, interval time.Duration) {
	if rc.rdb == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	rc.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rc.refresh(ctx)
		}
	}
}

func (rc *RedisControls) refresh(ctx context.Context) {
	val, err := rc.rdb.Get(ctx, modelControlsKey).Result()
	if err != nil {
		return // key absent — keep current/default state
	}
	var mc modelControls
	if json.Unmarshal([]byte(val), &mc) != nil {
		return
	}
	rc.apply(rc.sanitize(mc))
}

// apply installs a sanitized blob as the current state, logging when anything
// changed. slipOnly controls adopt only the fleet-wide slippage cap and keep
// their fixed personality.
func (rc *RedisControls) apply(mc modelControls) {
	rc.mu.Lock()
	if rc.slipOnly {
		slip := mc.MaxSlippagePct
		mc = rc.state
		mc.MaxSlippagePct = slip
	}
	changed := mc.Stance != rc.state.Stance || mc.RiskTier != rc.state.RiskTier ||
		mc.MatchThreshold != rc.state.MatchThreshold || mc.MaxSlippagePct != rc.state.MaxSlippagePct
	rc.state = mc
	rc.mu.Unlock()

	if changed {
		tier := riskTiers[mc.RiskTier]
		log.Printf("[INFO] Model controls updated: stance=%s risk=%s (size<=%d, exp<=$%.0f) threshold=%.2f max_slip=%.3f",
			mc.Stance, mc.RiskTier, tier.MaxSize, tier.MaxExp, mc.MatchThreshold, mc.MaxSlippagePct)
	}
}

// sanitize coerces unknown/out-of-range values back to safe defaults.
func (rc *RedisControls) sanitize(mc modelControls) modelControls {
	switch mc.Stance {
	case StanceBearish, StanceNeutral, StanceBullish:
	default:
		mc.Stance = StanceNeutral
	}
	if _, ok := riskTiers[mc.RiskTier]; !ok {
		mc.RiskTier = RiskBalanced
	}
	if mc.MatchThreshold < 0.35 || mc.MatchThreshold > 0.80 {
		mc.MatchThreshold = rc.defaultThr
	}
	// Covers blobs written before the field existed (zero) as well as junk values.
	if mc.MaxSlippagePct < 0.001 || mc.MaxSlippagePct > 0.10 {
		mc.MaxSlippagePct = rc.maxSlip
	}
	return mc
}

// ReadLimits implements LimitsReader from the current risk tier and slippage cap.
// The risk tier prefers the per-gordon live config when attached; slippage always
// comes from the fleet-wide blob.
func (rc *RedisControls) ReadLimits() (*PortfolioLimits, error) {
	rc.mu.RLock()
	tier := rc.state.RiskTier
	slip := rc.state.MaxSlippagePct
	rc.mu.RUnlock()
	if rc.cfg != nil {
		tier = rc.cfg.RiskTier()
	}
	t, ok := riskTiers[tier]
	if !ok {
		t = riskTiers[RiskBalanced]
	}
	// BET_SIZE_MULT inflates the order before Evaluate caps it, so the tier caps
	// scale with it; otherwise every multiplied order truncates and reads as a breach.
	return &PortfolioLimits{
		MaxPositionSize: int(float64(t.MaxSize) * betSizeMult),
		MaxExposure:     t.MaxExp * betSizeMult,
		MaxSlippagePct:  slip,
	}, nil
}

// Threshold returns the current news→market match threshold, preferring the
// per-gordon live config when attached.
func (rc *RedisControls) Threshold() float64 {
	if rc.cfg != nil {
		return rc.cfg.Threshold()
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.state.MatchThreshold
}

// Stance returns the current directional stance (bearish/neutral/bullish),
// preferring the per-gordon live config when attached.
func (rc *RedisControls) Stance() string {
	if rc.cfg != nil {
		return rc.cfg.Stance()
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.state.Stance
}

// RiskTier returns the current risk-tier name (conservative/balanced/aggressive),
// preferring the per-gordon live config when attached. Surfaced for health/telemetry.
func (rc *RedisControls) RiskTier() string {
	if rc.cfg != nil {
		return rc.cfg.RiskTier()
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.state.RiskTier
}

// tierForLimits picks the named tier whose caps match the given env defaults,
// falling back to balanced when nothing matches.
func tierForLimits(size int, exp float64) string {
	for name, t := range riskTiers {
		if t.MaxSize == size && t.MaxExp == exp {
			return name
		}
	}
	return RiskBalanced
}

// stanceDemoteFactor returns the size multiplier for a proposed action under the
// given stance. Agreeing or neutral stances leave size unchanged (1.0); an action
// opposing the stance is softly demoted (sized down) rather than dropped — the
// bot stays responsive in a one-sided market.
func stanceDemoteFactor(stance, action string) float64 {
	const demote = 0.4
	switch strings.ToLower(stance) {
	case StanceBullish:
		if strings.EqualFold(action, "SELL") {
			return demote
		}
	case StanceBearish:
		if strings.EqualFold(action, "BUY") {
			return demote
		}
	}
	return 1.0
}
