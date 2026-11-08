package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// discoveredStore holds the latest discovered ticker set, shared with the poll loop
// so the orderbook watch set tracks discovery automatically (no manual watchlist).
type discoveredStore struct {
	mu      sync.RWMutex
	tickers []string
}

func (d *discoveredStore) set(t []string) {
	d.mu.Lock()
	d.tickers = t
	d.mu.Unlock()
}

func (d *discoveredStore) get() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, len(d.tickers))
	copy(out, d.tickers)
	return out
}

// weatherHighTempSeries are Kalshi's daily high-temperature series (category
// "Climate and Weather"), one per city, pinned when KALSHI_WEATHER_ENABLED so their
// low-volume buckets are always cataloged for the weather strategy. City→series was
// confirmed against each series' NWS settlement source (e.g. Chicago = Midway).
var weatherHighTempSeries = []string{
	"KXHIGHNY", "KXHIGHCHI", "KXHIGHMIA", "KXHIGHAUS", "KXHIGHDEN", "KXHIGHLAX", "KXHIGHPHIL",
}

type discoverer struct {
	client     *KalshiClient
	rdb        *redis.Client
	store      *discoveredStore
	maxMarkets int
	maxSeries  int      // cap on category-enumerated series fetched per cycle
	priority   []string // marquee series fetched first (always included)
	categories []string // categories to enumerate (e.g. Crypto, Economics, Financials)
	// strikesPerEvent trims each event's strike ladder to the strikes nearest the
	// money (0 = keep all).
	strikesPerEvent int
}

// run discovers open markets in finance/crypto/econ series and publishes the catalog.
func (dc *discoverer) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("[INFO] Market discovery active (every %v): priority=%v + up to %d series in %v.",
		interval, dc.priority, dc.maxSeries, dc.categories)

	dc.discoverOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dc.discoverOnce(ctx)
		}
	}
}

func (dc *discoverer) discoverOnce(ctx context.Context) {
	// 1. Build the series list: marquee priority first, then category-enumerated fill.
	seen := map[string]bool{}
	var seriesList []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			seriesList = append(seriesList, s)
		}
	}
	for _, s := range dc.priority {
		add(s)
	}
	if dc.maxSeries > 0 && len(dc.categories) > 0 {
		if cat, err := dc.client.ListSeriesByCategory(dc.categories); err != nil {
			log.Printf("[WARN] series enumeration failed: %v", err)
		} else {
			for _, s := range cat {
				if len(seriesList) >= len(dc.priority)+dc.maxSeries {
					break
				}
				add(s)
			}
		}
	}

	prioritySet := map[string]bool{}
	for _, s := range dc.priority {
		if s = strings.TrimSpace(s); s != "" {
			prioritySet[s] = true
		}
	}

	// 2. Fetch open markets for each series (read-only, rate-limited in the client).
	// Markets from priority (pinned) series are kept unconditionally; only the
	// category-enumerated fill competes for the remaining volume budget. This is what
	// keeps intentionally-pinned but low-volume series — e.g. the weather high-temp
	// buckets — from being pruned away by higher-volume noise.
	var keep, fill []CatalogMarket
	for _, s := range seriesList {
		if ctx.Err() != nil {
			return
		}
		mk, err := dc.client.ListMarketsBySeries(s, 100)
		if err != nil {
			continue // series may have no open markets / not exist
		}
		mk = nearMoney(mk, dc.strikesPerEvent)
		if prioritySet[s] {
			keep = append(keep, mk...)
		} else {
			fill = append(fill, mk...)
		}
	}

	// 3. Rank the fill by volume and cap it to the budget left after the pinned set.
	all := capMarkets(keep, fill, dc.maxMarkets)

	tickers := make([]string, 0, len(all))
	for _, m := range all {
		tickers = append(tickers, m.Ticker)
	}
	dc.store.set(tickers)
	marketsDiscovered.Store(int64(len(all)))

	if dc.rdb != nil {
		if data, err := json.Marshal(all); err == nil {
			_ = dc.rdb.Set(ctx, "gekko:kalshi_markets", string(data), 0).Err()
		}
	}
	log.Printf("[INFO] Discovered %d markets across %d series (published to gekko:kalshi_markets).",
		len(all), len(seriesList))
}

// capMarkets keeps every pinned-series market unconditionally and fills the remaining
// budget (maxMarkets − pinned) with the highest-volume category markets. When the
// pinned set alone meets or exceeds the budget, all pinned markets are still kept
// (operator intent wins) and the fill is dropped. Pure/deterministic — unit-tested.
func capMarkets(keep, fill []CatalogMarket, maxMarkets int) []CatalogMarket {
	sort.Slice(fill, func(i, j int) bool { return fill[i].Volume > fill[j].Volume })
	budget := maxMarkets - len(keep)
	if budget < 0 {
		budget = 0
	}
	if len(fill) > budget {
		fill = fill[:budget]
	}
	return append(keep, fill...)
}

// nearMoney keeps, per event, the perEvent markets whose mid is closest to 0.50.
// A 100-strike hourly crypto ladder is almost all one-sided deep-in/out strikes;
// four such ladders used to fill the whole catalog budget and crowd out every
// category-enumerated series. Unquoted markets rank last. Pure — unit-tested.
// ponytail: a held strike that drifts far from the money drops out of the catalog
// (its book is still polled as a held ticker); pin held tickers here if Gordon ever
// needs to match news to them for exits.
func nearMoney(markets []CatalogMarket, perEvent int) []CatalogMarket {
	if perEvent <= 0 {
		return markets
	}
	dist := func(m CatalogMarket) float64 {
		if m.YesBid <= 0 && m.YesAsk <= 0 {
			return 1
		}
		return math.Abs((m.YesBid+m.YesAsk)/2 - 0.5)
	}
	sorted := append([]CatalogMarket(nil), markets...)
	sort.SliceStable(sorted, func(i, j int) bool { return dist(sorted[i]) < dist(sorted[j]) })
	seen := map[string]int{}
	out := sorted[:0]
	for _, m := range sorted {
		if seen[m.Event] < perEvent {
			seen[m.Event]++
			out = append(out, m)
		}
	}
	return out
}
