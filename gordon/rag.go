package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrLLMDeclined is returned by GenerateProposal when the model's action is neither
// BUY nor SELL — i.e. it examined the event and chose not to trade (HOLD/NO/PASS or
// garbage). The caller drops the event rather than manufacturing a trade: the old
// behavior silently coerced any non-BUY/SELL action to BUY, which turned a bear's
// "no" into a long (2026-07-10 forensics). Distinct from a generation FAILURE, which
// still degrades to the rules fallback.
var ErrLLMDeclined = errors.New("llm declined to trade (action not BUY/SELL)")

// ProposalClient owns the RAG proposal pipeline: local Ollama embeddings (always)
// plus a pluggable primary/fallback ProposalBackend for turning a prompt into a
// trading proposal. It is backend-agnostic — each backend carries its own rate /
// concurrency limiting (see backend.go), so GenerateProposal never branches on
// "which provider."
type ProposalClient struct {
	// Embedding path — ALWAYS local Ollama (nomic-embed-text). The Qdrant
	// collections are built in that vector space, so embeddings never move
	// regardless of which backend generates proposals.
	baseURL    string
	embedModel string
	httpClient *http.Client

	// personaNudge is the persona's editable personality instruction, appended to
	// the proposal system prompt. Guarded because GordonConfig's poller can update it
	// live (from the Gordons tab) while GenerateProposal reads it. Use
	// SetPersonaNudge/PersonaNudge, never the field directly.
	nudgeMu      sync.RWMutex
	personaNudge string

	// Proposal backends. primary is the persona's chosen backend; fallback is
	// ALWAYS an Ollama backend (or nil when the primary already IS that model), so
	// a hosted outage or rate-cap degrades to a real small local model rather than
	// keyword rules. All rate/concurrency limiting lives inside these backends.
	primary  ProposalBackend
	fallback ProposalBackend

	// priority is the manual promote/demote/pause knob. Its RATE throttle is
	// applied inside the Ollama backend (it's sized to local-GPU capacity); here it
	// is consulted only for the cross-cutting PAUSE check (weight 0), which skips
	// the model on ANY backend and falls back to rules. Nil-safe.
	priority *PriorityControls

	// Primary-backend telemetry (generic across backends). Atomic so the health
	// endpoint reads them without contending with the generate path.
	primaryOK  atomic.Int64 // primary backend call succeeded
	primaryErr atomic.Int64 // primary backend call failed (rate cap / API error / timeout)
	fallbacks  atomic.Int64 // fell back to the local Ollama model after a primary failure
	lastOKUnix atomic.Int64 // unix seconds of the last successful primary call (0 = never)
}

// NewProposalClient creates a client for local Ollama embeddings. The proposal
// backends (primary/fallback) and the priority knob are wired in by main after
// the limiter and rate controls exist. The embed HTTP transport gets a generous
// ceiling; embedding calls carry their own short context deadlines.
func NewProposalClient(baseURL, embedModel string) *ProposalClient {
	return &ProposalClient{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		embedModel: embedModel,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// SetPersonaNudge installs the persona's personality instruction. Safe to call
// concurrently with GenerateProposal (the config poller uses it for live edits).
func (oc *ProposalClient) SetPersonaNudge(nudge string) {
	oc.nudgeMu.Lock()
	oc.personaNudge = nudge
	oc.nudgeMu.Unlock()
}

// PersonaNudge returns the current personality instruction under the read lock.
func (oc *ProposalClient) PersonaNudge() string {
	oc.nudgeMu.RLock()
	defer oc.nudgeMu.RUnlock()
	return oc.personaNudge
}

// Paused reports whether this persona is muted (priority weight 0). A paused
// persona should skip event processing entirely rather than fall back to rules.
// Nil-safe: no priority controls ⇒ never paused.
func (oc *ProposalClient) Paused() bool {
	if oc == nil || oc.priority == nil {
		return false
	}
	return oc.priority.Paused()
}

// ModelUsed returns the primary backend's model for telemetry (proposal.model_used).
func (oc *ProposalClient) ModelUsed() string {
	if oc == nil || oc.primary == nil {
		return ""
	}
	if m, ok := oc.primary.Stats()["model"].(string); ok {
		return m
	}
	return ""
}

// PrimaryCounters returns the generic backend telemetry for the health endpoint.
func (oc *ProposalClient) PrimaryCounters() (ok, primaryErr, fallbacks, lastOK int64) {
	return oc.primaryOK.Load(), oc.primaryErr.Load(), oc.fallbacks.Load(), oc.lastOKUnix.Load()
}

// maxEmbedRunes caps the text handed to the embedder. nomic-embed-text has a
// 2048-token context; a long news item (full article body, pasted tables, URLs)
// overflows it and Ollama returns 500 "input length exceeds the context length",
// which used to knock the whole proposal into the rules fallback. ~2000 runes sits
// under 2048 tokens only for prose — 6000 still 500'd on number/URL-heavy items
// (1 token per ~1-2 chars), so 2000 is the cap; the leading
// prefix of a headline+lede carries the semantic signal we match on anyway.
const maxEmbedRunes = 2000

// truncateForEmbed trims text to maxEmbedRunes runes (not bytes, so multibyte
// characters are never split). Pure.
func truncateForEmbed(text string) string {
	r := []rune(text)
	if len(r) <= maxEmbedRunes {
		return text
	}
	return string(r[:maxEmbedRunes])
}

// GetEmbedding fetches the float32 array vector from Ollama /api/embeddings.
func (oc *ProposalClient) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	reqBody, err := json.Marshal(map[string]interface{}{
		"model":  oc.embedModel,
		"prompt": truncateForEmbed(text),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/embeddings", oc.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := oc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var respJSON struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respJSON); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(respJSON.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding returned")
	}

	return respJSON.Embedding, nil
}

// LLMProposalResponse is the expected JSON response from the Ollama LLM.
type LLMProposalResponse struct {
	ContractID string  `json:"contract_id"`
	Action     string  `json:"action"`
	MaxPrice   float64 `json:"max_price"`
	Size       int     `json:"size"`
	Proposal   string  `json:"proposal"`
}

type rawLLMProposalResponse struct {
	ContractID string          `json:"contract_id"`
	Action     string          `json:"action"`
	MaxPrice   json.RawMessage `json:"max_price"`
	Size       json.RawMessage `json:"size"`
	Proposal   string          `json:"proposal"`
}

// LiveMarket carries the matched real market's title and book so the LLM can
// estimate probability against the actual contract and its current price —
// instead of proposing blind and having the price overridden downstream.
type LiveMarket struct {
	Ticker string
	Title  string
	YesBid float64
	YesAsk float64
	// Microstructure (clodds-mining ADOPT #6): a compact quant feature block so the
	// model reasons over spread/imbalance/momentum/expiry, not just title + price — a
	// model told the spread is 8¢ and expiry is 3h is less likely to claim 15¢ of edge.
	// All optional; a zero/neutral field is omitted from the rendered block.
	TrendPct   float64 // short-horizon price trend %, signed
	VolSurge   float64 // top-of-book size ÷ window average (1 = normal)
	Imbalance  float64 // order-book depth imbalance [-1,1] (+ bid-heavy, − ask-heavy)
	HoursToExp float64 // hours until resolution (0 = unknown)
	// Held: Gordon owns this contract, so SELL is a real exit. Unheld, the model may
	// only BUY or decline — the system is YES-only, so an unheld SELL never trades.
	Held bool
}

// microstructureLines renders the quant-feature block for the proposal prompt, one
// short line per available feature (clodds-mining ADOPT #6). Kept terse — long prompts
// cost latency on local Ollama — and each field is omitted when zero/neutral. Pure.
func (m *LiveMarket) microstructureLines() []string {
	var out []string
	if m.YesBid > 0 && m.YesAsk > m.YesBid {
		out = append(out, fmt.Sprintf("spread %.0f¢ (bid %.0f / ask %.0f)", (m.YesAsk-m.YesBid)*100, m.YesBid*100, m.YesAsk*100))
	}
	if m.Imbalance != 0 {
		lean := "bid-heavy (buy pressure)"
		if m.Imbalance < 0 {
			lean = "ask-heavy (sell pressure)"
		}
		out = append(out, fmt.Sprintf("book imbalance %+.2f, %s", m.Imbalance, lean))
	}
	if m.TrendPct != 0 {
		out = append(out, fmt.Sprintf("recent momentum %+.1f%%", m.TrendPct))
	}
	if m.VolSurge >= 1.5 {
		out = append(out, fmt.Sprintf("volume ×%.1f vs recent average", m.VolSurge))
	}
	if m.HoursToExp > 0 {
		if m.HoursToExp < 48 {
			out = append(out, fmt.Sprintf("resolves in ~%.0fh", m.HoursToExp))
		} else {
			out = append(out, fmt.Sprintf("resolves in ~%.0f days", m.HoursToExp/24))
		}
	}
	return out
}

// GenerateProposal queries Ollama with historical RAG context to formulate a trading
// proposal. When market is non-nil the prompt includes the live market's title and
// bid/ask, and max_price is the model's probability estimate for THAT market.
func (oc *ProposalClient) GenerateProposal(ctx context.Context, currentContent string, matches []QdrantMatch, market *LiveMarket) (*LLMProposalResponse, error) {
	// 1. Build prompt context from historical matches
	var contextBuilder strings.Builder
	contextBuilder.WriteString(`You are a prediction market trading analyst. Your job is to estimate probabilities and find edge against the current market price.

Prediction market rules:
- Contracts pay $1 if YES, $0 if NO at resolution
- Price = implied probability (0.65 = market thinks 65% chance YES)
- Edge exists when your probability estimate differs meaningfully from the market price
- BUY = you think YES probability is HIGHER than current ask price
- SELL = you think YES probability is LOWER than current bid price
- Only trade when you have a genuine edge (>5% probability difference from market price)

`)

	contextBuilder.WriteString("HISTORICAL OUTCOMES (similar past events and how they resolved):\n\n")
	if len(matches) == 0 {
		contextBuilder.WriteString("No historical precedents found. Base your estimate on fundamentals alone.\n\n")
	} else {
		for i, match := range matches {
			contextBuilder.WriteString(fmt.Sprintf("[%d] Similarity: %.0f%%\n", i+1, match.Score*100))
			contextBuilder.WriteString(fmt.Sprintf("    Event: %s\n", match.Payload.Text))
			contextBuilder.WriteString(fmt.Sprintf("    Trade: %s %s at $%.2f (size %d)\n", match.Payload.Action, match.Payload.ContractID, match.Payload.MaxPrice, match.Payload.Size))
			if match.Payload.Proposal != "" {
				contextBuilder.WriteString(fmt.Sprintf("    Outcome: %s\n", match.Payload.Proposal))
			}
			contextBuilder.WriteString("\n")
		}
	}

	// Lou phase 2: lessons distilled from this bot's own past runs. Injected
	// between the historical matches and the current event so the model weighs
	// them as priors, not as the event itself. Empty book = section omitted.
	lessonTicker := ""
	if market != nil {
		lessonTicker = market.Ticker
	}
	if lines := lessonBook.PromptLines(lessonTicker, 4); len(lines) > 0 {
		contextBuilder.WriteString("LESSONS FROM THIS BOT'S PAST TRADING (evidence-backed — treat as strong priors):\n")
		for _, line := range lines {
			contextBuilder.WriteString("- " + line + "\n")
		}
		contextBuilder.WriteString("\n")
	}

	contextBuilder.WriteString("CURRENT EVENT:\n")
	contextBuilder.WriteString(currentContent + "\n\n")
	if market != nil {
		contextBuilder.WriteString("LIVE MARKET (your proposal will trade THIS real contract — estimate ITS probability):\n")
		contextBuilder.WriteString(fmt.Sprintf("    Title: %s\n", market.Title))
		contextBuilder.WriteString(fmt.Sprintf("    Ticker: %s\n", market.Ticker))
		if market.YesBid > 0 || market.YesAsk > 0 {
			contextBuilder.WriteString(fmt.Sprintf("    YES bid/ask: $%.2f / $%.2f (market-implied probability ≈ %.0f%%)\n",
				market.YesBid, market.YesAsk, (market.YesBid+market.YesAsk)/2*100))
		}
		contextBuilder.WriteString("Set max_price to YOUR OWN probability estimate that this market resolves YES — do NOT copy the market price. If your estimate is close to the market price, there is no edge and you should still report your honest estimate.\n")
		if !market.Held {
			contextBuilder.WriteString("You do NOT hold this contract and cannot short it: answer BUY only if YES is underpriced, otherwise set action to DECLINE.\n")
		}
		contextBuilder.WriteString("\n")
		if block := market.microstructureLines(); len(block) > 0 {
			contextBuilder.WriteString("MARKET MICROSTRUCTURE (quant features — weigh these; a wide spread or imminent expiry argues against a thin edge):\n")
			for _, line := range block {
				contextBuilder.WriteString("    - " + line + "\n")
			}
			contextBuilder.WriteString("\n")
		}
	}
	contextBuilder.WriteString(`ANALYSIS STEPS (work through these before deciding):
1. What is this event actually claiming or reporting?
2. What is the base rate for this type of outcome?
3. Does this event shift that probability up or down, and by how much?
4. What is a reasonable probability estimate (0.0 to 1.0)?
5. Is there enough edge vs the historical market price to justify a trade?
6. What size is appropriate given your confidence?

OUTPUT: Respond with a single JSON object. No other text outside the JSON.
`)
	contextBuilder.WriteString(`{
  "contract_id": "TICKER (e.g. FED-RATE-CUT-JUL, BTC-100K-2026, AAPL-UP-Q2)",
  "action": "BUY or SELL",
  "max_price": <your probability estimate as float 0.01-0.99>,
  "size": <integer 500-2000, scale with confidence>,
  "proposal": "<one sentence: what edge you see and why>"
}
`)
	if nudge := oc.PersonaNudge(); nudge != "" {
		contextBuilder.WriteString("\nTrading personality to adopt: " + nudge + "\n")
	}

	systemPrompt := contextBuilder.String()

	// 2. Cross-cutting PAUSE: a paused persona (weight 0) skips the model on ANY
	// backend and falls back to rules, leaving fleet throughput for the promoted
	// gordons. This is the only part of the priority knob that spans backends — the
	// rate spacing and the concurrency slot are sized to local-GPU capacity and so
	// live inside the Ollama backend; a hosted backend self-paces to its own limits.
	if oc.priority.Paused() {
		return nil, fmt.Errorf("llm generation skipped: %w", ErrPersonaPaused)
	}

	// 3. Generate. primary is the persona's backend; on failure we fall back to the
	// local Ollama fallback backend (when configured), so a hosted outage or rate-cap
	// degrades to a real (small) LLM answer instead of rules-fallback noise. Each
	// backend owns its own per-attempt deadline and rate limiting.
	proposal, err := oc.primary.Generate(ctx, systemPrompt)
	if err == nil {
		oc.primaryOK.Add(1)
		oc.lastOKUnix.Store(time.Now().Unix())
	} else if oc.fallback != nil {
		oc.primaryErr.Add(1)
		oc.fallbacks.Add(1)
		// Log the PRIMARY error before the fallback attempt overwrites it — otherwise
		// a hosted outage/rate-cap is invisible in the logs (only the fallback's
		// outcome survives), as a live Gemini 429 storm demonstrated.
		log.Printf("[BACKEND WARN] primary %s call failed: %v — falling back to local %s.",
			oc.primary.Name(), err, oc.fallback.Name())
		proposal, err = oc.fallback.Generate(ctx, systemPrompt)
	} else {
		oc.primaryErr.Add(1)
	}
	if err != nil {
		return nil, fmt.Errorf("llm generation failed: %w", err)
	}

	// 3. Parse JSON from output (robust parser for reasoning tags). Decode with a
	// json.Decoder rather than Unmarshal: it reads exactly the first JSON value and
	// ignores any trailing bytes, so a model that appends junk after the object —
	// an echoed prompt template or a stray </body> HTML dump, both of which carry
	// their own braces that defeat extractJSON's last-brace trim — still parses
	// instead of failing "... after top-level value" and dropping the proposal.
	cleanJSON := extractJSON(proposal)
	var raw rawLLMProposalResponse
	if err := json.NewDecoder(strings.NewReader(cleanJSON)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("unmarshal raw proposal JSON: %w (Raw: %s)", err, cleanJSON)
	}

	var maxPrice float64
	if len(raw.MaxPrice) > 0 {
		var f float64
		if err := json.Unmarshal(raw.MaxPrice, &f); err == nil {
			maxPrice = f
		} else {
			var s string
			if err := json.Unmarshal(raw.MaxPrice, &s); err == nil {
				if parsed, err := strconv.ParseFloat(s, 64); err == nil {
					maxPrice = parsed
				}
			}
		}
	}

	var size int
	if len(raw.Size) > 0 {
		var i int
		if err := json.Unmarshal(raw.Size, &i); err == nil {
			size = i
		} else {
			var s string
			if err := json.Unmarshal(raw.Size, &s); err == nil {
				if parsed, err := strconv.Atoi(s); err == nil {
					size = parsed
				}
			}
		}
	}

	response := LLMProposalResponse{
		ContractID: raw.ContractID,
		Action:     raw.Action,
		MaxPrice:   maxPrice,
		Size:       size,
		Proposal:   raw.Proposal,
	}

	// Validate fields
	if response.ContractID == "" {
		return nil, fmt.Errorf("empty contract_id in proposal")
	}
	response.Action = strings.ToUpper(response.Action)
	if response.Action != "BUY" && response.Action != "SELL" {
		return nil, fmt.Errorf("%w: %q", ErrLLMDeclined, response.Action)
	}
	// A SELL on an unheld live market can't become an order; it used to die at the
	// edge gate without a cooldown, so the same contract was re-asked all day (KXU3
	// 71×, 2026-10-02). Treat it as the decline it is.
	if market != nil && !market.Held && response.Action == "SELL" {
		return nil, fmt.Errorf("%w: SELL on unheld %s", ErrLLMDeclined, market.Ticker)
	}
	if response.MaxPrice <= 0 || response.MaxPrice >= 1.0 {
		response.MaxPrice = 0.65 // fallback default
	}
	if response.Size <= 0 {
		response.Size = 1000 // default size
	}

	return &response, nil
}

// QdrantClient interfaces with the Qdrant REST API.
type QdrantClient struct {
	baseURL    string
	collection string
	httpClient *http.Client
}

// NewQdrantClient creates a new QdrantClient.
func NewQdrantClient(baseURL, collection string) *QdrantClient {
	return &QdrantClient{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		collection: collection,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// QdrantPayload represents the payload stored in each Qdrant point.
type QdrantPayload struct {
	Text       string  `json:"text"`
	ContractID string  `json:"contract_id"`
	Action     string  `json:"action"`
	MaxPrice   float64 `json:"max_price"`
	Size       int     `json:"size"`
	Proposal   string  `json:"proposal"`
	// Realized-outcome fields, populated only for points upserted from real trade
	// feedback (see RunRAGFeedbackConsumer). omitempty so existing seed documents and
	// proposal-time upserts — which don't know an outcome yet — round-trip unchanged.
	ReturnPct float64 `json:"return_pct,omitempty"`
	Outcome   string  `json:"outcome,omitempty"` // WIN | LOSS
}

// QdrantMatch represents a matched vector document.
type QdrantMatch struct {
	ID      string        `json:"id"`
	Score   float64       `json:"score"`
	Payload QdrantPayload `json:"payload"`
}

// EnsureCollection creates the collection (768-dim cosine) if it doesn't exist.
// Returns true if the collection was freshly created.
func (qc *QdrantClient) EnsureCollection(ctx context.Context) (created bool, err error) {
	url := fmt.Sprintf("%s/collections/%s", qc.baseURL, qc.collection)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}
	resp, err := qc.httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return false, nil // already exists
		}
	}

	createBody, err := json.Marshal(map[string]interface{}{
		"vectors": map[string]interface{}{"size": 768, "distance": "Cosine"},
	})
	if err != nil {
		return false, err
	}
	req, err = http.NewRequestWithContext(ctx, "PUT", url, bytes.NewBuffer(createBody))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = qc.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("create collection request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("qdrant create returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return true, nil
}

// RecreateCollection drops the collection (if any) and creates it fresh. Used for
// the kalshi_markets collection so stale embeddings from a prior run's universe
// (e.g. markets that have since closed) don't linger across restarts.
func (qc *QdrantClient) RecreateCollection(ctx context.Context) error {
	url := fmt.Sprintf("%s/collections/%s", qc.baseURL, qc.collection)
	if req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil); err == nil {
		if resp, err := qc.httpClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	_, err := qc.EnsureCollection(ctx)
	return err
}

// InitializeCollection ensures the gordon_events RAG collection exists. It is left
// EMPTY on creation: it used to be seeded with hardcoded mock proposals (FED-RATE-2026,
// BTC-100K-DEC, AAPL-UP-200 — fabricated tickers with made-up fair values), which
// polluted semantic matches and shared the fake tickers the deleted rules fallback
// emitted. The collection now populates only from real closed-trade outcomes via the
// RAG-feedback consumer, so proposals learn from what actually resolved.
func (qc *QdrantClient) InitializeCollection(ctx context.Context) error {
	if _, err := qc.EnsureCollection(ctx); err != nil {
		return err
	}
	return nil
}

// QueryMatches searches Qdrant for the top K closest matches.
func (qc *QdrantClient) QueryMatches(ctx context.Context, vector []float32, k int) ([]QdrantMatch, error) {
	searchBody, err := json.Marshal(map[string]interface{}{
		"vector":       vector,
		"limit":        k,
		"with_payload": true,
	})
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/collections/%s/points/search", qc.baseURL, qc.collection)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(searchBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := qc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("qdrant search returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var searchResponse struct {
		Result []QdrantMatch `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&searchResponse); err != nil {
		return nil, err
	}

	return searchResponse.Result, nil
}

// UpsertEvent inserts a new event + vector + metadata payload into Qdrant.
//
// returnPct/outcome carry a realized trade result when this point comes from RAG
// feedback (a closed position); they're zero/empty for proposal-time upserts. proposal
// is the human-readable rationale to store — pass a real summary (e.g. a closed-trade
// outcome) to override the synthetic boilerplate, or "" to keep the default.
func (qc *QdrantClient) UpsertEvent(ctx context.Context, text string, vector []float32, contractID, action string, maxPrice, returnPct float64, outcome, proposal string) error {
	pointID := newUUID()
	if proposal == "" {
		proposal = fmt.Sprintf("[RAG-Engine] Semantic vector matching complete for %s. Mapped unstructured telemetry feed to 768-dimension embeddings, performed nearest-neighbor cosine similarity lookup on Qdrant, and verified risk caps. Queued for validation routing.", contractID)
	}
	pointsBody, err := json.Marshal(map[string]interface{}{
		"points": []map[string]interface{}{
			{
				"id":     pointID,
				"vector": vector,
				"payload": QdrantPayload{
					Text:       text,
					ContractID: contractID,
					Action:     action,
					MaxPrice:   maxPrice,
					Size:       1000,
					Proposal:   proposal,
					ReturnPct:  returnPct,
					Outcome:    outcome,
				},
			},
		},
	})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/collections/%s/points?wait=true", qc.baseURL, qc.collection)
	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewBuffer(pointsBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := qc.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qdrant upsert returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// DeletePointsByContractID removes every point whose payload contract_id matches.
// Used by the market matcher to prune markets that have left the live catalog
// (resolved/closed/delisted) so stale embeddings can't match news to dead markets.
func (qc *QdrantClient) DeletePointsByContractID(ctx context.Context, contractID string) error {
	body, err := json.Marshal(map[string]interface{}{
		"filter": map[string]interface{}{
			"must": []map[string]interface{}{
				{"key": "contract_id", "match": map[string]interface{}{"value": contractID}},
			},
		},
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/collections/%s/points/delete?wait=true", qc.baseURL, qc.collection)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := qc.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qdrant delete returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// ragFeedbackKey is the Redis list the executive pushes realized trade outcomes onto.
// Must match breen's ragFeedbackKey.
const ragFeedbackKey = "gekko:rag_feedback"

// RAGFeedback mirrors the closed-trade outcome the executive publishes. Gordon embeds
// the Text and stores it in Qdrant so future proposals can see how similar real trades
// actually resolved — instead of learning only from the hardcoded seed examples.
type RAGFeedback struct {
	ContractID string  `json:"contract_id"`
	EntryPrice float64 `json:"entry_price"`
	ExitPrice  float64 `json:"exit_price"`
	ReturnPct  float64 `json:"return_pct"`
	Action     string  `json:"action"`
	Outcome    string  `json:"outcome"`
	Text       string  `json:"text"`
}

// RunRAGFeedbackConsumer drains realized trade outcomes from gekko:rag_feedback,
// embeds each summary, and upserts it into the gordon_events collection so the RAG
// layer learns from real trades. Blocks until ctx is cancelled; run in its own
// goroutine. Across a multi-persona fleet the BRPOP is a shared work queue, so each
// outcome is consumed (and stored) exactly once regardless of which process drains it.
func RunRAGFeedbackConsumer(ctx context.Context, redisAddr string, oc *ProposalClient, qc *QdrantClient) {
	if redisAddr == "" {
		return
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: 2 * time.Second, ReadTimeout: 6 * time.Second})
	defer rdb.Close()

	log.Println("[RAG-FEEDBACK] consumer started; learning from real trade outcomes.")
	for {
		if ctx.Err() != nil {
			return
		}
		// Block up to 5s for the next outcome. Producer LPUSHes, we BRPOP ⇒ FIFO.
		res, err := rdb.BRPop(ctx, 5*time.Second, ragFeedbackKey).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err == redis.Nil {
				continue // timed out with nothing queued
			}
			time.Sleep(2 * time.Second) // Redis hiccup; back off before retrying
			continue
		}
		if len(res) < 2 {
			continue // [key, value]
		}
		var fb RAGFeedback
		if err := json.Unmarshal([]byte(res[1]), &fb); err != nil {
			log.Printf("[RAG-FEEDBACK] dropping malformed payload: %v", err)
			continue
		}
		processRAGFeedback(ctx, oc, qc, fb)
	}
}

// processRAGFeedback embeds one outcome and stores it in Qdrant. Errors are logged and
// the item is dropped — RAG feedback is best-effort enrichment, never a hard dependency.
func processRAGFeedback(ctx context.Context, oc *ProposalClient, qc *QdrantClient, fb RAGFeedback) {
	text := fb.Text
	if text == "" {
		text = fmt.Sprintf("Traded %s — %+.0f%% return (%s)", fb.ContractID, fb.ReturnPct, fb.Outcome)
	}
	action := strings.ToUpper(fb.Action)
	if action != "BUY" && action != "SELL" {
		action = "BUY"
	}

	embCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	vec, err := oc.GetEmbedding(embCtx, text)
	cancel()
	if err != nil {
		log.Printf("[RAG-FEEDBACK] embed failed for %s: %v", fb.ContractID, err)
		return
	}

	// max_price = exit_price, and the outcome text is stored as both the searchable
	// text and the rationale — not the synthetic engine boilerplate.
	upCtx, upCancel := context.WithTimeout(ctx, 5*time.Second)
	err = qc.UpsertEvent(upCtx, text, vec, fb.ContractID, action, fb.ExitPrice, fb.ReturnPct, fb.Outcome, text)
	upCancel()
	if err != nil {
		log.Printf("[RAG-FEEDBACK] upsert failed for %s: %v", fb.ContractID, err)
		return
	}
	log.Printf("[RAG-FEEDBACK] learned %s: %s", fb.Outcome, text)
}

// Helpers
func extractJSON(input string) string {
	// Strip reasoning think block if present (e.g. from deepseek-r1)
	if thinkStart := strings.Index(input, "<think>"); thinkStart != -1 {
		if thinkEnd := strings.Index(input, "</think>"); thinkEnd != -1 && thinkEnd > thinkStart {
			input = input[thinkEnd+8:]
		}
	}

	start := strings.Index(input, "{")
	end := strings.LastIndex(input, "}")
	if start == -1 || end == -1 || start >= end {
		return normalizeSmartQuotes(input)
	}
	return normalizeSmartQuotes(input[start : end+1])
}

// smartQuoteReplacer folds the Unicode curly quotes an LLM sometimes emits back to
// their ASCII forms. gemma in particular closes a JSON string value with a curly ”
// (U+201D) after opening it with a straight " — the straight string then never
// terminates and json.Unmarshal fails with "unexpected end of JSON input", silently
// dropping the whole proposal (14 gemma-neutral drops on 2026-07-14). Double curly
// quotes are the structural break; single curly quotes (’) are harmless inside a JSON
// string but folded too so stored rationale text stays clean.
var smartQuoteReplacer = strings.NewReplacer(
	"“", `"`, // “ left double
	"”", `"`, // ” right double
	"‘", "'", // ‘ left single
	"’", "'", // ’ right single
)

func normalizeSmartQuotes(s string) string {
	return smartQuoteReplacer.Replace(s)
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
