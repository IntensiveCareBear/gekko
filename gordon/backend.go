package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ProposalBackend turns a prompt into raw model text AND owns whatever rate /
// concurrency limiting that model source requires. GenerateProposal calls
// Generate without ever learning which backend it is — each backend self-paces,
// so "configurable backend" gives "configurable rate limiting" for free.
//
// The key property: rate limiting lives INSIDE the backend, not on the shared
// call path. A local Ollama backend paces itself against local-GPU capacity
// (fleet slot + priority throttle); a hosted backend paces itself to the API's
// real RPM/RPD limits. Adding a third backend is one new Generate implementation
// with whatever limiter it needs.
type ProposalBackend interface {
	// Generate returns the model's raw text for prompt (extractJSON handles the
	// rest). The model is bound at construction. Implementations pace themselves.
	Generate(ctx context.Context, prompt string) (string, error)
	// Stats returns backend-scoped telemetry for the health endpoint (model,
	// and for hosted backends the daily budget / RPM pacing). Keys are bare —
	// health.go prefixes them with "backend_".
	Stats() map[string]interface{}
	// Name is the backend identifier ("ollama" | "gemini" | ...).
	Name() string
}

// ─── Ollama backend ──────────────────────────────────────────────────────────

// ollamaBackend generates against a local Ollama /api/generate. It owns the
// fleet-wide concurrency slot (genLimiter) and the manual rate throttle
// (priority) because both are sized to the local GPU's capacity — a hosted
// backend has no reason to obey either. It is used as the primary backend for
// Ollama personas and ALWAYS as the fallback, so a hosted outage degrades to a
// real (small) local model rather than keyword rules.
type ollamaBackend struct {
	baseURL         string
	model           string
	httpClient      *http.Client
	proposalTimeout time.Duration     // deadline for ONE model call (per attempt)
	acquireTimeout  time.Duration     // max wait for a slot / rate slot before giving up
	genLimiter      *LLMLimiter       // caps concurrent generate calls; nil ⇒ unbounded
	priority        *PriorityControls // manual per-gordon rate throttle; nil ⇒ no throttle
}

func (b *ollamaBackend) Name() string { return ProviderOllama }

func (b *ollamaBackend) Stats() map[string]interface{} {
	return map[string]interface{}{"model": b.model}
}

// Generate paces this call against local-Ollama capacity, then runs one model
// attempt under proposalTimeout. Pacing has two stages:
//
//   - priority.Wait spaces this gordon's calls per the manual promote/demote
//     knob (a demoted gordon waits longer; the pause case is handled cross-cutting
//     upstream in GenerateProposal). Bounded by acquireTimeout so a long spacing
//     never eats into the generate deadline.
//   - genLimiter.Acquire takes a fleet-wide concurrency slot BEFORE the call, so a
//     multi-persona fleet turns Ollama-side queueing into a short, deadline-
//     respecting wait (or a clean fallback) instead of a timeout.
func (b *ollamaBackend) Generate(ctx context.Context, prompt string) (string, error) {
	if err := b.priority.Wait(ctx, b.acquireTimeout); err != nil {
		return "", fmt.Errorf("llm generation skipped (priority): %w", err)
	}
	release, err := b.genLimiter.Acquire(ctx, b.acquireTimeout)
	if err != nil {
		return "", fmt.Errorf("llm generation skipped (no slot): %w", err)
	}
	defer release()

	callCtx, cancel := context.WithTimeout(ctx, b.proposalTimeout)
	defer cancel()
	return b.query(callCtx, prompt)
}

func (b *ollamaBackend) query(ctx context.Context, prompt string) (string, error) {
	reqBody, err := json.Marshal(map[string]interface{}{
		"model":  b.model,
		"prompt": prompt,
		"stream": false,
		"format": "json", // Instruct Ollama to return structured JSON
	})
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/api/generate", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var respJSON struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respJSON); err != nil {
		return "", err
	}

	return respJSON.Response, nil
}

// ─── Gemini backend ──────────────────────────────────────────────────────────

// geminiBackend generates against the Gemini generateContent API. It owns ONLY
// the GeminiRateLimiter (RPM/RPD) — no fleet slot, no Ollama pacer — so it
// self-paces to the hosted API's real limits instead of a local-GPU-shaped
// budget it has no reason to obey.
type geminiBackend struct {
	key             string
	baseURL         string
	model           string
	httpClient      *http.Client
	proposalTimeout time.Duration
	limiter         *GeminiRateLimiter
}

func (b *geminiBackend) Name() string { return ProviderGemini }

// Stats reports today's usage against the daily budget and the RPM pacing, for
// the health endpoint's External-APIs card. Keys are prefixed with "backend_"
// by health.go.
func (b *geminiBackend) Stats() map[string]interface{} {
	used, maxPerDay, rpm := b.limiter.Snapshot()
	return map[string]interface{}{
		"model":       b.model,
		"calls_today": used,
		"max_rpd":     maxPerDay,
		"max_rpm":     rpm,
	}
}

// retryableGeminiError marks a Gemini failure worth retrying: a server-side
// transient (5xx) or a network error, both of which usually clear on their own
// within a second or two. Rate limits (429) and other 4xx are deliberately NOT
// wrapped — the limiter already paces us and a quota/auth error won't heal on
// retry, so those fall straight through to the Ollama fallback.
type retryableGeminiError struct{ err error }

func (e *retryableGeminiError) Error() string { return e.err.Error() }
func (e *retryableGeminiError) Unwrap() error  { return e.err }

func isRetryableGemini(err error) bool {
	var r *retryableGeminiError
	return errors.As(err, &r)
}

// Generate runs generateContent under proposalTimeout, retrying server-side
// transient failures with capped exponential backoff before giving up. Gemini's
// "high demand" 503s are common (the overnight run logged ~600) and usually
// clear within a second or two, so a couple of quick retries recover the hosted
// answer instead of degrading the persona to the local fallback model. The whole
// retry sequence shares the single proposalTimeout budget, and each attempt is
// still RPM/RPD-paced inside query. Non-retryable errors (4xx, rate limits) break
// out immediately so rag.go's fallback path is reached without delay.
func (b *geminiBackend) Generate(ctx context.Context, prompt string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, b.proposalTimeout)
	defer cancel()

	const maxAttempts = 3
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := b.query(callCtx, prompt)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !isRetryableGemini(err) || attempt == maxAttempts {
			break
		}
		log.Printf("[GEMINI] transient failure (attempt %d/%d): %v — retrying in %s", attempt, maxAttempts, err, backoff)
		select {
		case <-callCtx.Done():
			return "", lastErr
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return "", lastErr
}

// query runs one generateContent call against the Gemini API. Same contract as
// the Ollama backend: prompt in, raw model text out. The key travels in a header
// — never the URL — so it can't leak into logs or error strings.
func (b *geminiBackend) query(ctx context.Context, prompt string) (string, error) {
	if b.key == "" {
		return "", fmt.Errorf("gemini: no API key configured")
	}
	if err := b.limiter.Wait(ctx); err != nil {
		return "", fmt.Errorf("gemini: %w", err)
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"contents": []map[string]interface{}{
			{"parts": []map[string]string{{"text": prompt}}},
		},
		"generationConfig": map[string]interface{}{
			"responseMimeType": "application/json",
		},
	})
	if err != nil {
		return "", err
	}

	base := b.baseURL
	if base == "" {
		base = defaultGeminiBaseURL
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimSuffix(base, "/"), b.model)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", b.key)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		// Network / transport failure (DNS, connection reset, timeout mid-flight):
		// transient, so mark it retryable.
		return "", &retryableGeminiError{err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		statusErr := fmt.Errorf("gemini returned status %d: %s", resp.StatusCode, string(bodyBytes))
		// 5xx (503 "high demand", 500, 502, 504) is a server-side transient — retry.
		// 4xx (429 quota, 400 bad request, 403 auth) won't heal on retry — fall through.
		if resp.StatusCode >= 500 {
			return "", &retryableGeminiError{statusErr}
		}
		return "", statusErr
	}

	var respJSON struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respJSON); err != nil {
		return "", err
	}
	if len(respJSON.Candidates) == 0 {
		return "", fmt.Errorf("gemini returned no candidates")
	}
	var text strings.Builder
	for _, p := range respJSON.Candidates[0].Content.Parts {
		text.WriteString(p.Text)
	}
	if text.Len() == 0 {
		return "", fmt.Errorf("gemini returned empty candidate text")
	}
	return text.String(), nil
}
