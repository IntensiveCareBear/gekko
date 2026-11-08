package main

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Momentum is the short-horizon price/volume signal marketdata publishes per
// contract to momentum:<ticker>. Gordon folds it into proposal confidence.
type Momentum struct {
	Ticker    string  `json:"ticker"`
	Mid       float64 `json:"mid"`       // latest top-of-book mid
	TrendPct  float64 `json:"trend_pct"` // (latest-oldest)/oldest over the window, %
	VolSurge  float64 `json:"vol_surge"` // latest top-of-book size ÷ window-avg size
	Samples   int     `json:"samples"`   // points in the window
	UpdatedMs int64   `json:"updated_ms"`
	// Imbalance is the signed top-N depth imbalance (bidVol−askVol)/(bidVol+askVol)
	// in [-1,1]: +1 all bids (bullish pressure), −1 all asks (bearish). ImbalanceConf
	// is a [0,1] reliability discount — thin or wide books make the signal noisy
	// (clodds-mining ADOPT #5). Snapshot, not windowed.
	Imbalance     float64 `json:"imbalance,omitempty"`
	ImbalanceConf float64 `json:"imbalance_conf,omitempty"`
}

const momentumWindow = 12 // ~1 min of history at a 5s poll

// Order-book imbalance parameters (clodds-mining ADOPT #5). Thin Kalshi books make
// imbalance noisy, so total depth and spread tightness discount the signal.
const (
	imbalanceLevels    = 5    // top-N book levels summed on each side
	imbalanceVolSat    = 200  // total contracts of depth for a full volume score
	imbalanceSpreadRef = 0.10 // spread (price units) at which the spread score hits 0
)

// orderbookImbalance computes the signed top-N depth imbalance and a reliability
// score, following CloddsBot's calculateOrderbookImbalance. reliability blends total
// depth (thin books discounted), spread tightness, and imbalance magnitude. Pure.
func orderbookImbalance(ob *OrderBook) (imbalance, reliability float64) {
	if ob == nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return 0, 0
	}
	var bidVol, askVol int
	for i := 0; i < len(ob.Bids) && i < imbalanceLevels; i++ {
		bidVol += ob.Bids[i].Volume
	}
	for i := 0; i < len(ob.Asks) && i < imbalanceLevels; i++ {
		askVol += ob.Asks[i].Volume
	}
	total := bidVol + askVol
	if total <= 0 {
		return 0, 0
	}
	imbalance = float64(bidVol-askVol) / float64(total)
	volumeScore := math.Min(1.0, float64(total)/imbalanceVolSat)
	spread := ob.Asks[0].Price - ob.Bids[0].Price
	spreadScore := math.Max(0, 1.0-spread/imbalanceSpreadRef)
	reliability = math.Max(0, math.Min(1, 0.5*volumeScore+0.3*spreadScore+0.2*math.Abs(imbalance)))
	return imbalance, reliability
}

// momentumTracker keeps a rolling per-ticker history of mid prices and top-of-book
// sizes, in-memory in the marketdata process (both pollers share it).
type momentumTracker struct {
	mu   sync.Mutex
	mids map[string][]float64
	vols map[string][]float64
}

func newMomentumTracker() *momentumTracker {
	return &momentumTracker{mids: map[string][]float64{}, vols: map[string][]float64{}}
}

var momentum = newMomentumTracker()

// observe appends a sample and returns the current momentum snapshot.
func (m *momentumTracker) observe(ticker string, mid, topVol float64) Momentum {
	m.mu.Lock()
	defer m.mu.Unlock()

	push := func(store map[string][]float64, v float64) []float64 {
		s := append(store[ticker], v)
		if len(s) > momentumWindow {
			s = s[len(s)-momentumWindow:]
		}
		store[ticker] = s
		return s
	}
	mids := push(m.mids, mid)
	vols := push(m.vols, topVol)

	out := Momentum{Ticker: ticker, Mid: mid, Samples: len(mids), UpdatedMs: time.Now().UnixMilli()}
	if oldest := mids[0]; oldest > 0 && len(mids) >= 2 {
		out.TrendPct = (mid - oldest) / oldest * 100.0
	}
	if len(vols) >= 2 {
		var sum float64
		for _, v := range vols {
			sum += v
		}
		if avg := sum / float64(len(vols)); avg > 0 {
			out.VolSurge = topVol / avg
		}
	}
	return out
}

// publishMomentum records a fresh order-book observation and writes the signal to
// Redis. No-ops when the book has no two-sided top of book.
func publishMomentum(ctx context.Context, rdb *redis.Client, ticker string, ob *OrderBook, ttl time.Duration) {
	if rdb == nil || ob == nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return
	}
	mid := (ob.Bids[0].Price + ob.Asks[0].Price) / 2.0
	topVol := float64(ob.Bids[0].Volume + ob.Asks[0].Volume)
	snap := momentum.observe(ticker, mid, topVol)
	snap.Imbalance, snap.ImbalanceConf = orderbookImbalance(ob)
	if data, err := json.Marshal(snap); err == nil {
		_ = rdb.Set(ctx, "momentum:"+ticker, string(data), ttl).Err()
	}
	if ev, ok := activity.check(snap, ob.Bids[0].Price, ob.Asks[0].Price, time.Now()); ok {
		publishActivity(ctx, rdb, ev)
	}
}
