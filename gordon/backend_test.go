package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─── Test helpers shared across the backend/rag/gemini suites ─────────────────

// testOllamaBackend builds an ollamaBackend with no limiter/priority (both are
// nil-safe) and generous timeouts, pointed at a test server.
func testOllamaBackend(baseURL, model string) *ollamaBackend {
	return &ollamaBackend{
		baseURL:         strings.TrimSuffix(baseURL, "/"),
		model:           model,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		proposalTimeout: 10 * time.Second,
		acquireTimeout:  5 * time.Second,
	}
}

// testGeminiBackend builds a geminiBackend with an effectively-unlimited rate
// limiter, pointed at a test server.
func testGeminiBackend(baseURL, key, model string) *geminiBackend {
	return &geminiBackend{
		key:             key,
		baseURL:         baseURL,
		model:           model,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		proposalTimeout: 10 * time.Second,
		limiter:         NewGeminiRateLimiter(60000, 100000),
	}
}

// newTestProposalClient builds a ProposalClient whose primary is a local Ollama
// backend (and fallback, when fallbackModel differs), mirroring how main wires an
// Ollama persona. Embeddings hit baseURL too.
func newTestProposalClient(baseURL, model, embedModel, fallbackModel string) *ProposalClient {
	pc := NewProposalClient(baseURL, embedModel)
	pc.primary = testOllamaBackend(baseURL, model)
	if fallbackModel != "" && fallbackModel != model {
		pc.fallback = testOllamaBackend(baseURL, fallbackModel)
	}
	return pc
}

// ─── Backend tests ───────────────────────────────────────────────────────────

func TestOllamaBackend_Generate(t *testing.T) {
	var gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("expected path /api/generate, got %s", r.URL.Path)
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"response": "hi"})
	}))
	defer server.Close()

	b := testOllamaBackend(server.URL, "qwen2.5:7b")
	out, err := b.Generate(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hi" {
		t.Errorf("unexpected output: %q", out)
	}
	if gotModel != "qwen2.5:7b" {
		t.Errorf("model bound at construction not sent, got %q", gotModel)
	}
	if b.Name() != ProviderOllama {
		t.Errorf("Name() = %q, want %q", b.Name(), ProviderOllama)
	}
}

func TestGeminiBackend_StatsAndName(t *testing.T) {
	b := testGeminiBackend("http://unused", "k", "gemini-2.5-flash-lite")
	if b.Name() != ProviderGemini {
		t.Errorf("Name() = %q, want %q", b.Name(), ProviderGemini)
	}
	if m, _ := b.Stats()["model"].(string); m != "gemini-2.5-flash-lite" {
		t.Errorf("Stats()[model] = %q, want the bound model", m)
	}
}

// A paused persona (weight 0) skips the model on ANY backend cross-cutting, so
// GenerateProposal returns an error (⇒ rules fallback) without hitting the server.
func TestGenerateProposal_PausedSkipsModel(t *testing.T) {
	var hit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"response": `{"contract_id":"X"}`})
	}))
	defer server.Close()

	client := newTestProposalClient(server.URL, "qwen2.5:7b", "nomic-embed-text", "")
	client.priority = NewPriorityControls(nil, "p", 1, 1)
	client.priority.weight = 0 // pause

	if _, err := client.GenerateProposal(context.Background(), "event", nil, nil); err == nil {
		t.Fatal("expected error when persona is paused")
	}
	if hit {
		t.Error("paused persona should not call the model backend")
	}
}
