package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	emath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
)

// Polymarket CLOB order path. Verified against Polymarket's own test vectors
// (python-order-utils tests/test_order_builder.py, see polymarket_test.go) for the EIP-712
// digest and signature, and against py-clob-client for amount rounding and L2 auth.
//
// Still NOT done (paper-only until it is): fill confirmation, cancel, user-channel fills,
// and the consecutive-error failsafe. Orders are GTC and the broker returns once the CLOB
// accepts them, so a resting or partial order is still booked as filled by breen.
// See gekko_improvement_tasks.md and docs/kalshi-live-execution-checklist.md.

const (
	polyChainID         = 137
	polyExchange        = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E" // CTF Exchange
	polyNegRiskExchange = "0xC5d563A36AE78145C45a50134d48A1215220f80a" // Neg Risk CTF Exchange
	polyTickerPrefix    = "POLY-"
	polySideBuy         = uint8(0)
	polySideSell        = uint8(1)
	polySigEOA          = uint8(0)
)

// polyOrder is the signed struct: field order matters, it is the EIP-712 type string.
type polyOrder struct {
	Salt          *big.Int
	Maker         common.Address
	Signer        common.Address
	Taker         common.Address
	TokenID       *big.Int
	MakerAmount   *big.Int
	TakerAmount   *big.Int
	Expiration    *big.Int
	Nonce         *big.Int
	FeeRateBps    *big.Int
	Side          uint8
	SignatureType uint8
}

// clobOrderJSON is the order object in the POST /order body. salt is a JSON number, the rest
// of the big values are decimal strings, side is "BUY"/"SELL".
type clobOrderJSON struct {
	Salt          int64  `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	Taker         string `json:"taker"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Expiration    string `json:"expiration"`
	Nonce         string `json:"nonce"`
	FeeRateBps    string `json:"feeRateBps"`
	Side          string `json:"side"`
	SignatureType int    `json:"signatureType"`
	Signature     string `json:"signature"`
}

type clobPostBody struct {
	Order     clobOrderJSON `json:"order"`
	Owner     string        `json:"owner"` // the API key
	OrderType string        `json:"orderType"`
}

type polyCreds struct{ Key, Secret, Passphrase string }

func polyCredsFromEnv() polyCreds {
	return polyCreds{brokerSecret("POLYMARKET_API_KEY"), brokerSecret("POLYMARKET_API_SECRET"), brokerSecret("POLYMARKET_API_PASSPHRASE")}
}

func (c polyCreds) complete() bool { return c.Key != "" && c.Secret != "" && c.Passphrase != "" }

// polyDigest is the EIP-712 hash of the order under the Polymarket CTF Exchange domain.
func polyDigest(o polyOrder, chainID *big.Int, exchange common.Address) [32]byte {
	domainType := crypto.Keccak256Hash([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	domain := crypto.Keccak256(
		domainType.Bytes(),
		crypto.Keccak256([]byte("Polymarket CTF Exchange")),
		crypto.Keccak256([]byte("1")),
		emath.U256Bytes(new(big.Int).Set(chainID)),
		common.LeftPadBytes(exchange.Bytes(), 32),
	)
	orderType := crypto.Keccak256Hash([]byte("Order(uint256 salt,address maker,address signer,address taker,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint256 expiration,uint256 nonce,uint256 feeRateBps,uint8 side,uint8 signatureType)"))
	u := func(v *big.Int) []byte { return emath.U256Bytes(new(big.Int).Set(v)) }
	structHash := crypto.Keccak256(
		orderType.Bytes(),
		u(o.Salt),
		common.LeftPadBytes(o.Maker.Bytes(), 32),
		common.LeftPadBytes(o.Signer.Bytes(), 32),
		common.LeftPadBytes(o.Taker.Bytes(), 32),
		u(o.TokenID), u(o.MakerAmount), u(o.TakerAmount), u(o.Expiration), u(o.Nonce), u(o.FeeRateBps),
		u(big.NewInt(int64(o.Side))), u(big.NewInt(int64(o.SignatureType))),
	)
	var out [32]byte
	copy(out[:], crypto.Keccak256([]byte("\x19\x01"), domain, structHash))
	return out
}

// polySign signs the digest and returns the 0x-prefixed 65-byte signature with v in {27,28}.
func polySign(digest [32]byte, key *ecdsa.PrivateKey) (string, error) {
	sig, err := crypto.Sign(digest[:], key)
	if err != nil {
		return "", err
	}
	if sig[64] < 27 {
		sig[64] += 27
	}
	return "0x" + hex.EncodeToString(sig), nil
}

// polyAmounts returns the maker and taker amounts (1e6 fixed point) for a limit order, using
// py-clob-client's rounding: price to the tick, size rounded DOWN to 2 decimals. BUY gives
// price*size USDC for size tokens; SELL gives size tokens for price*size USDC. All integer math,
// so the result is exact (price decimals + 2 size decimals never exceed the allowed amount decimals).
func polyAmounts(side uint8, price, size float64, tick float64) (maker, taker *big.Int, err error) {
	pd := 0
	switch tick {
	case 0.1:
		pd = 1
	case 0.01:
		pd = 2
	case 0.001:
		pd = 3
	case 0.0001:
		pd = 4
	default:
		return nil, nil, fmt.Errorf("unsupported tick size %v", tick)
	}
	if math.IsNaN(price) || math.IsNaN(size) || size <= 0 {
		return nil, nil, fmt.Errorf("invalid price %v / size %v", price, size)
	}
	pow := math.Pow10(pd)
	priceTicks := int64(math.Round(price * pow))
	if priceTicks < 1 || priceTicks >= int64(pow) {
		return nil, nil, fmt.Errorf("price %v is outside (0,1) at tick %v", price, tick)
	}
	sizeHund := int64(math.Floor(size*100 + 1e-9)) // size in hundredths of a token, rounded down
	if sizeHund < 1 {
		return nil, nil, fmt.Errorf("size %v rounds to zero", size)
	}
	tokens := new(big.Int).Mul(big.NewInt(sizeHund), big.NewInt(10_000)) // 1e6 scale
	usdc := new(big.Int).Mul(tokens, big.NewInt(priceTicks))
	usdc.Div(usdc, big.NewInt(int64(pow)))
	if side == polySideBuy {
		return usdc, tokens, nil
	}
	return tokens, usdc, nil
}

// polyL2Headers builds the L2 auth headers. The HMAC key is the url-safe-base64 API secret,
// the message is timestamp+METHOD+path+body, the output is url-safe base64.
func polyL2Headers(c polyCreds, address string, ts int64, method, path, body string) (http.Header, error) {
	secret, err := base64.URLEncoding.DecodeString(c.Secret)
	if err != nil {
		// Polymarket secrets are sometimes padded differently; retry unpadded.
		if secret, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(c.Secret, "=")); err != nil {
			return nil, fmt.Errorf("POLYMARKET_API_SECRET is not url-safe base64: %w", err)
		}
	}
	tsStr := strconv.FormatInt(ts, 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(tsStr + method + path + body))
	h := http.Header{}
	h.Set("POLY_ADDRESS", address)
	h.Set("POLY_SIGNATURE", base64.URLEncoding.EncodeToString(mac.Sum(nil)))
	h.Set("POLY_TIMESTAMP", tsStr)
	h.Set("POLY_API_KEY", c.Key)
	h.Set("POLY_PASSPHRASE", c.Passphrase)
	return h, nil
}

// polyTokenID extracts the CLOB token id from a "POLY-<decimal token id>" ticker, the form
// marketdata publishes (it keys on Gamma's clobTokenIds).
func polyTokenID(contractID string) (*big.Int, error) {
	if !strings.HasPrefix(contractID, polyTickerPrefix) {
		return nil, fmt.Errorf("%q is not a Polymarket ticker (want %s<token id>)", contractID, polyTickerPrefix)
	}
	id, ok := new(big.Int).SetString(strings.TrimPrefix(contractID, polyTickerPrefix), 10)
	if !ok || id.Sign() <= 0 {
		return nil, fmt.Errorf("%q does not carry a decimal CLOB token id", contractID)
	}
	return id, nil
}

// polyMeta caches per-token tick size and neg-risk flag from the CLOB.
type polyMeta struct {
	tick    float64
	negRisk bool
}

var (
	polyMetaMu    sync.Mutex
	polyMetaCache = map[string]polyMeta{}
)

func polyGetJSON(client *http.Client, u string, out any) error {
	resp, err := client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d: %s", u, resp.StatusCode, string(b))
	}
	return json.Unmarshal(b, out)
}

func polyLookupMeta(client *http.Client, base, tokenID string) (polyMeta, error) {
	polyMetaMu.Lock()
	m, ok := polyMetaCache[base+"|"+tokenID]
	polyMetaMu.Unlock()
	if ok {
		return m, nil
	}
	q := url.Values{"token_id": {tokenID}}.Encode()
	var tick struct {
		Min float64 `json:"minimum_tick_size"`
	}
	if err := polyGetJSON(client, base+"/tick-size?"+q, &tick); err != nil {
		return polyMeta{}, fmt.Errorf("tick size lookup: %w", err)
	}
	var neg struct {
		NegRisk bool `json:"neg_risk"`
	}
	if err := polyGetJSON(client, base+"/neg-risk?"+q, &neg); err != nil {
		return polyMeta{}, fmt.Errorf("neg-risk lookup: %w", err)
	}
	m = polyMeta{tick: tick.Min, negRisk: neg.NegRisk}
	polyMetaMu.Lock()
	polyMetaCache[base+"|"+tokenID] = m
	polyMetaMu.Unlock()
	return m, nil
}

func polyFail(format string, a ...any) TradeResult {
	return TradeResult{Success: false, Message: fmt.Sprintf(format, a...), Exchange: "polymarket"}
}

// ExecutePolymarket signs an order and submits it to the Polymarket CLOB. Success is reported
// only when the CLOB returns success=true AND an orderID; anything else is a failure carrying
// the CLOB's own error text.
func ExecutePolymarket(intent TradeIntent, privateKeyHex string) TradeResult {
	return executePolymarket(intent, privateKeyHex, polyCredsFromEnv(),
		getEnv("POLYMARKET_CLOB_URL", "https://clob.polymarket.com"), &http.Client{Timeout: 10 * time.Second})
}

func executePolymarket(intent TradeIntent, privateKeyHex string, creds polyCreds, clobBase string, client *http.Client) TradeResult {
	if halted, n := polyAPI.halted(); halted {
		return polyFail("Polymarket trading halted after %d consecutive CLOB API errors; restart the broker to resume", n)
	}
	if !creds.complete() {
		return polyFail("Polymarket API credentials missing (need POLYMARKET_API_KEY, POLYMARKET_API_SECRET, POLYMARKET_API_PASSPHRASE)")
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(privateKeyHex, "0x"))
	if err != nil {
		return polyFail("Invalid Polymarket private key")
	}
	defer key.D.SetInt64(0)
	addr := crypto.PubkeyToAddress(*key.Public().(*ecdsa.PublicKey))

	var side uint8
	switch strings.ToUpper(intent.Action) {
	case "BUY":
		side = polySideBuy
	case "SELL":
		side = polySideSell
	default:
		return polyFail("unknown action %q (want BUY or SELL)", intent.Action)
	}
	tokenID, err := polyTokenID(intent.ContractID)
	if err != nil {
		return polyFail("%v", err)
	}
	meta, err := polyLookupMeta(client, clobBase, tokenID.String())
	if err != nil {
		polyAPI.record(false)
		return polyFail("%v", err)
	}
	maker, taker, err := polyAmounts(side, intent.MaxPrice, float64(intent.Size), meta.tick)
	if err != nil {
		return polyFail("%v", err)
	}
	salt, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 48)) // < 2^53: safe as a JSON number
	if err != nil {
		return polyFail("salt generation failed: %v", err)
	}
	exchange := common.HexToAddress(polyExchange)
	if meta.negRisk {
		exchange = common.HexToAddress(polyNegRiskExchange)
	}
	order := polyOrder{
		Salt: salt, Maker: addr, Signer: addr, Taker: common.Address{}, TokenID: tokenID,
		MakerAmount: maker, TakerAmount: taker, Expiration: big.NewInt(0), Nonce: big.NewInt(0),
		FeeRateBps: big.NewInt(0), Side: side, SignatureType: polySigEOA,
	}
	sig, err := polySign(polyDigest(order, big.NewInt(polyChainID), exchange), key)
	if err != nil {
		return polyFail("ECDSA signing failed: %v", err)
	}
	sideName := "BUY"
	if side == polySideSell {
		sideName = "SELL"
	}
	body, err := json.Marshal(clobPostBody{
		Order: clobOrderJSON{
			Salt: salt.Int64(), Maker: addr.Hex(), Signer: addr.Hex(), Taker: order.Taker.Hex(),
			TokenID: tokenID.String(), MakerAmount: maker.String(), TakerAmount: taker.String(),
			Expiration: "0", Nonce: "0", FeeRateBps: "0", Side: sideName, SignatureType: int(polySigEOA), Signature: sig,
		},
		Owner: creds.Key, OrderType: "GTC",
	})
	if err != nil {
		return polyFail("marshal failed: %v", err)
	}
	hdr, err := polyL2Headers(creds, addr.Hex(), time.Now().Unix(), "POST", "/order", string(body))
	if err != nil {
		return polyFail("%v", err)
	}
	req, err := http.NewRequest("POST", clobBase+"/order", bytes.NewReader(body))
	if err != nil {
		return polyFail("request build failed: %v", err)
	}
	req.Header = hdr
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		polyAPI.record(false)
		return polyFail("Polymarket CLOB request failed: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	polyAPI.record(resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden)

	var r struct {
		Success  bool   `json:"success"`
		ErrorMsg string `json:"errorMsg"`
		OrderID  string `json:"orderID"`
		Status   string `json:"status"`
	}
	_ = json.Unmarshal(respBody, &r)
	if resp.StatusCode != http.StatusOK || !r.Success || r.OrderID == "" {
		msg := r.ErrorMsg
		if msg == "" {
			msg = string(respBody)
		}
		return TradeResult{Success: false, Message: fmt.Sprintf("Polymarket CLOB rejected the order (HTTP %d): %s", resp.StatusCode, msg),
			Exchange: "polymarket", Signature: sig}
	}

	res := polySettle(client, creds, addr.Hex(), clobBase, r.OrderID, intent.Size, polyFillWait(), polyPollEvery)
	res.Signature = sig
	return res
}

// ---- order lifecycle: confirm the fill, cancel what did not fill, trip a failsafe ----

var polyPollEvery = time.Second // var so tests can poll fast

// polyFillWait is how long the broker waits for an order to fill before cancelling the rest.
// Keep it well under breen's broker-call timeout.
func polyFillWait() time.Duration {
	if d, err := time.ParseDuration(getEnv("POLYMARKET_FILL_WAIT", "8s")); err == nil && d > 0 {
		return d
	}
	return 8 * time.Second
}

// polyHealth counts consecutive CLOB API failures (network, 5xx, 401/403). Past the limit the
// broker refuses new Polymarket orders until restarted: a venue that is erroring is not one to
// keep sending orders to. Business rejections (e.g. insufficient balance) are valid replies and
// do not count.
type polyHealth struct {
	mu          sync.Mutex
	consecutive int
}

var polyAPI = &polyHealth{}

func polyMaxAPIErrors() int {
	if n, err := strconv.Atoi(getEnv("POLYMARKET_MAX_API_ERRORS", "5")); err == nil && n > 0 {
		return n
	}
	return 5
}

func (h *polyHealth) record(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok {
		h.consecutive = 0
		return
	}
	h.consecutive++
}

func (h *polyHealth) halted() (bool, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.consecutive >= polyMaxAPIErrors(), h.consecutive
}

type polyOrderState struct {
	Status   string
	Original float64
	Matched  float64
	Price    float64
}

func (s polyOrderState) done() bool {
	if s.Original > 0 && s.Matched >= s.Original-1e-9 {
		return true
	}
	switch strings.ToUpper(s.Status) {
	case "MATCHED", "CANCELED", "CANCELLED", "INVALID", "CANCELED_MARKET_RESOLVED":
		return true
	}
	return false
}

// polyAuthedDo sends an L2-authenticated request and returns status and body. The HMAC covers
// the exact body bytes sent.
func polyAuthedDo(client *http.Client, creds polyCreds, address, base, method, path, body string) (int, []byte, error) {
	hdr, err := polyL2Headers(creds, address, time.Now().Unix(), method, path, body)
	if err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header = hdr
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		polyAPI.record(false)
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	polyAPI.record(resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden)
	return resp.StatusCode, b, nil
}

func polyGetOrder(client *http.Client, creds polyCreds, address, base, orderID string) (polyOrderState, error) {
	code, b, err := polyAuthedDo(client, creds, address, base, "GET", "/data/order/"+orderID, "")
	if err != nil {
		return polyOrderState{}, err
	}
	if code != http.StatusOK {
		return polyOrderState{}, fmt.Errorf("GET order: HTTP %d: %s", code, string(b))
	}
	var o struct {
		Status       string `json:"status"`
		OriginalSize string `json:"original_size"`
		SizeMatched  string `json:"size_matched"`
		Price        string `json:"price"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return polyOrderState{}, fmt.Errorf("GET order: bad JSON: %w", err)
	}
	f := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	return polyOrderState{Status: o.Status, Original: f(o.OriginalSize), Matched: f(o.SizeMatched), Price: f(o.Price)}, nil
}

// polyCancel cancels one order. It succeeds only if the exchange lists the order as cancelled
// (or reports it already gone); a not_canceled entry is an error.
func polyCancel(client *http.Client, creds polyCreds, address, base, orderID string) error {
	body, _ := json.Marshal(map[string]string{"orderID": orderID})
	code, b, err := polyAuthedDo(client, creds, address, base, "DELETE", "/order", string(body))
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("cancel: HTTP %d: %s", code, string(b))
	}
	var r struct {
		Canceled    []string          `json:"canceled"`
		NotCanceled map[string]string `json:"not_canceled"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("cancel: bad JSON: %w", err)
	}
	for _, id := range r.Canceled {
		if id == orderID {
			return nil
		}
	}
	if reason, ok := r.NotCanceled[orderID]; ok {
		return fmt.Errorf("cancel refused: %s", reason)
	}
	return fmt.Errorf("cancel: order %s not in the exchange's reply: %s", orderID, string(b))
}

// polySettle waits up to `wait` for the order to fill, then cancels any unfilled remainder, and
// reports what actually matched. A resting order is never left behind silently.
func polySettle(client *http.Client, creds polyCreds, address, base, orderID string, wantSize int, wait, every time.Duration) TradeResult {
	deadline := time.Now().Add(wait)
	var st polyOrderState
	var lastErr error
	for {
		s, err := polyGetOrder(client, creds, address, base, orderID)
		if err == nil {
			st, lastErr = s, nil
			if st.done() {
				break
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(every)
	}

	complete := st.done() && st.Original > 0 && st.Matched >= st.Original-1e-9
	var cancelErr error
	cancelled := false
	if !complete && !st.done() {
		if cancelErr = polyCancel(client, creds, address, base, orderID); cancelErr == nil {
			cancelled = true
			if s, err := polyGetOrder(client, creds, address, base, orderID); err == nil {
				st = s // final matched size, including anything that filled during the cancel
			}
		}
	}

	filled := int(math.Floor(st.Matched + 1e-9))
	res := TradeResult{Exchange: "polymarket", BrokerTxID: orderID, FilledSize: filled, FilledPrice: st.Price}
	switch {
	case filled > 0 && complete:
		res.Success, res.Status = true, "filled"
		res.Message = fmt.Sprintf("Polymarket order filled: %d of %d contracts", filled, wantSize)
	case filled > 0:
		res.Success, res.Status = true, "partial"
		res.Message = fmt.Sprintf("Polymarket order partially filled: %d of %d contracts", filled, wantSize)
		if cancelErr != nil {
			res.Message += fmt.Sprintf("; CANCEL FAILED (%v), remainder of %s may still be resting", cancelErr, orderID)
		}
	default:
		res.Success, res.Status = false, "unfilled"
		switch {
		case cancelled:
			res.Message = fmt.Sprintf("Polymarket order %s did not fill within %s and was cancelled", orderID, wait)
		case cancelErr != nil:
			res.Message = fmt.Sprintf("Polymarket order %s unfilled and CANCEL FAILED (%v): it may still be resting; check it manually", orderID, cancelErr)
		case lastErr != nil:
			res.Message = fmt.Sprintf("Polymarket order %s status unknown (%v); check it manually", orderID, lastErr)
		default:
			res.Message = fmt.Sprintf("Polymarket order %s ended with status %q and no fills", orderID, st.Status)
		}
	}
	return res
}
