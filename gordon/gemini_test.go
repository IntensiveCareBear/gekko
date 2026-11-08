package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// geminiResponse builds a minimal generateContent response wrapping text.
func geminiResponse(text string) map[string]interface{} {
	return map[string]interface{}{
		"candidates": []map[string]interface{}{
			{"content": map[string]interface{}{
				"parts": []map[string]string{{"text": text}},
			}},
		},
	}
}

func TestGeminiBackend_Generate(t *testing.T) {
	var gotPath, gotKey string
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse(`{"contract_id": "X"}`))
	}))
	defer server.Close()

	b := testGeminiBackend(server.URL, "test-key", "gemini-2.5-flash-lite")
	out, err := b.Generate(context.Background(), "hello prompt")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if out != `{"contract_id": "X"}` {
		t.Errorf("Unexpected response text: %q", out)
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash-lite:generateContent" {
		t.Errorf("Unexpected path: %s", gotPath)
	}
	if gotKey != "test-key" {
		t.Errorf("API key not sent in x-goog-api-key header, got %q", gotKey)
	}
	contents, _ := json.Marshal(gotBody["contents"])
	if !strings.Contains(string(contents), "hello prompt") {
		t.Errorf("prompt missing from request contents: %s", contents)
	}
	genCfg, _ := gotBody["generationConfig"].(map[string]interface{})
	if genCfg["responseMimeType"] != "application/json" {
		t.Errorf("expected responseMimeType application/json, got %v", genCfg)
	}
}

// A 503 "high demand" is a server-side transient: Generate should retry and
// recover the hosted answer instead of surfacing the error (which would force
// the persona onto the Ollama fallback). This is the overnight-run failure mode.
func TestGeminiBackend_Generate_RetriesOn503(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			http.Error(w, `{"error":{"code":503,"status":"UNAVAILABLE"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse(`{"contract_id": "OK"}`))
	}))
	defer server.Close()

	b := testGeminiBackend(server.URL, "test-key", "gemini-2.5-flash-lite")
	out, err := b.Generate(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("expected retry to recover, got error: %v", err)
	}
	if out != `{"contract_id": "OK"}` {
		t.Errorf("unexpected response after retry: %q", out)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (two 503s then success), got %d", attempts)
	}
}

// A 429 (rate limit / quota) is NOT retried — it won't heal and the caller should
// fall back immediately. Verifies we hit the server exactly once.
func TestGeminiBackend_Generate_NoRetryOn429(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, "quota exceeded", http.StatusTooManyRequests)
	}))
	defer server.Close()

	b := testGeminiBackend(server.URL, "test-key", "gemini-2.5-flash-lite")
	if _, err := b.Generate(context.Background(), "prompt"); err == nil {
		t.Fatal("expected error on 429")
	}
	if attempts != 1 {
		t.Errorf("429 should not be retried, got %d attempts", attempts)
	}
}

func TestGeminiBackend_Generate_NoKey(t *testing.T) {
	b := testGeminiBackend("http://unused", "", "gemini-2.5-flash-lite")
	if _, err := b.Generate(context.Background(), "x"); err == nil {
		t.Fatal("expected error when no API key configured")
	}
}

func TestGenerateProposal_GeminiProvider(t *testing.T) {
	var gotPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Contents) > 0 && len(req.Contents[0].Parts) > 0 {
			gotPrompt = req.Contents[0].Parts[0].Text
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse(
			`{"contract_id": "KXCPI-26JUL", "action": "BUY", "max_price": 0.62, "size": 800, "proposal": "edge"}`))
	}))
	defer server.Close()

	client := NewProposalClient("http://unused", "nomic-embed-text")
	client.primary = testGeminiBackend(server.URL, "test-key", "gemini-2.5-flash-lite")

	mkt := &LiveMarket{Ticker: "KXCPI-26JUL", Title: "Will CPI exceed 3% in July?", YesBid: 0.40, YesAsk: 0.45}
	res, err := client.GenerateProposal(context.Background(), "CPI news", nil, mkt)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if res.ContractID != "KXCPI-26JUL" || res.Action != "BUY" || res.MaxPrice != 0.62 || res.Size != 800 {
		t.Errorf("Unexpected proposal result: %+v", res)
	}
	for _, want := range []string{"Will CPI exceed 3% in July?", "KXCPI-26JUL", "$0.40 / $0.45"} {
		if !strings.Contains(gotPrompt, want) {
			t.Errorf("prompt missing live-market context %q", want)
		}
	}
}

func TestGenerateProposal_GeminiFallsBackToOllama(t *testing.T) {
	gemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "quota exceeded", http.StatusTooManyRequests)
	}))
	defer gemini.Close()

	var ollamaModel string
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		ollamaModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"response": `{"contract_id": "FALLBACK-1", "action": "SELL", "max_price": 0.30, "size": 500, "proposal": "local fallback"}`,
		})
	}))
	defer ollama.Close()

	client := NewProposalClient(ollama.URL, "nomic-embed-text")
	client.primary = testGeminiBackend(gemini.URL, "test-key", "gemini-2.5-flash-lite")
	client.fallback = testOllamaBackend(ollama.URL, "qwen2.5:1.5b")

	res, err := client.GenerateProposal(context.Background(), "test event", nil, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if res.ContractID != "FALLBACK-1" {
		t.Errorf("Unexpected proposal result: %+v", res)
	}
	if ollamaModel != "qwen2.5:1.5b" {
		t.Errorf("fallback should hit the local Ollama fallback model, got %q", ollamaModel)
	}
}

func TestGeminiRateLimiter_DailyBudget(t *testing.T) {
	rl := NewGeminiRateLimiter(60000, 2) // effectively no RPM gap
	ctx := context.Background()
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if err := rl.Wait(ctx); err == nil {
		t.Fatal("third call should exhaust the daily budget")
	}
}

func TestGeminiRateLimiter_Spacing(t *testing.T) {
	rl := NewGeminiRateLimiter(600, 100) // 100ms gap
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := rl.Wait(ctx); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("three calls at 100ms spacing finished in %s — pacing not enforced", elapsed)
	}
}

func TestGeminiRateLimiter_DeadlineExceeded(t *testing.T) {
	rl := NewGeminiRateLimiter(1, 100) // 60s gap
	ctx := context.Background()
	if err := rl.Wait(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	shortCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := rl.Wait(shortCtx); err == nil {
		t.Fatal("second call should fail: pacing wait exceeds deadline")
	}
	if time.Since(start) > time.Second {
		t.Error("deadline-exceeding wait should fail fast, not block")
	}
}

func TestReadSecret(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(file, []byte("  file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readSecret("inline-key", file); got != "inline-key" {
		t.Errorf("inline should win, got %q", got)
	}
	if got := readSecret("", file); got != "file-key" {
		t.Errorf("file contents should be trimmed, got %q", got)
	}
	if got := readSecret("", filepath.Join(dir, "missing.txt")); got != "" {
		t.Errorf("missing file should yield empty, got %q", got)
	}
	if got := readSecret("", ""); got != "" {
		t.Errorf("no sources should yield empty, got %q", got)
	}
}

// Needs a reachable Redis (REDIS_TEST_ADDR); skipped otherwise.
func TestRateLimiterRedisBudgetSharedAcrossInstances(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR unset")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	prefix := "gekko:test_budget:" + t.Name()
	day := time.Now().UTC().Format("2006-01-02")
	rdb.Del(context.Background(), prefix+":"+day)
	defer rdb.Del(context.Background(), prefix+":"+day)
	a := NewGeminiRateLimiter(60000, 2).WithRedis(rdb, prefix)
	b := NewGeminiRateLimiter(60000, 2).WithRedis(rdb, prefix) // "restarted"/other persona
	if a.Wait(context.Background()) != nil || b.Wait(context.Background()) != nil {
		t.Fatal("first two calls should pass")
	}
	if b.Wait(context.Background()) == nil || a.Wait(context.Background()) == nil {
		t.Fatal("third call must be denied fleet-wide")
	}
	if used, _, _ := NewGeminiRateLimiter(60000, 2).WithRedis(rdb, prefix).Snapshot(); used != 2 {
		t.Fatalf("snapshot after restart = %d, want 2", used)
	}
}
