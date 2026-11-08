package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GEKKO_PRIMER is the system context that teaches the assistant about the platform.
// It is informational only — the guardrail line below forbids any write actions.
const gekkoPrimer = `You are "Ask Gekko", a helpful assistant embedded in the Gekko quantitative
trading dashboard. Answer the user's questions about their portfolio and about how Gekko works.
Be concise, accurate, and explain finance terms simply (assume basic knowledge).

GEKKO ARCHITECTURE:
- Kahuna ingests public market/news streams (Kalshi, Polymarket, Reuters, Twitter) and publishes
  validated events to Kafka after a compliance gatekeeper filters them.
- Gordon (the strategy engine) consumes those events, fetches order books from Redis, runs a
  RAG lookup over a Qdrant vector DB plus a local Ollama LLM, evaluates liquidity/slippage and
  risk limits, and emits trade proposals to Kafka.
- Executive-core manages positions and the immutable (hash-chained) ledger, and either simulates
  fills (DRY mode) or routes live orders to the broker-worker over a Unix socket.
- Broker-worker executes orders on Kalshi (RSA-PSS signed REST) or Polymarket (EIP-712 CLOB).
- Liveview serves the dashboard's API/SSE; the React frontend has Strategy Stream, Portfolio
  Analytics, System, and Assistant tabs.

KEY MODES:
- DRY (simulator) vs WET (production) execution.
- SUPERVISED (manual approve/deny each proposal) vs UNSUPERVISED (auto-approve).

KEY TERMS: NAV (net asset value), drawdown (peak-to-trough % drop), profit factor (gross profit /
gross loss), win rate, Sharpe ratio, slippage, iceberg orders (orders sliced to hide size).

GUARDRAIL: You are informational only. You must NOT place, modify, or cancel trades, change risk
limits, or toggle controls. If asked to do any of those, politely decline and point the user to
the relevant tab (Strategy Stream for approvals, System tab for controls).`

// chatMessage is one turn in the conversation.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the body posted by the frontend.
type chatRequest struct {
	Messages         []chatMessage `json:"messages"`
	PortfolioContext interface{}   `json:"portfolio_context"`
}

// ollamaChatRequest matches Ollama's /api/chat schema.
type ollamaChatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ollamaChatResponse struct {
	Message chatMessage `json:"message"`
}

const maxHistory = 10

func handleChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
		return
	}
	if len(req.Messages) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no messages provided"})
		return
	}

	// Build the system message: primer + a JSON snapshot of what the user sees.
	systemContent := gekkoPrimer
	if req.PortfolioContext != nil {
		if ctxJSON, err := json.Marshal(req.PortfolioContext); err == nil && string(ctxJSON) != "null" {
			systemContent += "\n\nCurrent portfolio snapshot (JSON):\n" + string(ctxJSON)
		}
	}

	// Cap conversation history to keep the prompt small.
	history := req.Messages
	if len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
	}

	messages := append([]chatMessage{{Role: "system", Content: systemContent}}, history...)

	model := getEnv("OLLAMA_CHAT_MODEL", "qwen2.5:1.5b")
	ollamaURL := strings.TrimSuffix(getEnv("OLLAMA_URL", "http://localhost:11434"), "/")

	reply, err := callOllamaChat(ollamaURL, model, messages)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("assistant unavailable: %v", err),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]string{
		"reply": reply,
		"model": model,
	})
}

// callOllamaChat issues a non-streaming /api/chat request and returns the cleaned reply.
func callOllamaChat(baseURL, model string, messages []chatMessage) (string, error) {
	body, err := json.Marshal(ollamaChatRequest{
		Model:    model,
		Messages: messages,
		Stream:   false,
	})
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(baseURL+"/api/chat", "application/json", bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("cannot reach Ollama at %s (is it running?)", baseURL)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var oresp ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&oresp); err != nil {
		return "", fmt.Errorf("invalid response from ollama: %w", err)
	}

	reply := stripThink(oresp.Message.Content)
	if reply == "" {
		return "", fmt.Errorf("empty reply from model %q (is it pulled? try: ollama pull %s)", model, model)
	}
	return reply, nil
}

// stripThink removes a leading <think>...</think> reasoning block (emitted by models
// like deepseek-r1) so the user only sees the final answer.
func stripThink(s string) string {
	if start := strings.Index(s, "<think>"); start != -1 {
		if end := strings.Index(s, "</think>"); end != -1 && end > start {
			s = s[:start] + s[end+len("</think>"):]
		}
	}
	return strings.TrimSpace(s)
}
