package main

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Golden vectors from Polymarket's python-order-utils tests/test_order_builder.py. The key is
// the publicly known anvil/hardhat account 0; it holds nothing and is not a credential.
const (
	vecKey      = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	vecMaker    = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	vecChain    = 80002
	vecExchange = "0xdFE02Eb6733538f8Ea35D585af8DE5958AD99E40"
)

func vecOrder() polyOrder {
	m := common.HexToAddress(vecMaker)
	return polyOrder{
		Salt: big.NewInt(479249096354), Maker: m, Signer: m, Taker: common.Address{},
		TokenID: big.NewInt(1234), MakerAmount: big.NewInt(100000000), TakerAmount: big.NewInt(50000000),
		Expiration: big.NewInt(0), Nonce: big.NewInt(0), FeeRateBps: big.NewInt(100),
		Side: polySideBuy, SignatureType: polySigEOA,
	}
}

func TestPolyDigestAndSignature_GoldenVectors(t *testing.T) {
	key, _ := crypto.HexToECDSA(vecKey)
	cases := []struct{ name, exchange, digest, sig string }{
		{"exchange", vecExchange,
			"0x02ca1d1aa31103804173ad1acd70066cb6c1258a4be6dada055111f9a7ea4e55",
			"0x302cd9abd0b5fcaa202a344437ec0b6660da984e24ae9ad915a592a90facf5a51bb8a873cd8d270f070217fea1986531d5eec66f1162a81f66e026db653bf7ce1c"},
		{"neg-risk exchange", polyNegRiskExchange,
			"0xf15790d3edc4b5aed427b0b543a9206fcf4b1a13dfed016d33bfb313076263b8",
			"0x1b3646ef347e5bd144c65bd3357ba19c12c12abaeedae733cf8579bc51a2752c0454c3bc6b236957e393637982c769b8dc0706c0f5c399983d933850afd1cbcd1c"},
	}
	for _, c := range cases {
		d := polyDigest(vecOrder(), big.NewInt(vecChain), common.HexToAddress(c.exchange))
		if got := common.Hash(d).Hex(); got != c.digest {
			t.Errorf("%s digest = %s, want %s", c.name, got, c.digest)
		}
		sig, err := polySign(d, key)
		if err != nil || sig != c.sig {
			t.Errorf("%s signature = %s (%v), want %s", c.name, sig, err, c.sig)
		}
	}
}

func TestPolyL2HMAC_GoldenVector(t *testing.T) {
	// py-clob-client tests/signing/test_hmac.py
	h, err := polyL2Headers(polyCreds{"k", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "p"}, "0xabc", 1000000, "test-sign", "/orders", `{"hash": "0x123"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get("POLY_SIGNATURE"); got != "ZwAdJKvoYRlEKDkNMwd5BuwNNtg93kNaR_oU2HrfVvc=" {
		t.Errorf("POLY_SIGNATURE = %s", got)
	}
	for _, k := range []string{"POLY_ADDRESS", "POLY_TIMESTAMP", "POLY_API_KEY", "POLY_PASSPHRASE"} {
		if h.Get(k) == "" {
			t.Errorf("%s header missing", k)
		}
	}
}

func TestPolyAmounts(t *testing.T) {
	cases := []struct {
		side        uint8
		price, size float64
		tick        float64
		maker, tkr  int64
	}{
		{polySideBuy, 0.56, 21.04, 0.01, 11_782_400, 21_040_000},  // USDC out, tokens in
		{polySideSell, 0.56, 21.04, 0.01, 21_040_000, 11_782_400}, // tokens out, USDC in
		{polySideBuy, 0.29, 100, 0.01, 29_000_000, 100_000_000},   // 0.29*100 float error must not truncate the price
		{polySideBuy, 0.123, 10, 0.001, 1_230_000, 10_000_000},
		{polySideBuy, 0.5, 7.999, 0.1, 3_500_000, 7_000_000}, // size rounds DOWN to 7.99; price 0.5
	}
	for _, c := range cases {
		m, tk, err := polyAmounts(c.side, c.price, c.size, c.tick)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		want := [2]int64{c.maker, c.tkr}
		if c.size == 7.999 { // 7.99 tokens at 0.5 = 3.995 USDC
			want = [2]int64{3_995_000, 7_990_000}
		}
		if m.Int64() != want[0] || tk.Int64() != want[1] {
			t.Errorf("%+v: got maker %v taker %v, want %v", c, m, tk, want)
		}
	}
	for _, bad := range []struct{ price, size, tick float64 }{{0, 10, 0.01}, {1, 10, 0.01}, {0.5, 0, 0.01}, {0.5, 0.004, 0.01}, {0.5, 10, 0.05}} {
		if _, _, err := polyAmounts(polySideBuy, bad.price, bad.size, bad.tick); err == nil {
			t.Errorf("%+v should be rejected", bad)
		}
	}
}

func TestPolyTokenID(t *testing.T) {
	id, err := polyTokenID("POLY-1234567890123456789012345678901234567890")
	if err != nil || id.String() != "1234567890123456789012345678901234567890" {
		t.Errorf("got %v, %v", id, err)
	}
	for _, bad := range []string{"KXBTCD-26OCT1517-T81749.99", "POLY-", "POLY-abc", "POLY-0", ""} {
		if _, err := polyTokenID(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// fakeExchange is a scriptable Polymarket CLOB: tick-size, neg-risk, POST/GET/DELETE order.
type fakeExchange struct {
	negRisk bool
	post    func(w http.ResponseWriter, body []byte, r *http.Request) // nil = accept as order 0xorder1
	// getReplies are served in order to GET /data/order/*, the last one repeating.
	getReplies []string
	cancel     string // reply to DELETE /order; "" = cancels 0xorder1
	cancelCode int

	mu        sync.Mutex
	gets      int
	cancels   []string
	cancelHdr http.Header
}

func orderReply(status string, original, matched float64) string {
	b, _ := json.Marshal(map[string]string{
		"status": status, "original_size": strconv.FormatFloat(original, 'f', -1, 64),
		"size_matched": strconv.FormatFloat(matched, 'f', -1, 64), "price": "0.29",
	})
	return string(b)
}

func (e *fakeExchange) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		case r.URL.Path == "/neg-risk":
			_ = json.NewEncoder(w).Encode(map[string]bool{"neg_risk": e.negRisk})
		case r.URL.Path == "/order" && r.Method == "POST":
			b, _ := io.ReadAll(r.Body)
			if e.post != nil {
				e.post(w, b, r)
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"orderID":"0xorder1","status":"live"}`))
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			e.mu.Lock()
			i := e.gets
			e.gets++
			e.mu.Unlock()
			if len(e.getReplies) == 0 {
				_, _ = w.Write([]byte(orderReply("MATCHED", 100, 100)))
				return
			}
			if i >= len(e.getReplies) {
				i = len(e.getReplies) - 1
			}
			_, _ = w.Write([]byte(e.getReplies[i]))
		case r.URL.Path == "/order" && r.Method == "DELETE":
			b, _ := io.ReadAll(r.Body)
			e.mu.Lock()
			e.cancels = append(e.cancels, string(b))
			e.cancelHdr = r.Header.Clone()
			e.mu.Unlock()
			if e.cancelCode != 0 {
				w.WriteHeader(e.cancelCode)
			}
			if e.cancel != "" {
				_, _ = w.Write([]byte(e.cancel))
				return
			}
			_, _ = w.Write([]byte(`{"canceled":["0xorder1"],"not_canceled":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// fakeCLOB keeps the older tests' shape: accepts orders, fills them fully straight away.
func fakeCLOB(t *testing.T, negRisk bool, post func(w http.ResponseWriter, body []byte, r *http.Request)) *httptest.Server {
	t.Helper()
	return (&fakeExchange{negRisk: negRisk, post: post}).serve(t)
}

// fastLifecycle makes the fill wait and poll interval tiny and resets the failsafe counter.
func fastLifecycle(t *testing.T) {
	t.Helper()
	t.Setenv("POLYMARKET_FILL_WAIT", "60ms")
	old := polyPollEvery
	polyPollEvery = 10 * time.Millisecond
	polyAPI = &polyHealth{}
	polyMetaCache = map[string]polyMeta{}
	t.Cleanup(func() { polyPollEvery = old; polyAPI = &polyHealth{} })
}

func testCreds() polyCreds {
	return polyCreds{"api-key", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "pass"}
}

func intent() TradeIntent {
	return TradeIntent{ContractID: "POLY-987654321", Action: "BUY", Size: 100, MaxPrice: 0.29}
}

func TestExecutePolymarket_Success(t *testing.T) {
	fastLifecycle(t)
	var gotBody clobPostBody
	var gotHdr http.Header
	srv := fakeCLOB(t, false, func(w http.ResponseWriter, b []byte, r *http.Request) {
		gotHdr = r.Header
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"success":true,"orderID":"0xorder1","status":"live","errorMsg":""}`))
	})
	defer srv.Close()
	polyMetaCache = map[string]polyMeta{}
	res := executePolymarket(intent(), vecKey, testCreds(), srv.URL, srv.Client())
	if !res.Success || res.BrokerTxID != "0xorder1" {
		t.Fatalf("want success with order id, got %+v", res)
	}
	o := gotBody.Order
	if o.Side != "BUY" || o.TokenID != "987654321" || o.MakerAmount != "29000000" || o.TakerAmount != "100000000" || o.SignatureType != 0 {
		t.Errorf("unexpected order body: %+v", o)
	}
	if gotBody.Owner != "api-key" || gotBody.OrderType != "GTC" || !strings.HasPrefix(o.Signature, "0x") || len(o.Signature) != 132 {
		t.Errorf("unexpected envelope: %+v", gotBody)
	}
	if gotHdr.Get("POLY_API_KEY") != "api-key" || gotHdr.Get("POLY_SIGNATURE") == "" || !strings.EqualFold(gotHdr.Get("POLY_ADDRESS"), o.Maker) {
		t.Errorf("L2 headers wrong: %v", gotHdr)
	}
}

func TestExecutePolymarket_NeverReportsFalseSuccess(t *testing.T) {
	fastLifecycle(t)
	replies := map[string]string{
		"success false":      `{"success":false,"errorMsg":"not enough balance / allowance","orderID":""}`,
		"success but no id":  `{"success":true,"orderID":""}`,
		"id but not success": `{"success":false,"orderID":"0xabc","errorMsg":"rejected"}`,
		"empty body":         ``,
		"not json":           `<html>oops</html>`,
	}
	for name, reply := range replies {
		srv := fakeCLOB(t, false, func(w http.ResponseWriter, b []byte, r *http.Request) { _, _ = w.Write([]byte(reply)) })
		polyMetaCache = map[string]polyMeta{}
		res := executePolymarket(intent(), vecKey, testCreds(), srv.URL, srv.Client())
		srv.Close()
		if res.Success {
			t.Errorf("%s: must not be success, got %+v", name, res)
		}
	}
	srv := fakeCLOB(t, false, func(w http.ResponseWriter, b []byte, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	})
	defer srv.Close()
	polyMetaCache = map[string]polyMeta{}
	res := executePolymarket(intent(), vecKey, testCreds(), srv.URL, srv.Client())
	if res.Success || !strings.Contains(res.Message, "invalid api key") {
		t.Errorf("HTTP 401 must fail with the CLOB message, got %+v", res)
	}
}

func TestExecutePolymarket_SignsForTheRightExchange(t *testing.T) {
	fastLifecycle(t)
	for _, neg := range []bool{false, true} {
		var body clobPostBody
		srv := fakeCLOB(t, neg, func(w http.ResponseWriter, b []byte, r *http.Request) {
			_ = json.Unmarshal(b, &body)
			_, _ = w.Write([]byte(`{"success":true,"orderID":"0x1"}`))
		})
		polyMetaCache = map[string]polyMeta{}
		executePolymarket(intent(), vecKey, testCreds(), srv.URL, srv.Client())
		srv.Close()

		o := body.Order
		tok, _ := new(big.Int).SetString(o.TokenID, 10)
		mk, _ := new(big.Int).SetString(o.MakerAmount, 10)
		tk, _ := new(big.Int).SetString(o.TakerAmount, 10)
		rebuilt := polyOrder{Salt: big.NewInt(o.Salt), Maker: common.HexToAddress(o.Maker), Signer: common.HexToAddress(o.Signer),
			TokenID: tok, MakerAmount: mk, TakerAmount: tk, Expiration: big.NewInt(0), Nonce: big.NewInt(0), FeeRateBps: big.NewInt(0)}
		sig := common.FromHex(o.Signature)
		sig[64] -= 27
		recover := func(exchange string) string {
			d := polyDigest(rebuilt, big.NewInt(polyChainID), common.HexToAddress(exchange))
			pub, err := crypto.SigToPub(d[:], sig)
			if err != nil {
				return ""
			}
			return crypto.PubkeyToAddress(*pub).Hex()
		}
		right, wrong := polyExchange, polyNegRiskExchange
		if neg {
			right, wrong = polyNegRiskExchange, polyExchange
		}
		if recover(right) != o.Maker {
			t.Errorf("neg_risk=%v: signature does not recover to the maker under the expected exchange", neg)
		}
		if recover(wrong) == o.Maker {
			t.Errorf("neg_risk=%v: signature also verifies under the other exchange", neg)
		}
	}
}

func TestExecutePolymarket_RejectsBadInputsBeforePostingAnOrder(t *testing.T) {
	fastLifecycle(t)
	srv := fakeCLOB(t, false, func(w http.ResponseWriter, b []byte, r *http.Request) {
		t.Errorf("an invalid intent must never reach POST /order")
	})
	defer srv.Close()
	cases := map[string]struct {
		in    TradeIntent
		creds polyCreds
	}{
		"kalshi ticker":  {TradeIntent{ContractID: "KXBTCD-26OCT1517-T81749.99", Action: "BUY", Size: 10, MaxPrice: 0.3}, testCreds()},
		"unknown action": {TradeIntent{ContractID: "POLY-123", Action: "HOLD", Size: 10, MaxPrice: 0.3}, testCreds()},
		"zero price":     {TradeIntent{ContractID: "POLY-123", Action: "BUY", Size: 10, MaxPrice: 0}, testCreds()},
		"missing creds":  {TradeIntent{ContractID: "POLY-123", Action: "BUY", Size: 10, MaxPrice: 0.3}, polyCreds{}},
	}
	for name, c := range cases {
		polyMetaCache = map[string]polyMeta{}
		if res := executePolymarket(c.in, vecKey, c.creds, srv.URL, srv.Client()); res.Success {
			t.Errorf("%s: want failure, got %+v", name, res)
		}
	}
	if res := executePolymarket(intent(), "not-a-key", testCreds(), srv.URL, srv.Client()); res.Success || strings.Contains(res.Message, "not-a-key") {
		t.Errorf("bad key: %+v", res)
	}
}

func TestRouteExchange(t *testing.T) {
	cases := map[string]TradeIntent{
		"kalshi":     {ContractID: "KXBTCD-26OCT1517-T81749.99"}, // contains "btc": must NOT go to Polymarket
		"polymarket": {ContractID: "POLY-12345"},
	}
	for want, in := range cases {
		if got := routeExchange(in); got != want {
			t.Errorf("%s routed to %s", in.ContractID, got)
		}
	}
	if got := routeExchange(TradeIntent{ContractID: "POLY-1", Exchange: " Kalshi "}); got != "kalshi" {
		t.Errorf("explicit exchange must win, got %s", got)
	}
}

func run(t *testing.T, e *fakeExchange) TradeResult {
	t.Helper()
	fastLifecycle(t)
	srv := e.serve(t)
	defer srv.Close()
	return executePolymarket(intent(), vecKey, testCreds(), srv.URL, srv.Client())
}

func TestLifecycle_FilledAfterAPoll(t *testing.T) {
	res := run(t, &fakeExchange{getReplies: []string{orderReply("LIVE", 100, 0), orderReply("MATCHED", 100, 100)}})
	if !res.Success || res.Status != "filled" || res.FilledSize != 100 || res.FilledPrice != 0.29 || res.BrokerTxID != "0xorder1" {
		t.Errorf("want filled 100 @0.29, got %+v", res)
	}
}

func TestLifecycle_PartialFillCancelsTheRemainder(t *testing.T) {
	e := &fakeExchange{getReplies: []string{orderReply("LIVE", 100, 40)}}
	res := run(t, e)
	if !res.Success || res.Status != "partial" || res.FilledSize != 40 {
		t.Errorf("want partial 40, got %+v", res)
	}
	if len(e.cancels) != 1 || e.cancels[0] != `{"orderID":"0xorder1"}` {
		t.Errorf("the unfilled remainder must be cancelled once, got %v", e.cancels)
	}
}

func TestLifecycle_UnfilledIsCancelledAndNotBooked(t *testing.T) {
	e := &fakeExchange{getReplies: []string{orderReply("LIVE", 100, 0)}}
	res := run(t, e)
	if res.Success || res.Status != "unfilled" || res.FilledSize != 0 || !strings.Contains(res.Message, "cancelled") {
		t.Errorf("an unfilled order must fail so breen books nothing, got %+v", res)
	}
	if len(e.cancels) != 1 {
		t.Fatalf("want 1 cancel, got %v", e.cancels)
	}
	// The cancel carries L2 auth, signed over the exact body sent.
	want, _ := polyL2Headers(testCreds(), e.cancelHdr.Get("POLY_ADDRESS"), mustInt(t, e.cancelHdr.Get("POLY_TIMESTAMP")), "DELETE", "/order", e.cancels[0])
	if e.cancelHdr.Get("POLY_SIGNATURE") != want.Get("POLY_SIGNATURE") || e.cancelHdr.Get("POLY_API_KEY") != "api-key" {
		t.Errorf("cancel is not correctly authenticated: %v", e.cancelHdr)
	}
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLifecycle_CancelFailureIsReportedLoudly(t *testing.T) {
	e := &fakeExchange{
		getReplies: []string{orderReply("LIVE", 100, 0)},
		cancel:     `{"canceled":[],"not_canceled":{"0xorder1":"order already matched"}}`,
	}
	res := run(t, e)
	if res.Success || !strings.Contains(res.Message, "CANCEL FAILED") || res.BrokerTxID != "0xorder1" {
		t.Errorf("a failed cancel must say so and carry the order id, got %+v", res)
	}
}

func TestLifecycle_AlreadyCancelledNeedsNoCancel(t *testing.T) {
	e := &fakeExchange{getReplies: []string{orderReply("CANCELED", 100, 0)}}
	res := run(t, e)
	if res.Success || len(e.cancels) != 0 {
		t.Errorf("a terminal order must not be cancelled again: %+v cancels=%v", res, e.cancels)
	}
}

func TestLifecycle_UnreachableOrderStatusIsNotReportedAsFilled(t *testing.T) {
	e := &fakeExchange{post: nil}
	fastLifecycle(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		case r.URL.Path == "/neg-risk":
			_, _ = w.Write([]byte(`{"neg_risk":false}`))
		case r.URL.Path == "/order" && r.Method == "POST":
			_, _ = w.Write([]byte(`{"success":true,"orderID":"0xorder1"}`))
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	_ = e
	res := executePolymarket(intent(), vecKey, testCreds(), srv.URL, srv.Client())
	if res.Success || res.FilledSize != 0 {
		t.Errorf("unknown status must never read as a fill, got %+v", res)
	}
}

func TestFailsafe_HaltsAfterConsecutiveAPIErrors(t *testing.T) {
	t.Setenv("POLYMARKET_MAX_API_ERRORS", "2")
	posts := 0
	e := &fakeExchange{post: func(w http.ResponseWriter, b []byte, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusInternalServerError)
	}}
	fastLifecycle(t)
	srv := e.serve(t)
	defer srv.Close()
	for i := 0; i < 4; i++ {
		executePolymarket(intent(), vecKey, testCreds(), srv.URL, srv.Client())
	}
	if posts != 2 {
		t.Errorf("after 2 consecutive API errors no more orders may be sent, but %d were", posts)
	}
	if res := executePolymarket(intent(), vecKey, testCreds(), srv.URL, srv.Client()); res.Success || !strings.Contains(res.Message, "halted") {
		t.Errorf("want a halted refusal, got %+v", res)
	}
	// A business rejection (HTTP 400) is a valid reply and must not count toward the halt.
	polyAPI = &polyHealth{}
	e.post = func(w http.ResponseWriter, b []byte, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"not enough balance"}`))
	}
	for i := 0; i < 5; i++ {
		executePolymarket(intent(), vecKey, testCreds(), srv.URL, srv.Client())
	}
	if h, _ := polyAPI.halted(); h {
		t.Error("business rejections must not trip the failsafe")
	}
}
