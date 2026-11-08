package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// activityChannel is the Redis pub/sub channel gordon's activity trigger subscribes to.
const activityChannel = "gekko:market_activity"

// ActivityEvent says "this market just moved", the trigger for gordon to look at it
// (docs/gordons-creed-proposal.md, Phase 2): activity first, then the ingested news.
type ActivityEvent struct {
	Ticker    string  `json:"ticker"`
	Kind      string  `json:"kind"`      // "trend" | "vol_surge"
	Magnitude float64 `json:"magnitude"` // |trend %| for trend, the surge multiple for vol_surge
	TrendPct  float64 `json:"trend_pct"`
	VolSurge  float64 `json:"vol_surge"`
	Mid       float64 `json:"mid"`
	Bid       float64 `json:"bid"`
	Ask       float64 `json:"ask"`
	TsMs      int64   `json:"ts_ms"`
}

type activityConfig struct {
	TrendPct   float64       // |trend| over the momentum window must reach this %
	MinMove    float64       // ...and the mid must have moved this far in price units (penny books are noise)
	VolSurge   float64       // top-of-book size ÷ window average
	MinSamples int           // momentum window samples required
	Debounce   time.Duration // per-ticker quiet period after an event
	MinMid     float64       // ignore settled-looking books outside [MinMid, 1-MinMid]
}

func activityEnvFloat(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return d
}

func loadActivityConfig() activityConfig {
	deb := 10 * time.Minute
	if d, err := time.ParseDuration(os.Getenv("ACTIVITY_DEBOUNCE")); err == nil && d > 0 {
		deb = d
	}
	return activityConfig{
		TrendPct:   activityEnvFloat("ACTIVITY_TREND_PCT", 8),
		MinMove:    activityEnvFloat("ACTIVITY_MIN_MOVE", 0.03),
		VolSurge:   activityEnvFloat("ACTIVITY_VOL_SURGE", 3),
		MinSamples: int(activityEnvFloat("ACTIVITY_MIN_SAMPLES", 6)),
		Debounce:   deb,
		MinMid:     activityEnvFloat("ACTIVITY_MIN_MID", 0.03),
	}
}

type activityDetector struct {
	cfg  activityConfig
	mu   sync.Mutex
	last map[string]time.Time
}

func newActivityDetector(cfg activityConfig) *activityDetector {
	return &activityDetector{cfg: cfg, last: map[string]time.Time{}}
}

var activity = newActivityDetector(loadActivityConfig())

// check turns a momentum snapshot into an event when the market has genuinely moved. Pure apart
// from the per-ticker debounce clock.
func (d *activityDetector) check(m Momentum, bid, ask float64, now time.Time) (ActivityEvent, bool) {
	c := d.cfg
	if m.Samples < c.MinSamples || m.Mid < c.MinMid || m.Mid > 1-c.MinMid || bid <= 0 || ask <= 0 {
		return ActivityEvent{}, false
	}
	// price change over the window, recovered from the percentage trend
	move := 0.0
	if m.TrendPct > -100 {
		move = m.Mid - m.Mid/(1+m.TrendPct/100)
	}
	ev := ActivityEvent{Ticker: m.Ticker, TrendPct: m.TrendPct, VolSurge: m.VolSurge, Mid: m.Mid, Bid: bid, Ask: ask, TsMs: now.UnixMilli()}
	switch {
	case math.Abs(m.TrendPct) >= c.TrendPct && math.Abs(move) >= c.MinMove:
		ev.Kind, ev.Magnitude = "trend", math.Abs(m.TrendPct)
	case c.VolSurge > 0 && m.VolSurge >= c.VolSurge:
		ev.Kind, ev.Magnitude = "vol_surge", m.VolSurge
	default:
		return ActivityEvent{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.last[m.Ticker]; ok && now.Sub(t) < c.Debounce {
		return ActivityEvent{}, false
	}
	d.last[m.Ticker] = now
	return ev, true
}

// publishActivity emits the event on the pub/sub channel. Fire and forget: with no subscriber
// (gordon's trigger off) it costs one Redis PUBLISH.
func publishActivity(ctx context.Context, rdb *redis.Client, ev ActivityEvent) {
	if b, err := json.Marshal(ev); err == nil {
		_ = rdb.Publish(ctx, activityChannel, b).Err()
	}
}
