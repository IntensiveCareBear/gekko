// Command marketdata is a READ-ONLY Kalshi market-data poller. It pulls live
// order books + market status with a user-provided read-only API key and writes
// them to Redis (orderbook:<ticker>, market:<ticker>) where gordon and
// breen already read them. It NEVER places, modifies, or cancels orders.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

func main() {
	log.Println("[INFO] Starting Gekko marketdata service (READ-ONLY market data)...")

	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	// Book keys must outlive a full universe rotation (universe ÷ BOOKS_PER_CYCLE ×
	// poll interval, ≈40–70s at the defaults) or they expire before their next
	// refresh — the 2026-07-05 post-mortem found books missing two-thirds of the
	// time at the old 30s TTL because a full sweep took ~100s. Consumers judge
	// actual freshness from the as_of_ms stamp, not key existence.
	obTTL := getEnvDuration("ORDERBOOK_TTL", 3*time.Minute)
	healthAddr := getEnv("HEALTH_ADDR", ":8093")

	// Kalshi requires a read-only API key + RSA key; Polymarket needs no creds.
	apiKey := os.Getenv("KALSHI_API_KEY")
	keyFile := os.Getenv("KALSHI_PRIVATE_KEY_FILE")
	keyInline := os.Getenv("KALSHI_PRIVATE_KEY_PEM")
	kalshiEnabled := apiKey != "" && (keyFile != "" || keyInline != "")
	polyEnabled := strings.ToLower(getEnv("POLYMARKET_ENABLED", "true")) != "false"

	if !kalshiEnabled && !polyEnabled {
		log.Fatalf("[FATAL] marketdata has no enabled source. Set KALSHI_API_KEY+KALSHI_PRIVATE_KEY_FILE and/or POLYMARKET_ENABLED=true.")
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: 2 * time.Second})

	startHealthServer(healthAddr)

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; log.Println("[INFO] marketdata shutting down..."); cancel() }()

	if kalshiEnabled {
		go runKalshi(ctx, rdb, apiKey, keyFile, keyInline, obTTL)
	} else {
		log.Println("[INFO] Kalshi source disabled (no KALSHI_API_KEY/key file).")
	}
	if polyEnabled {
		go runPolymarket(ctx, rdb, obTTL)
	} else {
		log.Println("[INFO] Polymarket source disabled (POLYMARKET_ENABLED=false).")
	}

	<-ctx.Done()
}

// runKalshi wires up Kalshi discovery + order-book polling (READ-ONLY).
func runKalshi(ctx context.Context, rdb *redis.Client, apiKey, keyFile, keyInline string, obTTL time.Duration) {
	base := getEnv("KALSHI_API_BASE", "https://api.elections.kalshi.com/trade-api/v2")
	watch := splitCSV(getEnv("KALSHI_WATCH_TICKERS", ""))
	pollInterval := getEnvDuration("KALSHI_POLL_INTERVAL", 5*time.Second)
	rate := getEnvInt("KALSHI_RATE_LIMIT", 8)
	booksPerCycle := getEnvInt("KALSHI_BOOKS_PER_CYCLE", 30)
	discoveryInterval := getEnvDuration("KALSHI_DISCOVERY_INTERVAL", 5*time.Minute)
	maxMarkets := getEnvInt("KALSHI_MAX_MARKETS", 800)
	maxSeries := getEnvInt("KALSHI_MAX_SERIES", 200)
	// Marquee series fetched first (confirmed valid). Override via KALSHI_SERIES.
	defaultPriority := "KXBTCD,KXETHD,KXNASDAQ100,KXINXD,KXCPI,KXCPICORE,KXGDP,KXGDPNOM,KXPAYROLLS,KXU3,KXTNOTED,KXUSTYLD,KXFM30YMTG,KXBRENTW,KXSOLE"
	priority := splitCSV(getEnv("KALSHI_SERIES", defaultPriority))
	categories := splitCSV(getEnv("KALSHI_CATEGORIES", "Crypto,Economics,Financials,Commodities"))
	// Weather high-temp discovery is dark by default: the buckets are near-zero
	// volume and would otherwise pollute the news→market matcher. When enabled
	// (alongside the weather ingest source + strategy), pin the daily high-temp
	// series so they're always cataloged and add the Climate category for any others.
	if getEnvBool("KALSHI_WEATHER_ENABLED", false) {
		priority = append(priority, weatherHighTempSeries...)
		categories = append(categories, "Climate and Weather")
		log.Printf("[INFO] Weather high-temp discovery ENABLED: pinned series %v.", weatherHighTempSeries)
	}

	priv, err := loadRSAKey(keyFile, keyInline)
	if err != nil {
		log.Printf("[ERROR] Failed to load Kalshi RSA key, Kalshi source disabled: %v", err) // never prints the key
		return
	}
	log.Printf("[INFO] Kalshi read-only key loaded. Base=%s | watch=%v | poll=%v | rate=%d/s",
		base, watch, pollInterval, rate)

	client := NewKalshiClient(base, apiKey, priv, rate)

	// Market discovery: publishes gekko:kalshi_markets and drives the watch set.
	store := &discoveredStore{}
	dc := &discoverer{
		client: client, rdb: rdb, store: store,
		maxMarkets: maxMarkets, maxSeries: maxSeries,
		priority: priority, categories: categories,
		strikesPerEvent: getEnvInt("KALSHI_STRIKES_PER_EVENT", 10),
	}
	go dc.run(ctx, discoveryInterval)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	rot := &rotator{}
	cycle := func() {
		held := heldTickers(ctx, rdb, false)
		hot := hotTickers(ctx, rdb, false)
		priority, universe := splitPriority(append(append(held, hot...), watch...), store.get())
		pollCycle(ctx, client, rdb, priority, universe, rot, booksPerCycle, obTTL, "kalshi")
	}
	cycle()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle()
		}
	}
}

// runPolymarket wires up Polymarket discovery + order-book polling (READ-ONLY,
// no credentials — public Gamma + CLOB APIs).
func runPolymarket(ctx context.Context, rdb *redis.Client, obTTL time.Duration) {
	gamma := getEnv("POLYMARKET_GAMMA_URL", "https://gamma-api.polymarket.com")
	clob := getEnv("POLYMARKET_CLOB_URL", "https://clob.polymarket.com")
	pollInterval := getEnvDuration("POLYMARKET_POLL_INTERVAL", 5*time.Second)
	rate := getEnvInt("POLYMARKET_RATE_LIMIT", 8)
	booksPerCycle := getEnvInt("POLYMARKET_BOOKS_PER_CYCLE", 30)
	discoveryInterval := getEnvDuration("POLYMARKET_DISCOVERY_INTERVAL", 5*time.Minute)
	maxMarkets := getEnvInt("POLYMARKET_MAX_MARKETS", 150)

	log.Printf("[INFO] Polymarket read-only data enabled. Gamma=%s | CLOB=%s | poll=%v | rate=%d/s",
		gamma, clob, pollInterval, rate)

	client := NewPolymarketClient(gamma, clob, rate)
	store := &discoveredStore{}
	dc := &polyDiscoverer{client: client, rdb: rdb, store: store, maxMarkets: maxMarkets}
	go dc.run(ctx, discoveryInterval)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	rot := &rotator{}
	cycle := func() {
		held := heldTickers(ctx, rdb, true)
		hot := hotTickers(ctx, rdb, true)
		priority, universe := splitPriority(append(held, hot...), store.get())
		pollCycle(ctx, client, rdb, priority, universe, rot, booksPerCycle, obTTL, "polymarket")
	}
	cycle()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle()
		}
	}
}

// marketSource is the read-only per-venue client surface shared by the poll cycle.
type marketSource interface {
	FetchOrderBook(ticker string) (*OrderBook, error)
	FetchMarket(ticker string) (*MarketInfo, error)
}

// rotator walks the discovered universe one budget-sized batch per cycle, so
// priority tickers (held positions + watchlist) can be refreshed every cycle while
// the long tail still gets covered within one book TTL. The 2026-07-05 post-mortem
// found the old full sweep took ~100s against a 30s TTL, so books were expired
// two-thirds of the time and dry fills fell back to stale proposal prices.
type rotator struct {
	cursor int
	polled int       // tickers polled since the last completed pass
	wrapAt time.Time // start of the current pass
}

// next returns the following batch of up to budget tickers, wrapping at the end of
// the universe. wrapped reports a completed pass along with its duration.
func (r *rotator) next(universe []string, budget int) (batch []string, wrapped bool, wrapDur time.Duration) {
	n := len(universe)
	if n == 0 {
		return nil, false, 0
	}
	if r.wrapAt.IsZero() {
		r.wrapAt = time.Now()
	}
	if budget <= 0 || budget > n {
		budget = n
	}
	if r.cursor >= n {
		r.cursor = 0 // universe shrank since the last cycle
	}
	batch = make([]string, 0, budget)
	for i := 0; i < budget; i++ {
		batch = append(batch, universe[(r.cursor+i)%n])
	}
	r.cursor = (r.cursor + budget) % n
	r.polled += budget
	if r.polled >= n {
		wrapDur = time.Since(r.wrapAt)
		r.polled = 0
		r.wrapAt = time.Now()
		return batch, true, wrapDur
	}
	return batch, false, 0
}

// pollCycle runs one poll pass for a venue: every priority ticker (held + watch)
// gets its book AND market status refreshed, then up to budget universe tickers get
// their books refreshed via the rotator. Market status for non-held tickers is
// skipped — only settlement of HELD positions reads market:<ticker> — which halves
// the request cost of covering the universe.
func pollCycle(ctx context.Context, src marketSource, rdb *redis.Client, priority, universe []string, rot *rotator, budget int, obTTL time.Duration, venue string) {
	pollsTotal.Add(1)
	lastPollUnix.Store(time.Now().UnixMilli())

	for _, t := range priority {
		if ctx.Err() != nil {
			return
		}
		writeBook(ctx, src, rdb, t, obTTL)
		writeMarketInfo(ctx, src, rdb, t)
	}
	batch, wrapped, wrapDur := rot.next(universe, budget)
	for _, t := range batch {
		if ctx.Err() != nil {
			return
		}
		writeBook(ctx, src, rdb, t, obTTL)
	}
	watchedTickers.Store(int64(len(priority) + len(universe)))
	priorityTickers.Store(int64(len(priority)))
	if wrapped {
		lastRotationSecs.Store(int64(wrapDur.Seconds()))
		log.Printf("[INFO] %s book rotation: %d universe tickers covered in %v (%d held/watch/hot refreshed every cycle).",
			venue, len(universe), wrapDur.Round(time.Second), len(priority))
		if wrapDur > obTTL {
			log.Printf("[WARN] %s rotation period %v exceeds ORDERBOOK_TTL %v — books will expire before refresh. Raise the TTL or BOOKS_PER_CYCLE, or shrink the discovery universe.",
				venue, wrapDur.Round(time.Second), obTTL)
		}
	}
}

// writeBook fetches one order book, stamps its capture time, and publishes it.
func writeBook(ctx context.Context, src marketSource, rdb *redis.Client, t string, obTTL time.Duration) {
	ob, err := src.FetchOrderBook(t)
	if err != nil {
		pollErrors.Add(1)
		log.Printf("[WARN] orderbook %s: %v", t, err)
		return
	}
	if len(ob.Bids) == 0 && len(ob.Asks) == 0 {
		return
	}
	ob.AsOfMs = time.Now().UnixMilli()
	if data, err := json.Marshal(ob); err == nil {
		_ = rdb.Set(ctx, "orderbook:"+t, string(data), obTTL).Err()
		booksWritten.Add(1)
	}
	publishMomentum(ctx, rdb, t, ob, obTTL)
}

// writeMarketInfo fetches one market status snapshot (settlement/result) and publishes it.
func writeMarketInfo(ctx context.Context, src marketSource, rdb *redis.Client, t string) {
	mi, err := src.FetchMarket(t)
	if err != nil {
		pollErrors.Add(1)
		return
	}
	if data, err := json.Marshal(mi); err == nil {
		_ = rdb.Set(ctx, "market:"+t, string(data), 0).Err()
	}
}

// heldTickers reads open positions from Redis, split by venue: polyOnly selects
// POLY- tickers (Polymarket), otherwise everything else (Kalshi). The old union
// sweep polled held POLY- tickers against the Kalshi API, burning rate limit on
// guaranteed 404s.
func heldTickers(ctx context.Context, rdb *redis.Client, polyOnly bool) []string {
	val, err := rdb.Get(ctx, "gekko:positions").Result()
	if err != nil {
		return nil
	}
	var positions []struct {
		ContractID string `json:"contract_id"`
	}
	if json.Unmarshal([]byte(val), &positions) != nil {
		return nil
	}
	var out []string
	for _, p := range positions {
		if strings.HasPrefix(p.ContractID, polyPrefix) == polyOnly {
			out = append(out, p.ContractID)
		}
	}
	return out
}

// Hot tickers: contracts the fleet is actively proposing on. Breen ZADDs every
// incoming BUY proposal's ticker to this sorted set (score = unix ms); polling
// them every cycle keeps books live exactly where entries will be attempted,
// so the entry book gate judges a seconds-old book instead of a rotation-aged
// one. Members outside the recency window are pruned here on read.
const (
	hotTickersKey    = "gekko:hot_tickers"
	hotTickersWindow = 10 * time.Minute
	hotTickersMax    = 50
)

// hotTickers returns the most recently proposed tickers for one venue (polyOnly
// selects POLY- tickers, otherwise Kalshi), pruning entries older than the window.
func hotTickers(ctx context.Context, rdb *redis.Client, polyOnly bool) []string {
	cutoff := time.Now().Add(-hotTickersWindow).UnixMilli()
	_ = rdb.ZRemRangeByScore(ctx, hotTickersKey, "-inf", "("+strconv.FormatInt(cutoff, 10)).Err()
	members, err := rdb.ZRevRangeByScore(ctx, hotTickersKey, &redis.ZRangeBy{
		Min: strconv.FormatInt(cutoff, 10), Max: "+inf", Count: hotTickersMax,
	}).Result()
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range members {
		if strings.HasPrefix(t, polyPrefix) == polyOnly {
			out = append(out, t)
		}
	}
	return out
}

// splitPriority dedupes the per-cycle priority list (held + watch) and returns the
// discovered universe minus those tickers.
func splitPriority(heldAndWatch, discovered []string) (priority, universe []string) {
	seen := map[string]bool{}
	add := func(dst *[]string, t string) {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			*dst = append(*dst, t)
		}
	}
	for _, t := range heldAndWatch {
		add(&priority, t)
	}
	for _, t := range discovered {
		add(&universe, t)
	}
	return priority, universe
}

// ── rate limiter (min spacing between requests) ─────────────────────────────

type rateLimiter struct {
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

func newRateLimiter(perSec int) *rateLimiter {
	if perSec <= 0 {
		perSec = 8
	}
	return &rateLimiter{interval: time.Second / time.Duration(perSec)}
}

func (r *rateLimiter) wait() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d := r.interval - time.Since(r.last); d > 0 {
		time.Sleep(d)
	}
	r.last = time.Now()
}

// ── env helpers ─────────────────────────────────────────────────────────────

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
