package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// KalshiClient is a READ-ONLY Kalshi market-data client. It only ever issues
// signed GET requests — there is intentionally no order-placement code here.
type KalshiClient struct {
	base     string // e.g. https://api.elections.kalshi.com/trade-api/v2
	basePath string // /trade-api/v2  (path portion, used in the signature)
	apiKey   string
	priv     *rsa.PrivateKey
	http     *http.Client
	lim      *rateLimiter // shared rate limiter applied to every request
}

func NewKalshiClient(base, apiKey string, priv *rsa.PrivateKey, rate int) *KalshiClient {
	base = strings.TrimSuffix(base, "/")
	basePath := "/trade-api/v2"
	if i := strings.Index(base, "://"); i != -1 {
		if j := strings.Index(base[i+3:], "/"); j != -1 {
			basePath = base[i+3+j:]
		}
	}
	return &KalshiClient{
		base:     base,
		basePath: basePath,
		apiKey:   apiKey,
		priv:     priv,
		http:     &http.Client{Timeout: 15 * time.Second},
		lim:      newRateLimiter(rate),
	}
}

// signedGet performs a read-only RSA-PSS-signed GET. endpoint is relative to base
// (e.g. "/markets/FOO/orderbook"). Mirrors Kalshi's v2 auth scheme.
func (k *KalshiClient) signedGet(endpoint string) ([]byte, int, error) {
	if k.lim != nil {
		k.lim.wait()
	}
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	path := k.basePath + endpoint
	msg := ts + "GET" + path
	hashed := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPSS(rand.Reader, k.priv, crypto.SHA256, hashed[:], nil)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest("GET", k.base+endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("KALSHI-ACCESS-KEY", k.apiKey)
	req.Header.Set("KALSHI-ACCESS-SIGNATURE", base64.StdEncoding.EncodeToString(sig))
	req.Header.Set("KALSHI-ACCESS-TIMESTAMP", ts)

	resp, err := k.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

// OrderBook matches the shape gordon/breen read from Redis.
type OrderBook struct {
	ContractID string       `json:"contract_id"`
	Bids       []PriceLevel `json:"bids"`
	Asks       []PriceLevel `json:"asks"`
	// AsOfMs is stamped at write time so consumers can judge freshness directly
	// instead of inferring it from the Redis key TTL (which must now outlive a
	// full universe rotation).
	AsOfMs int64 `json:"as_of_ms,omitempty"`
}

type PriceLevel struct {
	Price  float64 `json:"price"`
	Volume int     `json:"volume"`
}

// FetchOrderBook pulls Kalshi's orderbook_fp (dollars) book and maps it into a
// YES-side bid/ask book. Levels are [price, size] strings in dollars. A YES ask
// is derived from the NO bids (buying YES at P == selling NO at 1-P).
func (k *KalshiClient) FetchOrderBook(ticker string) (*OrderBook, error) {
	b, code, err := k.signedGet("/markets/" + ticker + "/orderbook")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("orderbook %s status %d: %s", ticker, code, snippet(b))
	}

	var resp struct {
		OrderbookFP struct {
			YesDollars [][]string `json:"yes_dollars"`
			NoDollars  [][]string `json:"no_dollars"`
		} `json:"orderbook_fp"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}

	ob := &OrderBook{ContractID: ticker}
	for _, lvl := range resp.OrderbookFP.YesDollars {
		if len(lvl) >= 2 {
			price, _ := strconv.ParseFloat(lvl[0], 64)
			size, _ := strconv.ParseFloat(lvl[1], 64)
			ob.Bids = append(ob.Bids, PriceLevel{Price: price, Volume: int(size)})
		}
	}
	for _, lvl := range resp.OrderbookFP.NoDollars {
		if len(lvl) >= 2 {
			noPrice, _ := strconv.ParseFloat(lvl[0], 64)
			size, _ := strconv.ParseFloat(lvl[1], 64)
			ob.Asks = append(ob.Asks, PriceLevel{Price: 1.0 - noPrice, Volume: int(size)})
		}
	}
	sort.Slice(ob.Bids, func(i, j int) bool { return ob.Bids[i].Price > ob.Bids[j].Price })
	sort.Slice(ob.Asks, func(i, j int) bool { return ob.Asks[i].Price < ob.Asks[j].Price })
	return ob, nil
}

// CatalogMarket is a discovered open market, published for Gordon to match against.
type CatalogMarket struct {
	Ticker string  `json:"ticker"`
	Title  string  `json:"title"`
	YesBid float64 `json:"yes_bid"`
	YesAsk float64 `json:"yes_ask"`
	Volume float64 `json:"volume"`
	Status string  `json:"status"`
	Event  string  `json:"event"`
	// CloseTime is when trading stops (RFC3339). Tickers only encode it for some
	// series, and never for Polymarket.
	CloseTime string `json:"close_time,omitempty"`
}

// ListOpenMarkets pages through open markets (read-only GET), up to maxPages of 1000.
func (k *KalshiClient) ListOpenMarkets(maxPages int) ([]CatalogMarket, error) {
	var out []CatalogMarket
	cursor := ""
	for page := 0; page < maxPages; page++ {
		ep := "/markets?status=open&limit=1000"
		if cursor != "" {
			ep += "&cursor=" + cursor
		}
		b, code, err := k.signedGet(ep)
		if err != nil {
			return out, err
		}
		if code != http.StatusOK {
			return out, fmt.Errorf("markets list status %d: %s", code, snippet(b))
		}
		var resp struct {
			Cursor  string `json:"cursor"`
			Markets []struct {
				Ticker string `json:"ticker"`
				Title  string `json:"title"`
				YesBid string `json:"yes_bid_dollars"`
				YesAsk string `json:"yes_ask_dollars"`
				Volume string `json:"volume_fp"`
				Status string `json:"status"`
				Event  string `json:"event_ticker"`
				Close  string `json:"close_time"`
			} `json:"markets"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			return out, err
		}
		for _, m := range resp.Markets {
			vol, _ := strconv.ParseFloat(m.Volume, 64)
			yb, _ := strconv.ParseFloat(m.YesBid, 64)
			ya, _ := strconv.ParseFloat(m.YesAsk, 64)
			out = append(out, CatalogMarket{
				Ticker: m.Ticker, Title: m.Title, YesBid: yb, YesAsk: ya,
				Volume: vol, Status: m.Status, Event: m.Event, CloseTime: m.Close,
			})
		}
		cursor = resp.Cursor
		if cursor == "" || len(resp.Markets) == 0 {
			break
		}
	}
	return out, nil
}

// ListMarketsBySeries fetches open markets for a single series (read-only GET).
func (k *KalshiClient) ListMarketsBySeries(series string, limit int) ([]CatalogMarket, error) {
	if limit <= 0 {
		limit = 100
	}
	b, code, err := k.signedGet(fmt.Sprintf("/markets?status=open&series_ticker=%s&limit=%d", series, limit))
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("series %s status %d", series, code)
	}
	var resp struct {
		Markets []struct {
			Ticker string `json:"ticker"`
			Title  string `json:"title"`
			Sub    string `json:"yes_sub_title"`
			YesBid string `json:"yes_bid_dollars"`
			YesAsk string `json:"yes_ask_dollars"`
			Volume string `json:"volume_fp"`
			Status string `json:"status"`
			Event  string `json:"event_ticker"`
			Close  string `json:"close_time"`
		} `json:"markets"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	out := make([]CatalogMarket, 0, len(resp.Markets))
	for _, m := range resp.Markets {
		vol, _ := strconv.ParseFloat(m.Volume, 64)
		yb, _ := strconv.ParseFloat(m.YesBid, 64)
		ya, _ := strconv.ParseFloat(m.YesAsk, 64)
		title := m.Title
		if m.Sub != "" {
			title = m.Title + " — " + m.Sub
		}
		out = append(out, CatalogMarket{
			Ticker: m.Ticker, Title: title, YesBid: yb, YesAsk: ya,
			Volume: vol, Status: m.Status, Event: m.Event, CloseTime: m.Close,
		})
	}
	return out, nil
}

// ListSeriesByCategory returns KX-prefixed series tickers in the given categories.
func (k *KalshiClient) ListSeriesByCategory(categories []string) ([]string, error) {
	b, code, err := k.signedGet("/series")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("series list status %d", code)
	}
	var resp struct {
		Series []struct {
			Ticker   string `json:"ticker"`
			Category string `json:"category"`
		} `json:"series"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, c := range categories {
		want[strings.ToLower(strings.TrimSpace(c))] = true
	}
	var out []string
	for _, s := range resp.Series {
		if strings.HasPrefix(s.Ticker, "KX") && (len(want) == 0 || want[strings.ToLower(s.Category)]) {
			out = append(out, s.Ticker)
		}
	}
	return out, nil
}

// snippet truncates a response body for safe, compact logging.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// MarketInfo is the per-market status/last-price/resolution we cache for the sim.
type MarketInfo struct {
	Ticker    string  `json:"ticker"`
	Status    string  `json:"status"`
	LastPrice float64 `json:"last_price"`
	Result    string  `json:"result"`
}

func (k *KalshiClient) FetchMarket(ticker string) (*MarketInfo, error) {
	b, code, err := k.signedGet("/markets/" + ticker)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("market %s status %d", ticker, code)
	}
	var resp struct {
		Market struct {
			Status           string `json:"status"`
			LastPriceDollars string `json:"last_price_dollars"`
			Result           string `json:"result"`
		} `json:"market"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	last, _ := strconv.ParseFloat(resp.Market.LastPriceDollars, 64)
	return &MarketInfo{
		Ticker:    ticker,
		Status:    resp.Market.Status,
		LastPrice: last,
		Result:    resp.Market.Result,
	}, nil
}

// loadRSAKey reads a PEM PKCS8 RSA key from a file path or inline PEM string.
// The key value is never logged.
func loadRSAKey(file, inline string) (*rsa.PrivateKey, error) {
	var pemBytes []byte
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read key file: %w", err)
		}
		pemBytes = b
	} else if inline != "" {
		pemBytes = []byte(strings.ReplaceAll(inline, `\n`, "\n"))
	} else {
		return nil, fmt.Errorf("no key provided")
	}

	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS1 as a fallback.
		if k1, err1 := x509.ParsePKCS1PrivateKey(block.Bytes); err1 == nil {
			return k1, nil
		}
		return nil, fmt.Errorf("parse PKCS8 key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not RSA (%T)", key)
	}
	return rsaKey, nil
}
