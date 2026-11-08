package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// signalRedis is a read-only client for fetching signals (momentum) marketdata
// publishes. Set once at startup; nil-safe (signals degrade to the match score).
var signalRedis *redis.Client

func initSignalRedis(addr string) {
	if addr != "" {
		signalRedis = redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 1 * time.Second})
	}
}

// SignalInputs are the enrichment signals folded into a proposal's confidence and
// size on top of the base news→market match score. Any field may be zero/absent;
// the composite degrades gracefully.
type SignalInputs struct {
	MatchScore    float64 // base similarity [0,1] (0 ⇒ synthetic/no real match)
	TrendPct      float64 // momentum: short-horizon price trend %, signed
	VolSurge      float64 // momentum: top-of-book size ÷ window average (1 = normal)
	CrossEdge     float64 // cross-venue: otherVenueYes − thisVenueYes (price units; + ⇒ this venue cheaper)
	Sentiment     float64 // headline polarity [-1,1]
	Imbalance     float64 // order-book depth imbalance [-1,1] (+ bid-heavy/bullish, − ask-heavy/bearish)
	ImbalanceConf float64 // imbalance reliability [0,1] (thin/wide books discounted)
	Credibility   float64 // source credibility [0,1]; 0 = unknown (no effect), 0.5 neutral
}

// imbalanceDeadband suppresses the imbalance term until the book leans past ±0.15,
// so a near-balanced book (noise on thin Kalshi depth) doesn't nudge confidence
// (CloddsBot's ±0.15 signal threshold).
const imbalanceDeadband = 0.15

// compositeConfidence blends the base match score with the live signals into a
// final confidence [0.05,1.0] and a size multiplier [0.6,1.5], plus human notes.
// dir is +1 for BUY (betting the outcome up) and -1 for SELL/exit; signals that
// align with the trade direction raise conviction, opposing ones lower it. Pure.
func compositeConfidence(in SignalInputs, action string) (confidence, sizeMult float64, notes []string) {
	base := in.MatchScore
	if base <= 0 {
		base = 0.5 // synthetic ticker: neutral base so signals can still move it
	}
	dir := 1.0
	if strings.EqualFold(action, "SELL") {
		dir = -1.0
	}

	adj := 0.0
	if in.TrendPct != 0 {
		t := clamp(in.TrendPct/10.0, -1, 1) * 0.20 * dir // ±0.20 at ≥10% move
		adj += t
		notes = append(notes, fmt.Sprintf("momentum %+.1f%%", in.TrendPct))
	}
	if in.VolSurge >= 1.5 {
		adj += 0.05
		notes = append(notes, fmt.Sprintf("vol surge ×%.1f", in.VolSurge))
	}
	if in.CrossEdge != 0 {
		e := clamp(in.CrossEdge, -0.4, 0.4) * 0.5 * dir // up to ±0.20 for a full-price gap
		adj += e
		notes = append(notes, fmt.Sprintf("cross-venue edge %+.2f", in.CrossEdge))
	}
	if in.Sentiment != 0 {
		adj += clamp(in.Sentiment, -1, 1) * 0.10 * dir
		notes = append(notes, fmt.Sprintf("sentiment %+.2f", in.Sentiment))
	}
	// Order-book depth imbalance (clodds-mining ADOPT #5): a bid-heavy book supports
	// BUYs, an ask-heavy book supports SELL/bearish. Clamped small (±0.10, like
	// sentiment) and reliability-discounted so a thin book only nudges. Contrarian-
	// vulnerable (one big maker can skew depth), so it never drives on its own.
	if in.ImbalanceConf > 0 && math.Abs(in.Imbalance) >= imbalanceDeadband {
		adj += clamp(in.Imbalance, -1, 1) * 0.10 * dir * clamp(in.ImbalanceConf, 0, 1)
		notes = append(notes, fmt.Sprintf("book imbalance %+.2f", in.Imbalance))
	}

	confidence = clamp(base+adj, 0.05, 1.0)
	sizeMult = clamp(1.0+adj*1.5, 0.6, 1.5)
	// Source credibility is direction-independent: a shaky source shrinks size (and
	// confidence, more gently) for BUY and SELL alike; a solid one lifts it slightly.
	if in.Credibility > 0 {
		f := clamp(1.0+(in.Credibility-0.5), 0.5, 1.15)
		sizeMult *= f
		confidence = clamp(confidence*clamp(f, 0.85, 1.05), 0.05, 1.0)
		notes = append(notes, fmt.Sprintf("source credibility %.2f", in.Credibility))
	}
	return confidence, sizeMult, notes
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

var monthAbbr = map[string]time.Month{
	"JAN": time.January, "FEB": time.February, "MAR": time.March, "APR": time.April,
	"MAY": time.May, "JUN": time.June, "JUL": time.July, "AUG": time.August,
	"SEP": time.September, "OCT": time.October, "NOV": time.November, "DEC": time.December,
}

// tickerExpiry parses the resolution date encoded in a Kalshi ticker's event segment
// (the 2nd dash-part, YYMMMDD, optionally followed by an intraday time): e.g.
// KXINX-26JUN26 → 2026-06-26, KXINXY-26DEC31H1600 → 2026-12-31 16:00,
// KXETH15M-26JUL090845 → 2026-07-09 08:45. Returns (expiry, true) only on a clean
// parse; tickers without a parseable date (POLY-<token>, JPMCEOCHANGE-27, synthetics)
// return (_, false) so the caller skips the near-expiry guard. With no intraday time
// the expiry is end of day (23:59), so "expires today" reads as hours away rather than
// already past. UTC — coarse enough for a same-day variance guard.
//
// The intraday suffix (everything after YYMMMDD) comes in three flavors, all handled:
//   - "H"+HHMM ("H1600") — hourly index/equity series
//   - bare HHMM ("0845")  — sub-hourly crypto series (KXETH15M, KXBTC…): WITHOUT this,
//     a 15-minute market defaulted to 23:59 and slipped past the 1h near-expiry skip,
//     which is how the run's single worst trade (a 2-minute KXETH15M gamble) got in.
//   - bare HH ("17")      — hourly crypto series (KXBTCD-26JUL0917 → 17:00)
func tickerExpiry(contractID string) (time.Time, bool) {
	parts := strings.Split(contractID, "-")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	seg := strings.ToUpper(parts[1])
	if len(seg) < 7 {
		return time.Time{}, false
	}
	yy, err1 := strconv.Atoi(seg[0:2])
	mon, ok := monthAbbr[seg[2:5]]
	dd, err2 := strconv.Atoi(seg[5:7])
	if err1 != nil || !ok || err2 != nil || dd < 1 || dd > 31 {
		return time.Time{}, false
	}
	hour, min := 23, 59 // default: end of day so a dateless intraday reads as hours away
	switch rest := seg[7:]; {
	case len(rest) >= 5 && rest[0] == 'H':
		hour, min = parseHHMM(rest[1:5], hour, min)
	case len(rest) == 4:
		hour, min = parseHHMM(rest, hour, min)
	case len(rest) == 2:
		if h, e := strconv.Atoi(rest); e == nil && h < 24 {
			hour, min = h, 0
		}
	}
	// Kalshi encodes resolution times in US Eastern, NOT UTC. Parsing them as UTC
	// put every intraday expiry 4–5h off — enough that a 15-minute market read as
	// hours away and slipped past the near-expiry skip. Build the instant in ET.
	return time.Date(2000+yy, mon, dd, hour, min, 0, 0, kalshiTZ), true
}

// kalshiTZ is the exchange clock (US Eastern, with DST). Resolved once; falls back
// to a fixed EST offset if the tzdata database isn't available at runtime.
var kalshiTZ = func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.FixedZone("EST", -5*3600)
}()

// parseHHMM parses a 4-char "HHMM" string, returning (defHour, defMin) unchanged
// if it isn't a valid 24h clock time. Pure.
func parseHHMM(s string, defHour, defMin int) (int, int) {
	h, e1 := strconv.Atoi(s[0:2])
	m, e2 := strconv.Atoi(s[2:4])
	if e1 != nil || e2 != nil || h >= 24 || m >= 60 {
		return defHour, defMin
	}
	return h, m
}

// entrySkewSizeMult scales a BUY's size by its payoff skew (post-mortem #3). A YES
// contract bought at price p risks p per contract to win (1−p), i.e. a reward:risk of
// (1−p)/p. Entries at or below 0.50 (rr ≥ 1) keep full size; richer entries — where the
// downside dominates and, empirically, the bot showed no edge (run #2 lost ≈$8k on
// entries ≥0.50 at a 6% win rate) — are scaled down toward floor. Prices outside (0,1)
// or SELL/exit actions are left unscaled (return 1.0). Pure.
func entrySkewSizeMult(price, floor float64) float64 {
	if price <= 0 || price >= 1 {
		return 1.0
	}
	rr := (1 - price) / price
	return clamp(rr, floor, 1.0)
}

// edgeVsMarket computes the strategy's edge against the live book for an action:
// BUY edge = fair − ask (positive ⇒ the market underprices YES by our estimate);
// SELL edge = bid − fair. gated=false when the relevant book side is missing
// (nothing to compare against — the caller keeps its own limit price and the
// executive's entry book-quality gate handles the no-book case). Pure.
func edgeVsMarket(action string, fair, bid, ask float64) (edge float64, gated bool) {
	if strings.EqualFold(action, "SELL") {
		if bid <= 0 {
			return 0, false
		}
		return bid - fair, true
	}
	if ask <= 0 {
		return 0, false
	}
	return fair - ask, true
}

// fairImplausible reports whether a BUY's fair-value estimate sits implausibly far
// above the ask — fair > maxRatio × ask. A model claiming a 2¢ market is really
// worth 15¢ is hallucinating, not finding edge (2026-07-07 forensics: every loss
// in the 24h window was a sub-$0.25 longshot bought on exactly this pattern).
// Only bites cheap contracts by construction: for ask ≥ 1/maxRatio the cap
// exceeds 1.0 and any valid probability passes. SELLs, missing asks, and
// maxRatio ≤ 0 are never implausible. Pure.
func fairImplausible(action string, fair, ask, maxRatio float64) bool {
	if maxRatio <= 0 || ask <= 0 || strings.EqualFold(action, "SELL") {
		return false
	}
	return fair > maxRatio*ask
}

// requiredEdge is the minimum fair-vs-book edge for a matched trade: the base
// MIN_EDGE plus the top-of-book spread when both sides exist. An edge smaller
// than the spread doesn't survive the round trip — entering at the ask and
// exiting at the bid costs the full spread before any move. Pure.
func requiredEdge(minEdge, bid, ask float64) float64 {
	if bid > 0 && ask > bid {
		return minEdge + (ask - bid)
	}
	return minEdge
}

// readMomentum fetches the momentum signal marketdata publishes for a contract,
// including the order-book depth imbalance and its reliability discount.
func readMomentum(ctx context.Context, rdb *redis.Client, contractID string) (trendPct, volSurge, imbalance, imbalanceConf float64, ok bool) {
	if rdb == nil {
		return 0, 0, 0, 0, false
	}
	val, err := rdb.Get(ctx, "momentum:"+contractID).Result()
	if err != nil {
		return 0, 0, 0, 0, false
	}
	var m struct {
		TrendPct      float64 `json:"trend_pct"`
		VolSurge      float64 `json:"vol_surge"`
		Samples       int     `json:"samples"`
		Imbalance     float64 `json:"imbalance"`
		ImbalanceConf float64 `json:"imbalance_conf"`
	}
	if json.Unmarshal([]byte(val), &m) != nil || m.Samples < 2 {
		return 0, 0, 0, 0, false
	}
	return m.TrendPct, m.VolSurge, m.Imbalance, m.ImbalanceConf, true
}

// positiveWords / negativeWords drive the lightweight deterministic headline
// sentiment scorer (no external dependency; a stand-in for a real NLP feed).
var positiveWords = map[string]bool{
	"surge": true, "soar": true, "rally": true, "gain": true, "gains": true, "jump": true,
	"jumps": true, "rise": true, "rises": true, "climb": true, "climbs": true, "beat": true,
	"beats": true, "record": true, "high": true, "bullish": true, "up": true, "win": true,
	"wins": true, "strong": true, "growth": true, "boost": true, "optimism": true, "approve": true,
}
var negativeWords = map[string]bool{
	"plunge": true, "plunges": true, "crash": true, "fall": true, "falls": true, "drop": true,
	"drops": true, "slump": true, "tumble": true, "tumbles": true, "miss": true, "misses": true,
	"low": true, "bearish": true, "down": true, "loss": true, "losses": true, "weak": true,
	"decline": true, "fear": true, "fears": true, "cut": true, "cuts": true, "reject": true, "slowdown": true,
}

// headlineSentiment scores text polarity in [-1,1] from a small lexicon.
func headlineSentiment(text string) float64 {
	var pos, neg int
	for _, raw := range strings.Fields(strings.ToLower(text)) {
		w := strings.Trim(raw, ".,!?\"'();:")
		if positiveWords[w] {
			pos++
		} else if negativeWords[w] {
			neg++
		}
	}
	total := pos + neg
	if total == 0 {
		return 0
	}
	return float64(pos-neg) / float64(total)
}
