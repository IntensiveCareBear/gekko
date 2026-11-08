package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// catalogMarket mirrors the entries marketdata publishes to gekko:kalshi_markets
// and gekko:polymarket_markets.
type catalogMarket struct {
	Ticker string  `json:"ticker"`
	Title  string  `json:"title"`
	YesBid float64 `json:"yes_bid"`
	YesAsk float64 `json:"yes_ask"`
	Volume float64 `json:"volume"`
	Status string  `json:"status"`
	Event  string  `json:"event"`
	// CloseTime is RFC3339, empty when the venue did not report one.
	CloseTime string `json:"close_time"`
}

// MarketMatcher embeds discovered Kalshi market titles into a dedicated Qdrant
// collection and matches incoming news events to the nearest real market. When no
// catalog is available (keyless sim), Match returns ok=false and Gordon falls back
// to its synthetic ticker behavior.
type MarketMatcher struct {
	oc          *ProposalClient
	qc          *QdrantClient // kalshi_markets collection
	rdb         *redis.Client
	threshold   float64        // default/fallback when no thresholdFn is set
	thresholdFn func() float64 // live selectivity knob (overrides threshold)

	mu       sync.Mutex
	embedded map[string]bool // ticker -> embedded into Qdrant
	asks     map[string]float64
	bids     map[string]float64
	titles   map[string]string    // ticker -> market title (for LLM market context)
	closes   map[string]time.Time // ticker -> trading close, when the catalog has one
	count    int

	rerouted atomic.Int64 // matches moved off the nearest market onto a tradeable one
}

func NewMarketMatcher(oc *ProposalClient, qc *QdrantClient, redisAddr string, threshold float64) *MarketMatcher {
	var rdb *redis.Client
	if redisAddr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second})
	}
	return &MarketMatcher{
		oc: oc, qc: qc, rdb: rdb, threshold: threshold,
		embedded: make(map[string]bool),
		asks:     make(map[string]float64),
		bids:     make(map[string]float64),
		titles:   make(map[string]string),
		closes:   make(map[string]time.Time),
	}
}

// Active reports whether any real markets have been embedded (i.e. live mode on).
func (m *MarketMatcher) Active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count > 0
}

// runSync periodically embeds newly discovered market titles into Qdrant.
// It polls quickly until the catalog first appears (avoiding a cold-start race
// where Gordon runs before marketdata has published), then settles into interval.
func (m *MarketMatcher) runSync(ctx context.Context, interval time.Duration) {
	if m.rdb == nil || m.qc == nil {
		return
	}
	// Recreate fresh so stale embeddings from a prior run's universe don't linger.
	if err := m.qc.RecreateCollection(ctx); err != nil {
		log.Printf("[WARN] kalshi_markets collection recreate failed: %v", err)
	}
	const warmup = 10 * time.Second
	for {
		m.syncOnce(ctx)
		delay := interval
		if !m.Active() {
			delay = warmup // catalog not available yet — retry soon
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (m *MarketMatcher) syncOnce(ctx context.Context) {
	// Merge both venue catalogs marketdata publishes. Either may be absent
	// depending on which sources are enabled; the POLY- prefix on Polymarket
	// tickers carries the venue, so a single Qdrant collection is fine.
	var markets []catalogMarket
	fetched := map[string]bool{}
	for _, key := range []string{"gekko:kalshi_markets", "gekko:polymarket_markets"} {
		val, err := m.rdb.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		var batch []catalogMarket
		if json.Unmarshal([]byte(val), &batch) == nil {
			markets = append(markets, batch...)
			fetched[key] = true
		}
	}
	if len(markets) == 0 {
		return // no catalog yet (marketdata not running)
	}

	// Only tradeable markets belong in the match universe: a resolved/closed
	// market that stays matchable burns an LLM slot on a proposal that's
	// guaranteed to die at the order-book check.
	current := make(map[string]bool, len(markets))
	for _, mk := range markets {
		if mk.Ticker == "" || mk.Title == "" || !tradeableStatus(mk.Status) {
			continue
		}
		current[mk.Ticker] = true
	}

	// Prune markets that left their venue's catalog since we embedded them
	// (24h forensics: KXCPICOMBO/KXDOTPLOTPUB resolved mid-run but kept
	// matching news for a day). Skip a venue whose catalog wasn't fetched
	// this cycle — absence of the catalog is not absence of the market.
	stale := staleTickers(m.embeddedSnapshot(), current, fetched["gekko:kalshi_markets"], fetched["gekko:polymarket_markets"])
	if len(stale) > 0 {
		m.mu.Lock()
		for _, tk := range stale {
			delete(m.embedded, tk)
			delete(m.asks, tk)
			delete(m.bids, tk)
			delete(m.titles, tk)
			delete(m.closes, tk)
			m.count--
		}
		m.mu.Unlock()
		for _, tk := range stale {
			dc, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := m.qc.DeletePointsByContractID(dc, tk); err != nil {
				log.Printf("[WARN] failed to prune stale market %s from Qdrant: %v (Match guards against it)", tk, err)
			}
			cancel()
		}
		log.Printf("[INFO] Pruned %d market(s) that left the catalog (%d still matchable).", len(stale), m.countSafe())
	}

	const maxPerCycle = 300 // embed the whole catalog on the first successful sync
	embeds := 0
	for _, mk := range markets {
		if mk.Ticker == "" || mk.Title == "" || !current[mk.Ticker] {
			continue
		}
		m.mu.Lock()
		m.asks[mk.Ticker] = mk.YesAsk
		m.bids[mk.Ticker] = mk.YesBid
		m.titles[mk.Ticker] = mk.Title
		if ct, err := time.Parse(time.RFC3339, mk.CloseTime); err == nil {
			m.closes[mk.Ticker] = ct
		}
		already := m.embedded[mk.Ticker]
		m.mu.Unlock()
		if already || embeds >= maxPerCycle {
			continue
		}

		ec, cancel := context.WithTimeout(ctx, 5*time.Second)
		vec, err := m.oc.GetEmbedding(ec, mk.Title)
		cancel()
		if err != nil {
			continue
		}
		uc, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = m.qc.UpsertEvent(uc, mk.Title, vec, mk.Ticker, "", mk.YesAsk, 0, "", "")
		cancel()
		if err != nil {
			continue
		}
		m.mu.Lock()
		m.embedded[mk.Ticker] = true
		m.count++
		m.mu.Unlock()
		embeds++
	}
	if embeds > 0 {
		log.Printf("[INFO] Embedded %d new Kalshi market(s) for matching (%d total).", embeds, m.countSafe())
	}
}

// tradeableStatus reports whether a catalog status string denotes a live,
// tradeable market. Empty passes for back-compat with catalogs that omit it.
func tradeableStatus(status string) bool {
	switch strings.ToLower(status) {
	case "", "active", "open":
		return true
	}
	return false
}

// staleTickers returns the embedded tickers that no longer appear in the current
// catalog, restricted to venues whose catalog was actually fetched this cycle
// (kalshiOK / polyOK). Pure — unit-tested.
func staleTickers(embedded map[string]bool, current map[string]bool, kalshiOK, polyOK bool) []string {
	var stale []string
	for tk := range embedded {
		if current[tk] {
			continue
		}
		if strings.HasPrefix(tk, "POLY-") {
			if polyOK {
				stale = append(stale, tk)
			}
		} else if kalshiOK {
			stale = append(stale, tk)
		}
	}
	return stale
}

// embeddedSnapshot copies the embedded-ticker set for lock-free inspection.
func (m *MarketMatcher) embeddedSnapshot() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := make(map[string]bool, len(m.embedded))
	for tk := range m.embedded {
		snap[tk] = true
	}
	return snap
}

// crossVenueEdge finds the nearest market on the OPPOSITE venue to primaryTicker
// for the same news embedding and returns its YES price and the edge
// (otherYes − primaryYes; positive ⇒ the primary venue is cheaper, a buy edge).
// Used for the cross-venue divergence signal (Kalshi ↔ Polymarket on one event).
func (m *MarketMatcher) crossVenueEdge(ctx context.Context, embedding []float32, primaryTicker string, primaryYes float64) (otherYes, edge float64, ok bool) {
	if !m.Active() {
		return 0, 0, false
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	matches, err := m.qc.QueryMatches(qctx, embedding, 5)
	if err != nil || len(matches) == 0 {
		return 0, 0, false
	}
	primaryIsPoly := strings.HasPrefix(primaryTicker, "POLY-")
	thr := m.activeThreshold()
	for _, mt := range matches {
		t := mt.Payload.ContractID
		if t == "" || t == primaryTicker || float64(mt.Score) < thr {
			continue
		}
		if strings.HasPrefix(t, "POLY-") == primaryIsPoly {
			continue // need the other venue
		}
		m.mu.Lock()
		oy := m.asks[t]
		m.mu.Unlock()
		if oy <= 0 {
			continue
		}
		return oy, oy - primaryYes, true
	}
	return 0, 0, false
}

// activeThreshold returns the live selectivity threshold when wired, else the
// static default passed at construction.
func (m *MarketMatcher) activeThreshold() float64 {
	if m.thresholdFn != nil {
		return m.thresholdFn()
	}
	return m.threshold
}

func (m *MarketMatcher) countSafe() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

// --- Entity-verification gate on news→market matches (clodds-mining ADOPT #1) ---
//
// Penalties mirror CloddsBot's verifyMatch weights (year > date > threshold). A year
// or month mismatch is a strong wrong-market signal; the numeric-threshold check is
// weaker because news text is number-heavy, so it is applied directionally (title's
// strike must appear in the news) and never on the reverse.
const (
	yearMismatchPenalty      = 0.5
	monthMismatchPenalty     = 0.3
	thresholdMismatchPenalty = 0.15 // news writes strikes loosely ($83K vs 83000); was 0.4, left good BTC matches at conf 0.13
)

var (
	// Four-digit calendar years (avoids matching 3- or 5-digit strikes like 5000/12000).
	yearRe = regexp.MustCompile(`\b(19|20|21)\d{2}\b`)
	// Month names, full or abbreviated (normalized to the 3-letter monthAbbr keys).
	monthNameRe = regexp.MustCompile(`(?i)\b(jan(uary)?|feb(ruary)?|mar(ch)?|apr(il)?|may|jun(e)?|jul(y)?|aug(ust)?|sept?(ember)?|oct(ober)?|nov(ember)?|dec(ember)?)\b`)
	// Numeric magnitudes: optional $, digit groups with commas, optional decimal,
	// optional k/m/b or % suffix. e.g. $100k, 100,000, 5%, 3.5.
	thresholdRe = regexp.MustCompile(`(?i)\$?\d[\d,]*(?:\.\d+)?\s?[kmb%]?`)
)

// verifyMatch checks that a semantically matched market's title agrees with the news
// text on critical entities. It returns a confidence penalty (≥0) and human-readable
// warnings. Fail-open: an entity type present on only one side (or neither) contributes
// nothing, so a terse title with no extractable entities yields (0, nil). Pure.
func verifyMatch(newsText, marketTitle string) (penalty float64, warnings []string) {
	// Years: symmetric — if both texts name years and share none, the match likely
	// spans different resolution years.
	ny, ty := extractYears(newsText), extractYears(marketTitle)
	if len(ny) > 0 && len(ty) > 0 && disjointInt(ny, ty) {
		penalty += yearMismatchPenalty
		warnings = append(warnings, fmt.Sprintf("year %v not in news %v", sortedInts(ty), sortedInts(ny)))
	}
	// Months: symmetric — a "by January" ↔ "by February" mismatch.
	nm, tm := extractMonths(newsText), extractMonths(marketTitle)
	if len(nm) > 0 && len(tm) > 0 && disjointMonth(nm, tm) {
		penalty += monthMismatchPenalty
		warnings = append(warnings, fmt.Sprintf("month %v not in news %v", sortedMonths(tm), sortedMonths(nm)))
	}
	// Thresholds: directional — the title's numeric strike is the critical entity; if
	// the title carries thresholds and none appear in the news, the match likely paired
	// different strikes. News-side numbers are noisy, so the reverse is not penalized.
	tt := extractThresholds(marketTitle)
	if len(tt) > 0 {
		nt := extractThresholds(newsText)
		if len(nt) > 0 && !anyThresholdShared(tt, nt) {
			penalty += thresholdMismatchPenalty
			warnings = append(warnings, fmt.Sprintf("threshold %v not in news %v", sortedFloats(tt), sortedFloats(nt)))
		}
	}
	return penalty, warnings
}

func extractYears(text string) map[int]bool {
	out := map[int]bool{}
	for _, m := range yearRe.FindAllString(text, -1) {
		if y, err := strconv.Atoi(m); err == nil {
			out[y] = true
		}
	}
	return out
}

func extractMonths(text string) map[time.Month]bool {
	out := map[time.Month]bool{}
	for _, m := range monthNameRe.FindAllString(text, -1) {
		key := strings.ToUpper(m)
		if len(key) >= 3 {
			key = key[:3]
		}
		if mo, ok := monthAbbr[key]; ok {
			out[mo] = true
		}
	}
	return out
}

// extractThresholds parses numeric magnitudes into a normalized set (commas stripped,
// k/m/b suffixes applied). Four-digit year-range values without a suffix or decimal are
// excluded — they are years, handled separately, not strikes.
func extractThresholds(text string) map[float64]bool {
	out := map[float64]bool{}
	for _, tok := range thresholdRe.FindAllString(text, -1) {
		raw := strings.ToLower(strings.TrimSpace(tok))
		hadPct := strings.HasSuffix(raw, "%")
		mult := 1.0
		switch {
		case strings.HasSuffix(raw, "k"):
			mult = 1e3
		case strings.HasSuffix(raw, "m"):
			mult = 1e6
		case strings.HasSuffix(raw, "b"):
			mult = 1e9
		}
		digits := strings.NewReplacer("$", "", ",", "", "k", "", "m", "", "b", "", "%", "", " ", "").Replace(raw)
		v, err := strconv.ParseFloat(digits, 64)
		if err != nil {
			continue
		}
		v *= mult
		// Skip bare 4-digit years so a strike check isn't fooled by a date.
		if mult == 1.0 && !hadPct && !strings.Contains(digits, ".") && v >= 1900 && v <= 2099 {
			continue
		}
		out[v] = true
	}
	return out
}

// anyThresholdShared reports whether any title threshold matches a news threshold
// within 1% relative tolerance (so $100k ≈ 100,000 and rounding differences pass).
func anyThresholdShared(title, news map[float64]bool) bool {
	for t := range title {
		for n := range news {
			tol := 0.01 * math.Max(math.Abs(t), math.Abs(n))
			if math.Abs(t-n) <= tol {
				return true
			}
		}
	}
	return false
}

func disjointInt(a, b map[int]bool) bool {
	for k := range a {
		if b[k] {
			return false
		}
	}
	return true
}

func disjointMonth(a, b map[time.Month]bool) bool {
	for k := range a {
		if b[k] {
			return false
		}
	}
	return true
}

func sortedInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func sortedMonths(m map[time.Month]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.String()[:3])
	}
	sort.Strings(out)
	return out
}

func sortedFloats(m map[float64]bool) []float64 {
	out := make([]float64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Float64s(out)
	return out
}

// Match returns the nearest real market ticker for a news embedding, plus its
// title, yes bid/ask and the similarity score, when it clears the threshold.
// Candidates no longer in the live catalog (e.g. a stale Qdrant point whose
// delete failed) are skipped in favor of the next-nearest live market.
func (m *MarketMatcher) Match(ctx context.Context, embedding []float32) (ticker, title string, yesBid, yesAsk, score float64, ok bool) {
	if !m.Active() {
		return "", "", 0, 0, 0, false
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	matches, err := m.qc.QueryMatches(qctx, embedding, matchCandidates)
	if err != nil || len(matches) == 0 {
		return "", "", 0, 0, 0, false
	}
	thr := m.activeThreshold()
	first := true
	for _, mt := range matches {
		if mt.Score < thr {
			break // results are sorted by score; nothing further can clear it
		}
		t := mt.Payload.ContractID
		if t == "" || strings.HasPrefix(t, weatherSeriesPrefix) {
			continue // weather buckets belong to the deterministic weather gordon
		}
		m.mu.Lock()
		tt, known := m.titles[t]
		ya, yb := m.asks[t], m.bids[t]
		m.mu.Unlock()
		if !known {
			continue // stale embedding — market left the catalog
		}
		// Prefer the live book over the catalog quote: the catalog is minutes old and
		// Polymarket's carries no spread at all (bid == ask == outcome price).
		if lb, la, live := m.liveQuote(qctx, t); live {
			yb, ya = lb, la
		}
		if first {
			// Fallback when nothing above threshold is tradeable: the nearest market,
			// which processEvent's pre-gate then skips without spending an LLM slot.
			ticker, title, yesBid, yesAsk, score, ok = t, tt, yb, ya, float64(mt.Score), true
			first = false
		}
		held, _ := riskStateReader.HeldContract(ctx, t)
		tradeable, _ := quoteTradeable(yb, ya)
		if exp, ok := contractExpiry(t); ok && time.Until(exp) <= nearExpirySkip {
			tradeable = false // resolves too soon to enter; keep looking
		}
		if held || tradeable {
			if t != ticker {
				m.rerouted.Add(1)
			}
			return t, tt, yb, ya, float64(mt.Score), true
		}
	}
	return ticker, title, yesBid, yesAsk, score, ok
}

// matchCandidates is how many nearest markets Match considers before giving up on
// finding a tradeable one (strike ladders put many dead siblings next to a live one).
const matchCandidates = 20

const weatherSeriesPrefix = "KXHIGH"

// Same knobs (and defaults) as breen's entry spread gate, so Gordon stops proposing
// what breen refuses at fill.
var (
	entryMaxSpreadPct = getEnvFloat("ENTRY_MAX_SPREAD_PCT", 0.10)
	entryMaxSpreadAbs = getEnvFloat("ENTRY_MAX_SPREAD_ABS", 0.02)
)

// quoteTradeable reports whether a new entry at this top-of-book could pass the
// downstream gates: two-sided, spread within breen's cap, ask inside the entry
// price band. Pure — unit-tested.
func quoteTradeable(bid, ask float64) (ok bool, reason string) {
	if bid <= 0 || ask <= 0 || ask <= bid {
		return false, fmt.Sprintf("one-sided book (bid %.2f / ask %.2f)", bid, ask)
	}
	spread, mid := ask-bid, (ask+bid)/2
	// 1e-9: books carry float noise (0.29000000000000004), don't fail a 2c spread on it.
	if spread > entryMaxSpreadAbs+1e-9 && entryMaxSpreadPct > 0 && spread/mid > entryMaxSpreadPct+1e-9 {
		return false, fmt.Sprintf("spread %.2f is %.0f%% of mid (cap %.0f%%)", spread, 100*spread/mid, 100*entryMaxSpreadPct)
	}
	if (minEntryPrice > 0 && ask < minEntryPrice-1e-9) || (maxEntryPrice > 0 && ask > maxEntryPrice+1e-9) {
		return false, fmt.Sprintf("ask %.2f outside entry band %.2f–%.2f", ask, minEntryPrice, maxEntryPrice)
	}
	return true, ""
}

// bestQuote returns the top of a book without trusting level order or empty levels.
func bestQuote(ob *OrderBook) (bid, ask float64) {
	for _, l := range ob.Bids {
		if l.Volume > 0 && l.Price > bid {
			bid = l.Price
		}
	}
	for _, l := range ob.Asks {
		if l.Volume > 0 && (ask == 0 || l.Price < ask) {
			ask = l.Price
		}
	}
	return bid, ask
}

// liveQuote reads the top of marketdata's orderbook:<ticker>; ok=false when no book
// is cached (the caller keeps the catalog quote).
func (m *MarketMatcher) liveQuote(ctx context.Context, ticker string) (bid, ask float64, ok bool) {
	if m.rdb == nil {
		return 0, 0, false
	}
	val, err := m.rdb.Get(ctx, "orderbook:"+ticker).Result()
	if err != nil {
		return 0, 0, false
	}
	var ob OrderBook
	if json.Unmarshal([]byte(val), &ob) != nil {
		return 0, 0, false
	}
	bid, ask = bestQuote(&ob)
	return bid, ask, true
}

// contractExpiry is when a contract stops trading: the catalog's close time when the
// venue reported one, else the date parsed from the ticker. Monthly econ tickers
// (KXCPI-26SEP) and every POLY- ticker have no parseable date, so before the catalog
// carried close times the expiry gates silently passed them.
func contractExpiry(ticker string) (time.Time, bool) {
	if m := marketMatcher; m != nil {
		m.mu.Lock()
		ct, ok := m.closes[ticker]
		m.mu.Unlock()
		if ok {
			return ct, true
		}
	}
	return tickerExpiry(ticker)
}
