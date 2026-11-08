package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	kgo "github.com/twmb/franz-go/pkg/kgo"
)

// Activity trigger (docs/gordons-creed-proposal.md, Phase 2): marketdata publishes an event when
// a market moves, and this makes gordon look at that market, instead of only reacting to
// headlines. The event enters the same funnel as a news item (pre-gate, RAG lookup of what we
// have already ingested about the market, LLM, edge gates, breen), with the ticker already known.
// Off unless ACTIVITY_TRIGGER=true.

const activityChannel = "gekko:market_activity"

type activityEvent struct {
	Ticker    string  `json:"ticker"`
	Kind      string  `json:"kind"`
	Magnitude float64 `json:"magnitude"`
	TrendPct  float64 `json:"trend_pct"`
	VolSurge  float64 `json:"vol_surge"`
	Mid       float64 `json:"mid"`
	Bid       float64 `json:"bid"`
	Ask       float64 `json:"ask"`
	TsMs      int64   `json:"ts_ms"`
}

// matchHint pre-fills the market match for an event that already names its market.
type matchHint struct {
	ticker, title string
	bid, ask      float64
	confidence    float64
}

// activityGate decides which events reach the LLM: at most one evaluation per contract family per
// cooldown, so a churning ladder cannot burn the model budget. Safe for concurrent use.
type activityGate struct {
	cooldown time.Duration
	mu       sync.Mutex
	last     map[string]time.Time
}

func newActivityGate(cooldown time.Duration) *activityGate {
	return &activityGate{cooldown: cooldown, last: map[string]time.Time{}}
}

// admit reports whether the event may be evaluated, recording the family as evaluated if so.
func (g *activityGate) admit(ev activityEvent, now time.Time) (bool, string) {
	if ev.Ticker == "" || ev.Bid <= 0 || ev.Ask <= 0 || ev.Ask < ev.Bid {
		return false, "malformed or one-sided"
	}
	fam := contractFamily(ev.Ticker)
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok := g.last[fam]; ok && now.Sub(t) < g.cooldown {
		return false, fmt.Sprintf("family %s evaluated %v ago (cooldown %v)", fam, now.Sub(t).Round(time.Second), g.cooldown)
	}
	g.last[fam] = now
	return true, ""
}

// activityContent is the text the funnel treats as the "headline": what moved and by how much.
func activityContent(title string, ev activityEvent) string {
	what := fmt.Sprintf("YES moved %+.1f%% over the last minute to %.2f", ev.TrendPct, ev.Mid)
	if ev.Kind == "vol_surge" {
		what = fmt.Sprintf("top-of-book size surged %.1fx its recent average at %.2f", ev.VolSurge, ev.Mid)
	}
	return fmt.Sprintf("Market activity: %s. %s (bid %.2f / ask %.2f).", strings.TrimSpace(title), what, ev.Bid, ev.Ask)
}

func activityHash(ev activityEvent) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("activity|%s|%s|%d", ev.Ticker, ev.Kind, ev.TsMs)))
	return "act-" + hex.EncodeToString(h[:6])
}

// Lookup returns the catalog title for a ticker. The caller supplies the live bid/ask.
func (m *MarketMatcher) Lookup(ticker string) (title string, ok bool) {
	if m == nil {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	title, ok = m.titles[ticker]
	return
}

type queuedActivity struct {
	ev   GekkoIngestEvent
	hint *matchHint
}

// startActivityTrigger subscribes to marketdata's activity channel and feeds admitted events to
// the funnel one at a time. Returns immediately when the trigger is off.
func startActivityTrigger(ctx context.Context, redisAddr string, tr *TelemetryRegistry, engine *LiquidityEngine,
	prodClient *kgo.Client, outboundTopic string, oc *ProposalClient, qc *QdrantClient) {
	if !getEnvBool("ACTIVITY_TRIGGER", false) || weatherMode {
		return
	}
	gate := newActivityGate(getEnvDuration("ACTIVITY_FAMILY_COOLDOWN", 10*time.Minute))
	queue := make(chan queuedActivity, getEnvInt("ACTIVITY_QUEUE", 16))
	var dropped, seen, admitted atomic.Int64

	go func() { // worker: one evaluation at a time, same as the Kafka loop
		for {
			select {
			case <-ctx.Done():
				return
			case q := <-queue:
				processParsedEvent(ctx, q.ev, q.hint, tr, engine, prodClient, outboundTopic, oc, qc)
			}
		}
	}()

	go func() {
		rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
		defer rdb.Close()
		for ctx.Err() == nil {
			ps := rdb.Subscribe(ctx, activityChannel)
			tr.AddProposalLog(fmt.Sprintf("[ACTIVITY] trigger on: channel %s, family cooldown %v", activityChannel, gate.cooldown))
			for msg := range ps.Channel() {
				var ev activityEvent
				if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
					continue
				}
				seen.Add(1)
				if oc.Paused() {
					continue
				}
				title, ok := marketMatcher.Lookup(ev.Ticker)
				if !ok {
					continue // not a market gordon's catalog knows
				}
				if strings.HasPrefix(ev.Ticker, weatherSeriesPrefix) {
					continue // weather buckets belong to the deterministic weather gordon, as in Match
				}
				if ok, why := gate.admit(ev, time.Now()); !ok {
					tr.AddProposalLog(fmt.Sprintf("[ACTIVITY] %s skipped — %s", ev.Ticker, why))
					continue
				}
				admitted.Add(1)
				tr.AddProposalLog(fmt.Sprintf("[ACTIVITY] %s %s %.1f → evaluating (seen %d, admitted %d, dropped %d)",
					ev.Ticker, ev.Kind, ev.Magnitude, seen.Load(), admitted.Load(), dropped.Load()))
				q := queuedActivity{
					ev: GekkoIngestEvent{SourceID: "MARKET_ACTIVITY", RawContent: activityContent(title, ev),
						SourceURL: "marketdata://" + activityChannel, ExtractedTimestampNS: ev.TsMs * 1_000_000, PayloadHash: activityHash(ev)},
					hint: &matchHint{ticker: ev.Ticker, title: title, bid: ev.Bid, ask: ev.Ask, confidence: 1.0},
				}
				select {
				case queue <- q:
				default:
					dropped.Add(1) // the model is saturated: drop rather than build a backlog of stale moves
				}
			}
			ps.Close()
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
		}
	}()
}
