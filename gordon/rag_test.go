package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaClient_GetEmbedding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			t.Errorf("Expected path /api/embeddings, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"embedding": []float32{0.1, 0.2, 0.3},
		})
	}))
	defer server.Close()

	client := newTestProposalClient(server.URL, "deepseek-r1:7b", "nomic-embed-text", "qwen2.5:1.5b")
	emb, err := client.GetEmbedding(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(emb) != 3 || emb[0] != 0.1 || emb[1] != 0.2 || emb[2] != 0.3 {
		t.Errorf("Unexpected embedding: %v", emb)
	}
}

func TestOllamaClient_GenerateProposal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("Expected path /api/generate, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		// Simulate reasoning output with think tags and JSON
		proposalJSON := `{"contract_id": "AAPL-UP-200", "action": "BUY", "max_price": 0.75, "size": 1500, "proposal": "test rationale"}`
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "<think>some thinking...</think>\n" + proposalJSON,
		})
	}))
	defer server.Close()

	client := newTestProposalClient(server.URL, "deepseek-r1:7b", "nomic-embed-text", "")
	res, err := client.GenerateProposal(context.Background(), "test event", nil, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if res.ContractID != "AAPL-UP-200" || res.Action != "BUY" || res.MaxPrice != 0.75 || res.Size != 1500 || res.Proposal != "test rationale" {
		t.Errorf("Unexpected proposal result: %+v", res)
	}
}

// A non-BUY/SELL action (HOLD/NO/PASS) must surface ErrLLMDeclined so the caller
// drops the event, rather than being silently coerced to a BUY (2026-07-10 fix).
func TestGenerateProposal_DeclineOnNonBuySell(t *testing.T) {
	for _, action := range []string{"HOLD", "NO", "PASS", ""} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			body := `{"contract_id": "CPI-YES", "action": "` + action + `", "max_price": 0.4, "size": 100, "proposal": "no edge"}`
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"response": body})
		}))
		client := newTestProposalClient(server.URL, "qwen2.5:7b", "nomic-embed-text", "")
		res, err := client.GenerateProposal(context.Background(), "event", nil, nil)
		server.Close()
		if !errors.Is(err, ErrLLMDeclined) {
			t.Errorf("action %q: expected ErrLLMDeclined, got res=%+v err=%v", action, res, err)
		}
		if res != nil {
			t.Errorf("action %q: expected nil proposal on decline, got %+v", action, res)
		}
	}
}

func TestOllamaClient_GenerateProposal_LiveMarketInPrompt(t *testing.T) {
	var gotPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotPrompt = req.Prompt
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"response": `{"contract_id": "KXCPI-26JUL", "action": "BUY", "max_price": 0.62, "size": 800, "proposal": "edge"}`,
		})
	}))
	defer server.Close()

	client := newTestProposalClient(server.URL, "qwen2.5:7b", "nomic-embed-text", "")
	mkt := &LiveMarket{Ticker: "KXCPI-26JUL", Title: "Will CPI exceed 3% in July?", YesBid: 0.40, YesAsk: 0.45}
	if _, err := client.GenerateProposal(context.Background(), "CPI news", nil, mkt); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	for _, want := range []string{"Will CPI exceed 3% in July?", "KXCPI-26JUL", "$0.40 / $0.45"} {
		if !strings.Contains(gotPrompt, want) {
			t.Errorf("prompt missing live-market context %q", want)
		}
	}
}

func TestMicrostructureLines(t *testing.T) {
	// Full feature set renders one terse line each.
	m := &LiveMarket{YesBid: 0.42, YesAsk: 0.50, Imbalance: -0.6, TrendPct: -3.2, VolSurge: 2.1, HoursToExp: 3}
	lines := m.microstructureLines()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"spread 8¢", "ask-heavy", "-3.2%", "×2.1", "~3h"} {
		if !strings.Contains(joined, want) {
			t.Errorf("microstructure lines missing %q; got:\n%s", want, joined)
		}
	}

	// Bid-heavy book is labeled as buy pressure.
	if l := (&LiveMarket{Imbalance: 0.5}).microstructureLines(); len(l) != 1 || !strings.Contains(l[0], "bid-heavy") {
		t.Errorf("bid-heavy label wrong: %v", l)
	}

	// Neutral/zero fields are omitted; a below-surge volume is dropped.
	if l := (&LiveMarket{VolSurge: 1.1}).microstructureLines(); len(l) != 0 {
		t.Errorf("neutral market should render no lines, got %v", l)
	}

	// Far expiry renders in days, not hours.
	if l := (&LiveMarket{HoursToExp: 120}).microstructureLines(); len(l) != 1 || !strings.Contains(l[0], "days") {
		t.Errorf("far expiry should render days: %v", l)
	}

	// A crossed/absent book omits the spread line.
	if l := (&LiveMarket{YesBid: 0.5, YesAsk: 0.5}).microstructureLines(); len(l) != 0 {
		t.Errorf("no positive spread should render no lines, got %v", l)
	}
}

// TestExtractJSON_SmartQuotes reproduces the 2026-07-14 gemma-neutral failure: the
// model closed the "proposal" string value with a curly ” after opening with a
// straight ", so json.Unmarshal saw an unterminated string ("unexpected end of JSON
// input") and the proposal was dropped. extractJSON must fold the curly quotes so the
// payload parses.
func TestExtractJSON_SmartQuotes(t *testing.T) {
	// Real shape from the log: mixed straight/curly quotes, a curly apostrophe in the
	// prose, and the trailing curly ” that broke the parse.
	raw := "Here you go:\n{\"contract_id\": \"KXCPI-26JUL-T-0.3\", \"action\": \"SELL\", \"max_price\": 0.85, \"size\": 1000, \"proposal\": \"bear’s losses make 85% a reasonable underweight position.”}"

	clean := extractJSON(raw)
	var got rawLLMProposalResponse
	if err := json.Unmarshal([]byte(clean), &got); err != nil {
		t.Fatalf("expected smart-quote payload to parse after extractJSON, got: %v\ncleaned: %s", err, clean)
	}
	if got.ContractID != "KXCPI-26JUL-T-0.3" || got.Action != "SELL" {
		t.Errorf("parsed fields wrong: contract=%q action=%q", got.ContractID, got.Action)
	}
	if strings.ContainsAny(clean, "“”‘’") {
		t.Errorf("curly quotes survived normalization: %s", clean)
	}
}

// TestNormalizeSmartQuotes covers each curly form folding to ASCII, and that ASCII
// input is left untouched.
func TestNormalizeSmartQuotes(t *testing.T) {
	in := "“a” ‘b’"
	want := `"a" 'b'`
	if got := normalizeSmartQuotes(in); got != want {
		t.Errorf("normalizeSmartQuotes(%q) = %q, want %q", in, got, want)
	}
	plain := `{"k":"v"}`
	if got := normalizeSmartQuotes(plain); got != plain {
		t.Errorf("ASCII input mutated: %q", got)
	}
}

// TestParseProposal_TrailingJunk reproduces the 2026-07-15 gemma-neutral drops: a
// valid proposal object followed by trailing content the model appended — an HTML
// </body> dump (whose embedded CSS/JS braces defeat extractJSON's last-brace trim)
// and an echoed prompt template. Unmarshal rejected these ("invalid character '<' /
// '0' after top-level value"); a json.Decoder reads the first value and ignores the
// rest, so the parse must now succeed.
func TestParseProposal_TrailingJunk(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"html-dump", `{"contract_id": "KXGDP-26JUL30-T0.5", "action": "SELL", "max_price": 0.88, "size": 500, "proposal": "cautious."} </body><style>.x{color:red}</style>`},
		{"echoed-template", `{"contract_id": "KXBRENTW-26JUL1717-T81.99", "action": "SELL", "max_price": 0.68, "size": 1000, "proposal": "selling."} 0.68 0.76 1000 2 days {template}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clean := extractJSON(c.raw)
			var got rawLLMProposalResponse
			if err := json.NewDecoder(strings.NewReader(clean)).Decode(&got); err != nil {
				t.Fatalf("expected first JSON object to parse despite trailing junk, got: %v\ncleaned: %s", err, clean)
			}
			if got.ContractID == "" || got.Action == "" {
				t.Errorf("parsed empty fields: %+v", got)
			}
		})
	}
}

func TestGenerateProposal_UnheldSellIsDecline(t *testing.T) {
	var gotPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotPrompt = req.Prompt
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"response": `{"contract_id": "KXU3-26OCT-T4.1", "action": "SELL", "max_price": 0.30, "size": 800, "proposal": "overpriced"}`,
		})
	}))
	defer server.Close()
	client := newTestProposalClient(server.URL, "qwen2.5:7b", "nomic-embed-text", "")

	mkt := &LiveMarket{Ticker: "KXU3-26OCT-T4.1", Title: "U-3 above 4.1%?", YesBid: 0.37, YesAsk: 0.38}
	if _, err := client.GenerateProposal(context.Background(), "jobs news", nil, mkt); !errors.Is(err, ErrLLMDeclined) {
		t.Errorf("unheld SELL: err = %v, want ErrLLMDeclined", err)
	}
	if !strings.Contains(gotPrompt, "DECLINE") {
		t.Error("unheld prompt should offer DECLINE instead of SELL")
	}

	mkt.Held = true
	if p, err := client.GenerateProposal(context.Background(), "jobs news", nil, mkt); err != nil || p.Action != "SELL" {
		t.Errorf("held SELL is an exit and must pass: %v, %v", p, err)
	}
	if strings.Contains(gotPrompt, "cannot short it") {
		t.Error("held prompt must not forbid SELL")
	}
}

func TestTruncateForEmbed(t *testing.T) {
	if got := truncateForEmbed("short"); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
	long := strings.Repeat("é", maxEmbedRunes+500)
	if n := len([]rune(truncateForEmbed(long))); n != maxEmbedRunes {
		t.Fatalf("got %d runes, want %d", n, maxEmbedRunes)
	}
}
