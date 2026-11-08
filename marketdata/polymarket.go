package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// polyMarketsKey is the Redis catalog key Gordon reads alongside gekko:kalshi_markets.
const polyMarketsKey = "gekko:polymarket_markets"

// polyPrefix marks a Polymarket contract id. Lower-cased it contains "poly", so
// breen's inferExchange routes it to the polymarket venue unchanged.
const polyPrefix = "POLY-"

// PolymarketClient is a READ-ONLY Polymarket market-data client. It discovers
// markets via the public Gamma API and fetches order books via the public CLOB
// API. No credentials are required and it never places, modifies, or cancels
// orders. It mirrors the read-only surface of KalshiClient.
type PolymarketClient struct {
	gamma string // e.g. https://gamma-api.polymarket.com
	clob  string // e.g. https://clob.polymarket.com
	http  *http.Client
	lim   *rateLimiter

	// token maps POLY-<tokenId> ticker -> the YES outcome ERC1155 token id used
	// by the CLOB /book endpoint. Populated by ListOpenMarkets.
	mu    sync.RWMutex
	token map[string]string
}

func NewPolymarketClient(gamma, clob string, rate int) *PolymarketClient {
	return &PolymarketClient{
		gamma: strings.TrimSuffix(gamma, "/"),
		clob:  strings.TrimSuffix(clob, "/"),
		http:  &http.Client{Timeout: 15 * time.Second},
		lim:   newRateLimiter(rate),
		token: make(map[string]string),
	}
}

// get performs a read-only GET against a fully-qualified URL.
func (p *PolymarketClient) get(rawURL string) ([]byte, int, error) {
	if p.lim != nil {
		p.lim.wait()
	}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

// gammaMarket is the subset of Gamma's market object we consume. The array
// fields (clobTokenIds, outcomePrices, outcomes) are returned as JSON-encoded
// STRINGS, so they need a second unmarshal.
type gammaMarket struct {
	ID              string  `json:"id"`
	Question        string  `json:"question"`
	ConditionID     string  `json:"conditionId"`
	ClobTokenIDs    string  `json:"clobTokenIds"`
	OutcomePrices   string  `json:"outcomePrices"`
	VolumeNum       float64 `json:"volumeNum"`
	Liquidity       string  `json:"liquidity"`
	Active          bool    `json:"active"`
	Closed          bool    `json:"closed"`
	EnableOrderBook bool    `json:"enableOrderBook"`
	EndDate         string  `json:"endDate"`
}

// ListOpenMarkets pages the Gamma API for active, order-book-enabled markets,
// ranked by volume, and maps them into CatalogMarket entries keyed by the YES
// token. It records the ticker -> YES token-id mapping for later book polling.
func (p *PolymarketClient) ListOpenMarkets(maxMarkets int) ([]CatalogMarket, error) {
	if maxMarkets <= 0 {
		maxMarkets = 150
	}
	const pageSize = 100
	var out []CatalogMarket
	fresh := make(map[string]string)

	for offset := 0; len(out) < maxMarkets; offset += pageSize {
		ep := fmt.Sprintf("%s/markets?active=true&closed=false&limit=%d&offset=%d&order=volumeNum&ascending=false",
			p.gamma, pageSize, offset)
		b, code, err := p.get(ep)
		if err != nil {
			return out, err
		}
		if code != http.StatusOK {
			return out, fmt.Errorf("gamma markets status %d: %s", code, snippet(b))
		}
		var page []gammaMarket
		if err := json.Unmarshal(b, &page); err != nil {
			return out, fmt.Errorf("gamma markets decode: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			if !m.Active || m.Closed || !m.EnableOrderBook {
				continue
			}
			yesTok, _ := parseYesToken(m.ClobTokenIDs)
			if yesTok == "" {
				continue
			}
			ticker := polyPrefix + yesTok
			yesPrice := parseFirstFloat(m.OutcomePrices)
			out = append(out, CatalogMarket{
				Ticker:    ticker,
				Title:     m.Question,
				YesBid:    yesPrice,
				YesAsk:    yesPrice,
				Volume:    m.VolumeNum,
				Status:    "open",
				Event:     m.ConditionID,
				CloseTime: m.EndDate,
			})
			fresh[ticker] = yesTok
			if len(out) >= maxMarkets {
				break
			}
		}
		if len(page) < pageSize {
			break
		}
	}

	p.mu.Lock()
	for k, v := range fresh {
		p.token[k] = v
	}
	p.mu.Unlock()
	return out, nil
}

// FetchOrderBook pulls the CLOB book for the YES token behind a POLY- ticker and
// maps it into the shared YES-side bid/ask OrderBook (prices already in dollars).
func (p *PolymarketClient) FetchOrderBook(ticker string) (*OrderBook, error) {
	tok := p.yesToken(ticker)
	if tok == "" {
		return nil, fmt.Errorf("unknown polymarket ticker %s (no token id)", ticker)
	}
	b, code, err := p.get(p.clob + "/book?token_id=" + url.QueryEscape(tok))
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("book %s status %d: %s", ticker, code, snippet(b))
	}
	ob, err := parsePolyBook(b, ticker)
	if err != nil {
		return nil, err
	}
	return ob, nil
}

// FetchMarket returns a lightweight status/last-price snapshot from the CLOB
// midpoint. In paper mode resolution is not tracked (Result stays empty).
func (p *PolymarketClient) FetchMarket(ticker string) (*MarketInfo, error) {
	tok := p.yesToken(ticker)
	if tok == "" {
		return nil, fmt.Errorf("unknown polymarket ticker %s (no token id)", ticker)
	}
	b, code, err := p.get(p.clob + "/midpoint?token_id=" + url.QueryEscape(tok))
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("midpoint %s status %d: %s", ticker, code, snippet(b))
	}
	var resp struct {
		Mid string `json:"mid"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	mid, _ := strconv.ParseFloat(resp.Mid, 64)
	return &MarketInfo{Ticker: ticker, Status: "open", LastPrice: mid}, nil
}

func (p *PolymarketClient) yesToken(ticker string) string {
	p.mu.RLock()
	tok := p.token[ticker]
	p.mu.RUnlock()
	if tok == "" {
		// Fall back to the embedded id (ticker is POLY-<tokenId>).
		tok = strings.TrimPrefix(ticker, polyPrefix)
	}
	return tok
}

// parseYesToken decodes the JSON-string clobTokenIds array and returns the YES
// token (index 0) and NO token (index 1).
func parseYesToken(clobTokenIDs string) (yes, no string) {
	var ids []string
	if json.Unmarshal([]byte(clobTokenIDs), &ids) != nil {
		return "", ""
	}
	if len(ids) > 0 {
		yes = ids[0]
	}
	if len(ids) > 1 {
		no = ids[1]
	}
	return yes, no
}

// parseFirstFloat decodes a JSON-string array of decimal strings and returns the
// first element as a float (e.g. outcomePrices "[\"0.03\",\"0.97\"]" -> 0.03).
func parseFirstFloat(jsonArr string) float64 {
	var vals []string
	if json.Unmarshal([]byte(jsonArr), &vals) != nil || len(vals) == 0 {
		return 0
	}
	f, _ := strconv.ParseFloat(vals[0], 64)
	return f
}

// parsePolyBook maps a CLOB /book response into a sorted YES-side OrderBook.
func parsePolyBook(b []byte, ticker string) (*OrderBook, error) {
	var resp struct {
		Bids []struct {
			Price string `json:"price"`
			Size  string `json:"size"`
		} `json:"bids"`
		Asks []struct {
			Price string `json:"price"`
			Size  string `json:"size"`
		} `json:"asks"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	ob := &OrderBook{ContractID: ticker}
	for _, lvl := range resp.Bids {
		price, _ := strconv.ParseFloat(lvl.Price, 64)
		size, _ := strconv.ParseFloat(lvl.Size, 64)
		ob.Bids = append(ob.Bids, PriceLevel{Price: price, Volume: int(size)})
	}
	for _, lvl := range resp.Asks {
		price, _ := strconv.ParseFloat(lvl.Price, 64)
		size, _ := strconv.ParseFloat(lvl.Size, 64)
		ob.Asks = append(ob.Asks, PriceLevel{Price: price, Volume: int(size)})
	}
	sort.Slice(ob.Bids, func(i, j int) bool { return ob.Bids[i].Price > ob.Bids[j].Price })
	sort.Slice(ob.Asks, func(i, j int) bool { return ob.Asks[i].Price < ob.Asks[j].Price })
	return ob, nil
}

// ── discovery ───────────────────────────────────────────────────────────────

// polyDiscoverer periodically lists the most liquid open Polymarket markets,
// drives the poll store, and publishes the catalog to gekko:polymarket_markets.
type polyDiscoverer struct {
	client     *PolymarketClient
	rdb        *redis.Client
	store      *discoveredStore
	maxMarkets int
}

func (dc *polyDiscoverer) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("[INFO] Polymarket discovery active (every %v): top %d markets by volume.",
		interval, dc.maxMarkets)

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

func (dc *polyDiscoverer) discoverOnce(ctx context.Context) {
	all, err := dc.client.ListOpenMarkets(dc.maxMarkets)
	if err != nil {
		log.Printf("[WARN] Polymarket discovery failed: %v", err)
		if len(all) == 0 {
			return
		}
	}

	tickers := make([]string, 0, len(all))
	for _, m := range all {
		tickers = append(tickers, m.Ticker)
	}
	dc.store.set(tickers)
	marketsDiscovered.Add(int64(len(all)))

	if dc.rdb != nil {
		if data, err := json.Marshal(all); err == nil {
			_ = dc.rdb.Set(ctx, polyMarketsKey, string(data), 0).Err()
		}
	}
	log.Printf("[INFO] Discovered %d Polymarket markets (published to %s).", len(all), polyMarketsKey)
}

// Polymarket polling runs through the shared pollCycle in main.go (PolymarketClient
// satisfies marketSource); held POLY- positions are selected by heldTickers.
