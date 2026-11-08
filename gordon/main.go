package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

// GekkoIngestEvent represents the incoming payload from Kahuna.
type GekkoIngestEvent struct {
	SourceID             string `json:"SourceID"`
	RawContent           string `json:"RawContent"`
	SourceURL            string `json:"SourceURL"`
	ExtractedTimestampNS int64  `json:"ExtractedTimestampNS"`
	PayloadHash          string `json:"PayloadHash"`
}

// StrategyProposal represents the outbound quantitative proposal.
type StrategyProposal struct {
	ContractID      string         `json:"contract_id"`
	Action          string         `json:"action"`
	AveragePrice    float64        `json:"average_price"`
	TotalSize       int            `json:"total_size"`
	SlippagePct     float64        `json:"slippage_pct"`
	LimitBreached   bool           `json:"limit_breached"`
	BreachReason    string         `json:"breach_reason,omitempty"`
	IcebergOrders   []IcebergOrder `json:"iceberg_orders"`
	Timestamp       time.Time      `json:"timestamp"`
	EventHash       string         `json:"event_hash"`
	ProcessingTime  string         `json:"processing_time"`
	ProposalText    string         `json:"proposal_text,omitempty"`
	SliceBoundaries []int          `json:"slice_boundaries,omitempty"`
	SourceID        string         `json:"source_id,omitempty"`
	SourceURL       string         `json:"source_url,omitempty"`
	ModelUsed       string         `json:"model_used,omitempty"`
	EmbedLatencyMS  int64          `json:"embed_latency_ms,omitempty"`
	QdrantLatencyMS int64          `json:"qdrant_latency_ms,omitempty"`
	LLMLatencyMS    int64          `json:"llm_latency_ms,omitempty"`

	// Identity & conviction (Phase 1 foundation for multi-persona + executive
	// comparison). StrategyID/Persona identify the emitting Gordon; Confidence is
	// the match/conviction score [0,1] the executive can weight on.
	StrategyID string  `json:"strategy_id,omitempty"`
	Persona    string  `json:"persona,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`

	// FairValue is this strategy's own estimate of the contract's true probability —
	// the LLM's number, or the rules-fallback pseudo-probability — captured BEFORE the
	// limit price is anchored to the book. Without it the estimate was discarded at
	// exactly the moment it became checkable: breen saw only the book-anchored price and
	// so could never re-test edge against the book it actually fills on
	// (2026-08-25 desk review, fix #4). Zero means "not supplied", which leaves every
	// downstream edge gate inert rather than guessing.
	FairValue float64 `json:"fair_value,omitempty"`

	// Bearish marks a signal-only proposal: a persona's conviction that a market
	// falls, emitted when it wanted to SELL a family it doesn't hold. The system is
	// YES-only so this can't be an order; breen's consensus weighs it AGAINST
	// concurrent BUYs on the same event (veto/shrink) instead of executing it.
	Bearish bool `json:"bearish,omitempty"`

	// SettleResolution is a deterministic settlement hint ("YES"/"NO") from a strategy
	// that already knows the outcome from real-world ground truth — the weather
	// strategy sets "YES" on a bucket the day's observed high has already cleared. breen
	// prefers a real Kalshi resolution, then this hint, and only then the sim coin flip.
	SettleResolution string `json:"settle_resolution,omitempty"`
}

type Config struct {
	KafkaBrokers          []string
	InboundTopic          string
	OutboundTopic         string
	ConsumerGroup         string
	RedisAddr             string
	KafkaSimulated        bool
	PortfolioMaxSize      int
	PortfolioMaxExp       float64
	PortfolioMaxSlip      float64
	OllamaURL             string
	QdrantURL             string
	OllamaModel           string
	OllamaEmbedModel      string
	OllamaFallbackModel   string
	OllamaProposalTimeout time.Duration
	OllamaMaxConcurrent   int           // max concurrent generate calls fleet-wide (Redis) + per process
	OllamaAcquireTimeout  time.Duration // max wait for a concurrency slot before falling back
	QdrantCollection      string
	MarketsCollection     string
	CatalogSyncInterval   time.Duration
	MatchThreshold        float64
	// Gemini API provider (per-persona A/B vs the local Ollama). The key comes
	// from GEMINI_API_KEY or a gitignored local file (GEMINI_API_KEY_FILE, e.g.
	// GEMINI_API_KEY.txt) — never the repo.
	GeminiAPIKey   string
	GeminiModel    string
	GeminiBaseURL  string
	GeminiMaxRPM   int      // free-tier requests/min cap for this persona
	GeminiMaxRPD   int      // free-tier requests/day cap for this persona
	GeminiPersonas []string // persona IDs routed to Gemini (runtime A/B switch)
}

// marketMatcher matches news events to real Kalshi markets; nil/dormant in keyless sim.
var marketMatcher *MarketMatcher

// riskStateReader reads breen's published risk state for the pre-LLM numeric veto;
// nil when Redis is unconfigured (gate off, behavior unchanged).
var riskStateReader *RiskStateReader

// bearCooldown suppresses re-spending an LLM call on a market where a persona just said
// SELL/NONE while we hold nothing there (the verdict can't become an order). Keyed by
// real ticker; held markets bypass it so exits are never delayed. BEARISH_COOLDOWN=0 off.
var (
	bearCooldownDur = getEnvDuration("BEARISH_COOLDOWN", 30*time.Minute)
	bearCooldownMu  sync.Mutex
	bearCooldown    = map[string]time.Time{}
	// Bearish signals only matter to breen's consensus; with it off they are noise.
	consensusEnabled = getEnvBool("CONSENSUS_ENABLED", false)
)

func markBearCooldown(ticker string) { markCooldownFor(ticker, bearCooldownDur) }

// declineCooldownDur is the (short) suppression after a plain DECLINE: a decline is
// the model's read of one news item, not a standing bearish view, so the market is
// re-askable soon. 0 = no suppression.
var declineCooldownDur = getEnvDuration("DECLINE_COOLDOWN", 2*time.Minute)

func markCooldownFor(ticker string, d time.Duration) {
	if d <= 0 || bearCooldownDur <= 0 || ticker == "" {
		return
	}
	bearCooldownMu.Lock()
	bearCooldown[ticker] = time.Now().Add(d)
	bearCooldownMu.Unlock()
}

func inBearCooldown(ticker string) bool {
	bearCooldownMu.Lock()
	defer bearCooldownMu.Unlock()
	until, ok := bearCooldown[ticker]
	if ok && time.Now().After(until) {
		delete(bearCooldown, ticker)
		return false
	}
	return ok
}

// llmLimiter caps concurrent Ollama generate calls across the persona fleet. Set
// in main; read by the health endpoint. Nil-safe (Acquire is a no-op when nil).
var llmLimiter *LLMLimiter

// modelControlsState holds the live, Redis-backed model knobs (stance/risk/
// selectivity). Global so the Kafka/sim workers and processEvent can read it
// without threading it through every signature (mirrors marketMatcher).
var modelControlsState *RedisControls

// personaID identifies this Gordon instance on every proposal it emits. Phase 1
// uses a single default; Phase 2 sets it per-instance via GORDON_PERSONA so the
// executive can attribute and compare competing personas.
var personaID = getEnv("GORDON_PERSONA", "gordon-default")

// weatherMode makes this gordon a dedicated deterministic weather trader: it prices
// Kalshi high-temp buckets from METAR observations and skips the LLM entirely.
// weatherStrategy is built in main() when weatherMode is set.
var weatherMode = getEnvBool("GORDON_WEATHER_MODE", false)
var weatherStrategy *WeatherStrategy

// sourceWeather is the SourceID kahuna stamps on METAR observation events.
const sourceWeather = "WEATHER"

// sourceWeatherForecast is the SourceID kahuna stamps on NWS daily-high FORECAST events
// (Phase 2 forecast-bias model input), distinct from realized METAR observations.
const sourceWeatherForecast = "WEATHER_FORECAST"

// Payoff-skew entry sizing (post-mortem #3). Run #2 concentrated its losses in
// high-priced YES entries (≥0.50 lost ≈$8k at a 6% win rate) because their reward:risk
// is poor and the bot has no edge there. maxEntryPrice refuses BUYs above the ceiling;
// entrySkewFloor bounds how far the payoff-skew multiplier can shrink an entry.
var maxEntryPrice = getEnvFloat("MAX_ENTRY_PRICE", 0.70)
var entrySkewFloor = getEnvFloat("ENTRY_SKEW_FLOOR", 0.30)

// Minimum edge vs the live book before a matched-market trade is allowed (dry-run
// forensics: maxPrice was overridden with the book price on match, so the strategy's
// own probability estimate was never compared to the price — any match traded).
// BUY needs fair − ask ≥ minEdge; SELL needs bid − fair ≥ minEdge. Matches the
// prompt's ">5% probability difference" rule. The top-of-book spread is added on
// top (requiredEdge): an edge smaller than the spread doesn't survive the round trip.
var minEdge = getEnvFloat("MIN_EDGE", 0.05)

// BET_SIZE_MULT scales every matched-market order after all downsizing (stance,
// skew, near-expiry). Breen's per-contract/category/total caps still clip it.
var betSizeMult = getEnvFloat("BET_SIZE_MULT", 1.0)

// Minimum entry price for BUYs. Raised 0.10→0.35 (2026-07-08 forensics): the
// whole book was longshots — every settled trade entered under 0.35 and the
// 0.20–0.35 band went 0-for-11 for −$168 (the run's entire realized loss), while
// no band ≥0.35 traded at all. The favorite-longshot pattern (LLM hallucinates
// edge on cheap YES, event resolves NO, −15% stop realizes it) survives the flat
// 5% edge gate and the 3× fair/ask cap alike, so the only durable fix is a price
// floor. 0.35 keeps the bot in the 0.35–0.50 reward:risk-≥1 window it has not
// disproven; earlier lesson stands too (entries under $0.10 went 0-for-23, −$301,
// where one tick is ±25% and the spread is the whole trade). Relax via env to
// re-open cheaper bands once edge there is demonstrated.
var minEntryPrice = getEnvFloat("MIN_ENTRY_PRICE", 0.35)

// Plausibility cap on a matched BUY's fair estimate (24h forensics, item 5): a
// fair more than this multiple above the ask is treated as model hallucination
// rather than edge and the proposal is skipped. 0 disables. Tightened 3.0→2.0
// (2026-07-08): at 3× an ask of 0.25 let a claimed fair of 0.75 through, so the
// cap was blind across the 0.20–0.35 band where the losses concentrated; 2× still
// admits a genuine doubling-mispricing while halving the hallucination headroom.
var fairAskMaxRatio = getEnvFloat("FAIR_ASK_MAX_RATIO", 2.0)

// Near-expiry entry guard (post-mortem #7). Contracts about to resolve are high-variance
// (KXINX-26JUN26 entered same-day; KXBEZELRO 0.47→0.03) and would be force-exited almost
// immediately by the executive. Skip entries resolving within nearExpirySkip; downsize
// those resolving within nearExpiryWindow by nearExpirySizeMult.
var nearExpiryWindow = getEnvDuration("NEAR_EXPIRY_WINDOW", 24*time.Hour)
var nearExpirySkip = getEnvDuration("NEAR_EXPIRY_SKIP", 1*time.Hour)
var nearExpirySizeMult = getEnvFloat("NEAR_EXPIRY_SIZE_MULT", 0.25)

// Entity-verification gate on news→market matches (clodds-mining ADOPT #1). After
// the embedding match picks a real ticker, verifyMatch checks that the news text and
// the market title agree on the critical entities (year, month, numeric threshold).
// The similarity match trusts cosine alone, so a semantically-near but wrong-strike or
// wrong-date market ("BTC>100k by Jan" ↔ "…by Feb") slips through and every downstream
// gate then runs against the WRONG book. A mismatch subtracts its penalty from the
// match confidence; a total penalty ≥ MATCH_VERIFY_DROP_PENALTY drops the match
// outright (the event is not traded — there is no valid live market for it). Fail-open:
// terse titles with no extractable entities yield no penalty. Disable via env.
var matchVerifyEnabled = getEnvBool("MATCH_VERIFY_ENABLED", true)
var matchVerifyDropPenalty = getEnvFloat("MATCH_VERIFY_DROP_PENALTY", 0.5)

func LoadConfig() *Config {
	brokersEnv := getEnv("KAFKA_BROKERS", "localhost:9092")
	brokers := strings.Split(brokersEnv, ",")
	for i, b := range brokers {
		brokers[i] = strings.TrimSpace(b)
	}

	return &Config{
		KafkaBrokers:     brokers,
		InboundTopic:     getEnv("KAFKA_INBOUND_TOPIC", "kahuna.ingest.public"),
		OutboundTopic:    getEnv("KAFKA_OUTBOUND_TOPIC", "gordon.strategies.proposed"),
		ConsumerGroup:    getEnv("KAFKA_CONSUMER_GROUP", "gordon-group"),
		RedisAddr:        getEnv("REDIS_ADDR", "localhost:6379"),
		KafkaSimulated:   getEnvBool("KAFKA_SIMULATED", true), // default to simulation for robust standalone runs
		PortfolioMaxSize: getEnvInt("PORTFOLIO_MAX_SIZE", 2500),
		PortfolioMaxExp:  getEnvFloat("PORTFOLIO_MAX_EXP", 8000.0),
		PortfolioMaxSlip: getEnvFloat("PORTFOLIO_MAX_SLIP", 0.03), // 3.0% max slippage
		OllamaURL:        getEnv("OLLAMA_URL", "http://localhost:11434"),
		QdrantURL:        getEnv("QDRANT_URL", "http://localhost:6333"),
		// qwen2.5:7b for stronger proposal reasoning; falls back to the fast 1.5b if a
		// call exceeds the proposal deadline (e.g. cold model load). A reasoning model
		// like deepseek-r1:7b emits long <think> traces that blow the 15s default, so
		// run it on a single persona with a raised OLLAMA_PROPOSAL_TIMEOUT (or a
		// per-persona Model/Timeout in personas.go) rather than across the fast fleet.
		OllamaModel:         getEnv("OLLAMA_MODEL", "qwen2.5:7b"),
		OllamaEmbedModel:    getEnv("OLLAMA_EMBED_MODEL", "nomic-embed-text"),
		OllamaFallbackModel: getEnv("OLLAMA_FALLBACK_MODEL", "qwen2.5:1.5b"),
		// Raised 15s→30s: a local 7B generation routinely exceeds 15s (cold model
		// load, CPU inference), which was a primary source of the 88% rules-fallback
		// rate. The concurrency limiter means we hold a slot for this long, so it
		// trades throughput for a real LLM answer — tune down on faster hardware.
		OllamaProposalTimeout: getEnvDuration("OLLAMA_PROPOSAL_TIMEOUT", 30*time.Second),
		// Fleet-wide concurrent generate cap. Default 2 suits a single local Ollama
		// (OLLAMA_NUM_PARALLEL is small); the personas queue in the limiter — which
		// respects deadlines — instead of piling into Ollama and timing out.
		OllamaMaxConcurrent:  getEnvInt("OLLAMA_MAX_CONCURRENT", 2),
		OllamaAcquireTimeout: getEnvDuration("OLLAMA_ACQUIRE_TIMEOUT", 30*time.Second),
		QdrantCollection:     getEnv("QDRANT_COLLECTION", "gordon_events"),
		MarketsCollection:    getEnv("KALSHI_MARKETS_COLLECTION", "kalshi_markets"),
		CatalogSyncInterval:  getEnvDuration("CATALOG_SYNC_INTERVAL", 2*time.Minute),
		MatchThreshold:       getEnvFloat("MARKET_MATCH_THRESHOLD", 0.5),
		// Defaults sized for the free AI Studio tier of gemini-3.1-flash-lite
		// (strict RPM/RPD caps); embeddings stay on Ollama regardless. NB: Google
		// retired the gemini-2.5-flash generation (generateContent → 404 "no longer
		// available") on 2026-07-09, which forced 100% fallback to local Ollama;
		// 3.1-flash-lite is the current cheap/fast equivalent.
		GeminiAPIKey:   readSecret(getEnv("GEMINI_API_KEY", ""), getEnv("GEMINI_API_KEY_FILE", "")),
		GeminiModel:    getEnv("GEMINI_MODEL", "gemini-3.1-flash-lite"),
		GeminiBaseURL:  getEnv("GEMINI_BASE_URL", defaultGeminiBaseURL),
		GeminiMaxRPM:   getEnvInt("GEMINI_MAX_RPM", 8),
		GeminiMaxRPD:   getEnvInt("GEMINI_MAX_RPD", 900),
		GeminiPersonas: splitCSV(getEnv("GEMINI_PERSONAS", "")),
	}
}

func main() {
	// Initialize configurations
	cfg := LoadConfig()

	// Initialize shared metrics telemetry registry
	telemetry := NewTelemetryRegistry()

	// Context for background routine control
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Model controls (stance/risk/selectivity). The default persona tracks the
	// global gekko:model_controls knob live; a named persona (Phase 2) runs as a
	// fixed personality so multiple Gordons can hold distinct opinions.
	persona := resolvePersona(personaID)
	if isDefaultPersona(personaID) {
		modelControlsState = NewRedisControls(cfg.RedisAddr, cfg.PortfolioMaxSize, cfg.PortfolioMaxExp, cfg.PortfolioMaxSlip, cfg.MatchThreshold)
		go modelControlsState.run(ctx, 5*time.Second)
	} else {
		modelControlsState = NewStaticControls(cfg.RedisAddr, persona.Stance, persona.RiskTier, persona.Threshold)
		go modelControlsState.run(ctx, 5*time.Second)
		log.Printf("[INFO] Persona %q active: stance=%s risk=%s threshold=%.2f (fixed personality; tracks only the global max-slippage cap).",
			persona.ID, persona.Stance, persona.RiskTier, persona.Threshold)
	}

	// Per-gordon editable config (Gordons tab): live nudge/stance/risk/threshold plus a
	// startup-only model/provider override, persisted in gekko:gordon_config:<id> and
	// preserved across run resets. Only named personas (the fleet) carry one; the
	// default persona tracks the global model_controls blob instead. LoadOnce reads it
	// synchronously so the model/provider override is available before the backends are
	// built below; the poller (started after the client exists) applies live edits.
	var gordonCfg *GordonConfig
	if !isDefaultPersona(personaID) {
		gordonCfg = NewGordonConfig(cfg.RedisAddr, persona)
		loadCtx, loadCancel := context.WithTimeout(ctx, 2*time.Second)
		gordonCfg.LoadOnce(loadCtx)
		loadCancel()
		modelControlsState.SetGordonConfig(gordonCfg)
	}

	// Read-only client for signal enrichment (momentum published by marketdata).
	initSignalRedis(cfg.RedisAddr)

	// External feeds (Phase 3c, all optional / default-off): econ-calendar blackout
	// and a news-sentiment API. Absent config ⇒ nil ⇒ no behavior change.
	econCalendar = NewEconCalendar(getEnv("ECON_CALENDAR_FILE", ""), getEnv("ECON_CALENDAR_URL", ""), getEnvDuration("ECON_BLACKOUT_WINDOW", 30*time.Minute))
	if econCalendar != nil {
		go econCalendar.run(ctx, getEnvDuration("ECON_CALENDAR_REFRESH", 10*time.Minute))
	}
	sentimentClient = NewSentimentClient(getEnv("NEWS_SENTIMENT_URL", ""), getEnvDuration("NEWS_SENTIMENT_TIMEOUT", 3*time.Second))
	if sentimentClient != nil {
		log.Println("[INFO] External news-sentiment API enabled.")
	}

	// Setup portfolio limits reader and order book fetcher
	obFetcher := NewOrderBookFetcher(cfg.RedisAddr)
	engine := NewLiquidityEngine(modelControlsState, obFetcher)

	// Initialize the proposal client (local Ollama embeddings, always) and its
	// pluggable proposal backend. A persona may override the proposal model and
	// timeout (e.g. a deliberate persona on deepseek-r1:7b) so it runs a heavier
	// model on a longer deadline without slowing the fast fleet — and/or pick a
	// hosted backend (provider=gemini) to A/B a hosted model against the local fleet
	// on the per-persona scorecard. Embeddings and the fallback always stay local.
	provider := persona.Provider
	// The Gordons-tab stored config overrides the registry's Provider (empty ⇒ keep).
	if p := gordonCfg.Provider(); p != "" {
		provider = p
	}
	if provider == "" {
		provider = ProviderOllama
	}
	// GEMINI_PERSONAS is a runtime convenience override on top of the registry/config
	// Provider field — flip a matching persona to Gemini without a registry edit.
	for _, id := range cfg.GeminiPersonas {
		if id == persona.ID {
			provider = ProviderGemini
		}
	}
	if provider == ProviderGemini && cfg.GeminiAPIKey == "" {
		log.Printf("[ERROR] Persona %q wants provider=gemini but no API key found (set GEMINI_API_KEY or put the key in GEMINI_API_KEY.txt / GEMINI_API_KEY_FILE) — falling back to ollama.", persona.ID)
		provider = ProviderOllama
	}
	proposalModel := cfg.OllamaModel
	if provider == ProviderGemini {
		proposalModel = cfg.GeminiModel
	}
	if persona.Model != "" {
		proposalModel = persona.Model
	}
	// The Gordons-tab stored config wins over the registry's Model (empty ⇒ keep).
	if m := gordonCfg.Model(); m != "" {
		proposalModel = m
	}
	proposalTimeout := cfg.OllamaProposalTimeout
	if persona.Timeout > 0 {
		proposalTimeout = persona.Timeout
	}

	ollamaClient := NewProposalClient(cfg.OllamaURL, cfg.OllamaEmbedModel)
	// Seed the personality prompt. Named personas route it through GordonConfig so the
	// Gordons tab can edit it live; the default persona uses its fixed registry nudge.
	if gordonCfg != nil {
		gordonCfg.SetNudgeSink(ollamaClient.SetPersonaNudge)
		go gordonCfg.run(ctx, 5*time.Second)
	} else {
		ollamaClient.SetPersonaNudge(persona.Nudge)
	}

	// Concurrency limiter for local generate calls. Shared fleet-wide via Redis so
	// all persona processes hitting one local Ollama stay within OLLAMA_MAX_CONCURRENT;
	// falls back to in-process-only limiting when Redis is absent. The slot lease
	// covers one worst-case generate (timeout + headroom) so a crashed persona's
	// slot auto-frees. Only the Ollama backend takes a slot — a hosted backend has
	// no local-GPU contention.
	var limiterRedis *redis.Client
	if cfg.RedisAddr != "" {
		limiterRedis = redis.NewClient(&redis.Options{
			Addr:        cfg.RedisAddr,
			DialTimeout: 2 * time.Second,
			ReadTimeout: 2 * time.Second,
		})
	}
	llmLimiter = NewLLMLimiter(limiterRedis, cfg.OllamaMaxConcurrent, proposalTimeout+15*time.Second)
	log.Printf("[INFO] LLM concurrency limit: %d fleet-wide (%s) | acquire wait ≤ %s | per-attempt timeout %s.",
		cfg.OllamaMaxConcurrent, map[bool]string{true: "redis-shared", false: "in-process only"}[limiterRedis != nil],
		cfg.OllamaAcquireTimeout, proposalTimeout)

	// Manual per-gordon knob (promote/demote/pause from the UI). Its RATE half is
	// estimated from concurrency ÷ proposal timeout — how many generate calls the
	// local Ollama can drain per second — split across the assumed fleet size; the
	// operator overrides both (and per-gordon weights) live via gekko:gordon_priority.
	// GORDON_FLEET_RATE=0 keeps the auto estimate. Nil-safe: absent Redis ⇒ open gate.
	// The rate gates the Ollama backend; the PAUSE half is cross-cutting (any backend).
	fleetRate := getEnvFloat("GORDON_FLEET_RATE", 0)
	if fleetRate <= 0 {
		fleetRate = float64(cfg.OllamaMaxConcurrent) / proposalTimeout.Seconds()
	}
	fleetSize := getEnvInt("GORDON_FLEET_SIZE", 3)
	priority := NewPriorityControls(limiterRedis, personaID, fleetRate, fleetSize)
	ollamaClient.priority = priority
	go priority.run(ctx, 5*time.Second)
	// Advertise this gordon so the UI's priority pane lists the real fleet.
	go RegisterPresence(ctx, limiterRedis, personaID, persona.Label)
	if _, iv, _ := priority.Snapshot(); iv > 0 {
		log.Printf("[INFO] Proposal rate throttle: est. fleet %.3f gen/s ÷ %d gordons ⇒ ~%s/call at weight 1 (promote/demote live via UI).",
			fleetRate, fleetSize, iv.Round(time.Millisecond))
	}

	// Build the proposal backends. The fallback is ALWAYS a local Ollama backend on
	// the small fallback model (nil when the primary already IS that model), so a
	// hosted outage or rate-cap degrades to a real LLM answer instead of rules. Each
	// Ollama backend carries the fleet slot limiter + rate throttle; the Gemini
	// backend carries only its own RPM/RPD limiter.
	newOllamaBackend := func(model string) *ollamaBackend {
		return &ollamaBackend{
			baseURL:         strings.TrimSuffix(cfg.OllamaURL, "/"),
			model:           model,
			httpClient:      &http.Client{Timeout: proposalTimeout + 5*time.Second},
			proposalTimeout: proposalTimeout,
			acquireTimeout:  cfg.OllamaAcquireTimeout,
			genLimiter:      llmLimiter,
			priority:        priority,
		}
	}
	if cfg.OllamaFallbackModel != "" && cfg.OllamaFallbackModel != proposalModel {
		ollamaClient.fallback = newOllamaBackend(cfg.OllamaFallbackModel)
	}
	if provider == ProviderGemini {
		ollamaClient.primary = &geminiBackend{
			key:             cfg.GeminiAPIKey,
			baseURL:         cfg.GeminiBaseURL,
			model:           proposalModel,
			httpClient:      &http.Client{Timeout: proposalTimeout + 5*time.Second},
			proposalTimeout: proposalTimeout,
			limiter:         NewGeminiRateLimiter(cfg.GeminiMaxRPM, cfg.GeminiMaxRPD).WithRedis(limiterRedis, "gekko:backend_budget:gemini:"+proposalModel),
		}
		log.Printf("[INFO] Persona %q proposal backend: gemini model=%s timeout=%s (≤%d rpm, ≤%d req/day; Ollama slot limiter bypassed; fallback stays local: %s).",
			persona.ID, proposalModel, proposalTimeout, cfg.GeminiMaxRPM, cfg.GeminiMaxRPD, cfg.OllamaFallbackModel)
	} else {
		ollamaClient.primary = newOllamaBackend(proposalModel)
		if proposalModel != cfg.OllamaModel || proposalTimeout != cfg.OllamaProposalTimeout {
			log.Printf("[INFO] Persona %q proposal backend: ollama model=%s timeout=%s.", persona.ID, proposalModel, proposalTimeout)
		}
	}

	qdrantClient := NewQdrantClient(cfg.QdrantURL, cfg.QdrantCollection)
	if !weatherMode && getEnvBool("NEWS_STORE", true) {
		recentNews = newNewsStore(NewQdrantClient(cfg.QdrantURL, getEnv("NEWS_COLLECTION", "gordon_news")), getEnvInt("NEWS_BUFFER", 500))
	}

	// Initialize Qdrant collection on start
	initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := qdrantClient.InitializeCollection(initCtx); err != nil {
		log.Printf("[WARNING] Failed to initialize Qdrant collection: %v", err)
	}
	initCancel()

	// Close the RAG learning loop: drain realized trade outcomes the executive
	// publishes to gekko:rag_feedback, embed them, and store them in gordon_events so
	// proposals improve from real trades instead of only the seed examples.
	go RunRAGFeedbackConsumer(ctx, cfg.RedisAddr, ollamaClient, qdrantClient)

	// Real-market matcher: embeds discovered Kalshi markets (published by the
	// marketdata service to gekko:kalshi_markets) and matches news → real tickers.
	// Stays dormant when no catalog exists (keyless sim falls back to synthetic).
	marketsClient := NewQdrantClient(cfg.QdrantURL, cfg.MarketsCollection)
	marketMatcher = NewMarketMatcher(ollamaClient, marketsClient, cfg.RedisAddr, cfg.MatchThreshold)
	marketMatcher.thresholdFn = modelControlsState.Threshold // live selectivity knob
	go marketMatcher.runSync(ctx, cfg.CatalogSyncInterval)

	// Pre-LLM numeric veto: read breen's risk-state mirror so proposals the executive
	// would deterministically reject skip the LLM call entirely. Fail-open if absent.
	riskStateReader = NewRiskStateReader(cfg.RedisAddr)

	// Outbound Strategy Producer Client
	var prodClient *kgo.Client
	var err error
	if len(cfg.KafkaBrokers) > 0 {
		prodClient, err = kgo.NewClient(kgo.SeedBrokers(cfg.KafkaBrokers...))
		if err != nil {
			log.Printf("[WARNING] Failed to start Kafka producer client: %v.", err)
			prodClient = nil
		} else {
			pingCtx, pingCancel := context.WithTimeout(ctx, 2*time.Second)
			if err := prodClient.Ping(pingCtx); err != nil {
				log.Printf("[WARNING] Kafka brokers unreachable for producer: %v. Outbound streaming disabled.", err)
				prodClient.Close()
				prodClient = nil
			}
			pingCancel()
		}
	}

	// Dedicated weather mode: this gordon consumes only WEATHER observations and
	// prices Kalshi high-temp buckets deterministically (no LLM). Built here so
	// processEvent can route to it. Other personas ignore WEATHER events.
	if weatherMode {
		weatherStrategy = NewWeatherStrategy(cfg.RedisAddr, prodClient, cfg.OutboundTopic, DefaultWeatherConfig(), telemetry)
		log.Printf("[INFO] Weather strategy ENABLED (persona %q): deterministic cleared-bucket pricing, LLM bypassed. cfg=%+v",
			personaID, weatherStrategy.cfg)
	}

	// Expose /healthz for the status dashboard (both TUI and headless modes).
	startHealthServer(getEnv("HEALTH_ADDR", ":8092"), telemetry, ollamaClient, persona)

	// Start Background Worker (Kafka Consumer or Simulator)
	if cfg.KafkaSimulated {
		go startSimulatedWorker(ctx, telemetry, engine, prodClient, cfg.OutboundTopic, ollamaClient, qdrantClient)
	} else {
		// Attempt to start real Kafka Ingestion Client
		go startKafkaWorker(ctx, cfg, telemetry, engine, prodClient, ollamaClient, qdrantClient)
	}
	recentNews.start(ctx)
	initOnDemand(cfg.RedisAddr)
	if onDemand != nil {
		onDemand.useEmbedder(ollamaClient)
	}
	startActivityTrigger(ctx, cfg.RedisAddr, telemetry, engine, prodClient, cfg.OutboundTopic, ollamaClient, qdrantClient)

	// Initialize and run the TUI Dashboard in the main thread
	if getEnvBool("HEADLESS", false) {
		log.Println("[INFO] Headless mode active. Running worker thread directly. Press Ctrl+C to exit.")
		// Wait for signal or block indefinitely
		select {}
	} else {
		p := tea.NewProgram(NewTUIModel(telemetry), tea.WithAltScreen())
		if _, err := p.Run(); err != nil {
			fmt.Printf("Error running TUI program: %v\n", err)
			os.Exit(1)
		}
	}
}

func startKafkaWorker(ctx context.Context, cfg *Config, tr *TelemetryRegistry, engine *LiquidityEngine, prodClient *kgo.Client, oc *ProposalClient, qc *QdrantClient) {
	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.KafkaBrokers...),
		kgo.ConsumerGroup(cfg.ConsumerGroup),
		kgo.ConsumeTopics(cfg.InboundTopic),
	}

	cl, err := kgo.NewClient(opts...)
	if err != nil {
		tr.AddProposalLog(fmt.Sprintf("[ERROR] Kafka connection failed: %v. Running simulation fallback...", err))
		startSimulatedWorker(ctx, tr, engine, prodClient, cfg.OutboundTopic, oc, qc)
		return
	}
	defer cl.Close()

	// Check connection
	pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := cl.Ping(pingCtx); err != nil {
		pingCancel()
		tr.AddProposalLog("[WARNING] Kafka unreachable. Falling back to simulated stream.")
		startSimulatedWorker(ctx, tr, engine, prodClient, cfg.OutboundTopic, oc, qc)
		return
	}
	pingCancel()

	tr.AddProposalLog("[INFO] Kafka consumer connected and active. Ingesting streams...")

	for {
		select {
		case <-ctx.Done():
			return
		default:
			fetches := cl.PollRecords(ctx, 10)
			if fetches.IsClientClosed() {
				return
			}
			if errs := fetches.Errors(); len(errs) > 0 {
				for _, err := range errs {
					tr.AddProposalLog(fmt.Sprintf("[KAFKA WARN] Fetch error: %v", err.Err))
				}
			}

			iter := fetches.RecordIter()
			for !iter.Done() {
				record := iter.Next()

				// Set local lag tracker (decrements as we process)
				tr.SetLag(0)
				tr.IncrementConsumed()

				processEvent(ctx, record.Value, tr, engine, prodClient, cfg.OutboundTopic, oc, qc)
			}
		}
	}
}

func startSimulatedWorker(ctx context.Context, tr *TelemetryRegistry, engine *LiquidityEngine, prodClient *kgo.Client, outboundTopic string, oc *ProposalClient, qc *QdrantClient) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	headlines := []string{
		`{"SourceID":"REUTERS","RawContent":"Federal Reserve meeting suggests interest rate cuts incoming","SourceURL":"https://www.reuters.com/business","PayloadHash":"abc123fed"}`,
		`{"SourceID":"TWITTER","RawContent":"Apple shares climb as market sentiment shifts on AAPL-UP-200","SourceURL":"https://twitter.com/X","PayloadHash":"tweet456aapl"}`,
		`{"SourceID":"KALSHI","RawContent":"Closed yes AAPL-UP-200 market status update","SourceURL":"wss://api.kalshi.com","PayloadHash":"kalshi789"}`,
		`{"SourceID":"POLYMARKET","RawContent":"BTC-100K-DEC bids are mounting on volume surge","SourceURL":"wss://clob.polymarket.com","PayloadHash":"poly101btc"}`,
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	tr.AddProposalLog("[SIMULATION] Outbound simulation channel initialized. Emitting mock events...")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Emit simulated event
			idx := rng.Intn(len(headlines))
			eventBytes := []byte(headlines[idx])

			tr.SetLag(int64(rng.Intn(5)))
			tr.IncrementConsumed()

			processEvent(ctx, eventBytes, tr, engine, prodClient, outboundTopic, oc, qc)
		}
	}
}

func processEvent(ctx context.Context, value []byte, tr *TelemetryRegistry, engine *LiquidityEngine, prodClient *kgo.Client, outboundTopic string, oc *ProposalClient, qc *QdrantClient) {
	var ev GekkoIngestEvent
	if err := json.Unmarshal(value, &ev); err != nil {
		tr.AddProposalLog(fmt.Sprintf("[ERROR] JSON unmarshal failed: %v", err))
		return
	}
	processParsedEvent(ctx, ev, nil, tr, engine, prodClient, outboundTopic, oc, qc)
}

// processParsedEvent is the whole news→proposal funnel. hint, when set, names the market the
// event is about (the activity trigger), so the embedding match is skipped.
func processParsedEvent(ctx context.Context, ev GekkoIngestEvent, hint *matchHint, tr *TelemetryRegistry, engine *LiquidityEngine, prodClient *kgo.Client, outboundTopic string, oc *ProposalClient, qc *QdrantClient) {

	// Weather routing. The dedicated weather gordon handles ONLY WEATHER observations
	// (deterministic, LLM-bypassed); every other persona ignores them so a temperature
	// reading never enters the news→market matcher.
	if ev.SourceID == sourceWeather {
		if !weatherMode || oc.Paused() {
			return
		}
		var obs weatherObs
		if err := json.Unmarshal([]byte(ev.RawContent), &obs); err != nil {
			tr.AddProposalLog(fmt.Sprintf("[WEATHER] bad observation payload: %v", err))
			return
		}
		weatherStrategy.OnObservation(ctx, ev, obs)
		return
	}
	// The NWS forecast feed (Phase 2) is cached by the weather gordon for predictive
	// pricing; every other persona ignores it.
	if ev.SourceID == sourceWeatherForecast {
		if weatherMode && !oc.Paused() {
			weatherStrategy.OnForecast(ev)
		}
		return
	}
	if weatherMode {
		return // the weather gordon trades nothing but weather
	}

	// Paused persona (priority weight 0): drop the event up front. Doing any work
	// here — embedding, Qdrant, or especially the rules fallback — would let a
	// muted gordon keep emitting proposals, which is what the operator paused it
	// to stop. GenerateProposal also guards this, but returning now skips the whole
	// RAG pipeline for muted personas.
	if oc.Paused() {
		tr.AddProposalLog(fmt.Sprintf("[PAUSED] %s — persona paused, dropping event", ev.PayloadHash))
		return
	}

	var contractID string
	var action string
	var maxPrice float64
	var proposalText string
	var size int
	var modelUsed string
	var embedLatency int64
	var qdrantLatency int64
	var llmLatency int64

	// 1. Vector RAG Pipeline
	embedStart := time.Now()
	embCtx, embCancel := context.WithTimeout(ctx, 3*time.Second)
	embedding, err := oc.GetEmbedding(embCtx, ev.RawContent)
	embCancel()
	if err == nil {
		embedLatency = time.Since(embedStart).Milliseconds()
		if hint == nil { // ordinary news: remember it so an activity-triggered look can find it later
			rememberHeadline(oc, ev.RawContent, embedding)
		}
	}

	// Match this event to a real Kalshi market (when live discovery is active).
	var realTicker, realTitle string
	var realBid, realAsk float64
	var matchConfidence float64
	if hint != nil {
		realTicker, realTitle, realBid, realAsk, matchConfidence = hint.ticker, hint.title, hint.bid, hint.ask, hint.confidence
	} else if err == nil && marketMatcher != nil {
		if tk, tt, yb, ya, sc, ok := marketMatcher.Match(ctx, embedding); ok {
			realTicker, realTitle, realBid, realAsk, matchConfidence = tk, tt, yb, ya, sc
		}
	}

	// Entity-verification gate (clodds-mining ADOPT #1): the embedding match trusts
	// cosine alone, so a wrong-strike/wrong-date market can be paired to the news and
	// every downstream gate then runs against the wrong book. Verify the year/month/
	// threshold agree before spending an LLM slot on it. Placed before the pre-LLM veto
	// so a clearly-wrong match costs nothing.
	if realTicker != "" && matchVerifyEnabled && hint == nil {
		if penalty, warnings := verifyMatch(ev.RawContent, realTitle); penalty > 0 {
			if penalty >= matchVerifyDropPenalty {
				tr.AddProposalLog(fmt.Sprintf("[VERIFY] %s dropped — entity mismatch (penalty %.2f ≥ %.2f): %s",
					realTicker, penalty, matchVerifyDropPenalty, strings.Join(warnings, "; ")))
				return
			}
			matchConfidence = math.Max(0, matchConfidence-penalty)
			tr.AddProposalLog(fmt.Sprintf("[VERIFY] %s entity-mismatch penalty %.2f (conf→%.2f): %s",
				realTicker, penalty, matchConfidence, strings.Join(warnings, "; ")))
		}
	}

	// Read the live momentum/imbalance signal once (matched markets only) so it feeds
	// both the proposal prompt's microstructure block (ADOPT #6) and the post-LLM
	// confidence enrichment (ADOPT #5) without a second Redis round-trip.
	var mTrend, mVol, mImb, mImbConf float64
	var mOK bool
	if realTicker != "" {
		mTrend, mVol, mImb, mImbConf, mOK = readMomentum(ctx, signalRedis, realTicker)
	}

	// Pre-LLM numeric veto (shrink LLM calls): once we know the real market, skip the
	// LLM/Qdrant work when the executive would deterministically reject a new entry
	// there anyway — drawdown halt, family loss-limit/cooldown, or imminent expiry.
	// Guarded on breen holding NO position in the family: with an open position a
	// proposal could be a valid exit (SELLs always pass the risk gate) or a bypass-
	// cooldown add, so those still reach the LLM. Only fires on matched real tickers;
	// unmatched events can't be checked against per-family state and are left alone.
	if realTicker != "" {
		held, blocked, reason := riskStateReader.Evaluate(ctx, realTicker)
		if blocked {
			tr.RecordPregateSkip()
			tr.AddProposalLog(fmt.Sprintf("[PREGATE] %s skipped pre-LLM — %s", realTicker, reason))
			return
		}
		if !held && inBearCooldown(realTicker) {
			tr.RecordPregateSkip()
			tr.AddProposalLog(fmt.Sprintf("[PREGATE] %s skipped pre-LLM — bearish/no-trade cooldown (%v), nothing held", realTicker, bearCooldownDur))
			return
		}
		// Near-expiry hard skip, hoisted ahead of the LLM (the near-dated downsize path
		// stays post-LLM where size is known). Same no-open-position safety: a SELL exit
		// near expiry is desirable, so only pre-skip when nothing is held there.
		if !held {
			if exp, ok := contractExpiry(realTicker); ok {
				if ttl := time.Until(exp); ttl <= nearExpirySkip {
					tr.RecordPregateSkip()
					tr.AddProposalLog(fmt.Sprintf("[PREGATE] %s skipped pre-LLM — resolves in %v (≤ %v)", realTicker, ttl.Round(time.Minute), nearExpirySkip))
					return
				}
			}
		}
		// Tradeability pre-skip: Match already prefers a tradeable market, so landing here
		// means nothing near this event can be entered (one-sided, spread over breen's
		// cap, or ask outside the entry band) and breen or the post-LLM gates would refuse
		// it anyway. Only when nothing is held there — a SELL to exit an open position
		// can fill against one side, so those still reach the LLM (mirrors the
		// near-expiry guard above).
		if !held {
			if ok, why := quoteTradeable(realBid, realAsk); !ok {
				tr.RecordPregateSkip()
				tr.AddProposalLog(fmt.Sprintf("[PREGATE] %s skipped pre-LLM — %s, entry would be refused", realTicker, why))
				return
			}
		}
	}

	// Phase 3: an activity-triggered look needs news about the market. Check what we have already
	// ingested; otherwise ask kahuna for it and wait (bounded) before spending an LLM call.
	if hint != nil && err == nil && onDemand != nil {
		enriched, ok := onDemand.enrich(ctx, tr, ev.RawContent, embedding, hint.ticker, hint.title)
		if !ok {
			return
		}
		ev.RawContent = enriched
	}

	if err != nil {
		tr.AddProposalLog(fmt.Sprintf("[RAG WARN] Embeddings failed: %v. Dropping event.", err))
	} else {
		// Query Qdrant for top 3 matches
		qdrantStart := time.Now()
		qdrantCtx, qdrantCancel := context.WithTimeout(ctx, 3*time.Second)
		matches, err := qc.QueryMatches(qdrantCtx, embedding, getEnvInt("RAG_TOPK", 3))
		qdrantCancel()
		if err == nil {
			qdrantLatency = time.Since(qdrantStart).Milliseconds()
		}

		if err != nil {
			tr.AddProposalLog(fmt.Sprintf("[RAG WARN] Qdrant search failed: %v. Dropping event.", err))
		} else {
			// Query Ollama LLM to generate proposal. GenerateProposal owns its own
			// deadlines now — it waits (bounded) for a concurrency slot, then runs
			// each model attempt under proposalTimeout — so we pass the long-lived
			// ctx rather than pre-bounding the whole thing to one proposalTimeout.
			// When the event matched a real market, the LLM sees its title and
			// bid/ask so max_price is a probability estimate for THAT contract.
			var liveMkt *LiveMarket
			if realTicker != "" {
				liveMkt = &LiveMarket{Ticker: realTicker, Title: realTitle, YesBid: realBid, YesAsk: realAsk}
				// Unknown holdings (breen outage) count as held so exits are never suppressed.
				held, known := riskStateReader.HeldContract(ctx, realTicker)
				liveMkt.Held = held || !known
				if mOK {
					liveMkt.TrendPct, liveMkt.VolSurge, liveMkt.Imbalance = mTrend, mVol, mImb
				}
				if exp, ok := contractExpiry(realTicker); ok {
					if h := time.Until(exp).Hours(); h > 0 {
						liveMkt.HoursToExp = h
					}
				}
			}
			llmStart := time.Now()
			proposalResult, err := oc.GenerateProposal(ctx, ev.RawContent, matches, liveMkt)
			if err == nil {
				llmLatency = time.Since(llmStart).Milliseconds()
				modelUsed = oc.ModelUsed()
			}

			if errors.Is(err, ErrLLMDeclined) {
				// The model examined the event and chose not to trade. Drop it rather
				// than manufacturing a BUY (old behavior) or falling to the rules
				// heuristic — a decline is a decision, not a generation failure.
				tr.AddProposalLog(fmt.Sprintf("[DECLINE] %s — model declined to trade (%v)", ev.PayloadHash, err))
				if realTicker != "" {
					if held, _, _ := riskStateReader.Evaluate(ctx, realTicker); !held {
						markCooldownFor(realTicker, declineCooldownDur)
					}
				}
				return
			}
			if errors.Is(err, ErrPersonaPaused) {
				// A paused persona (weight 0) is muted on purpose. Drop the event
				// entirely — falling to the rules heuristic here would let a paused
				// gordon keep emitting rules-fallback proposals, which is exactly the
				// output the operator paused it to stop.
				tr.AddProposalLog(fmt.Sprintf("[PAUSED] %s — persona paused, dropping event (no rules fallback)", ev.PayloadHash))
				return
			}
			if err != nil {
				tr.AddProposalLog(fmt.Sprintf("[RAG WARN] LLM generation failed: %v. Dropping event.", err))
			} else {
				contractID = proposalResult.ContractID
				action = proposalResult.Action
				maxPrice = proposalResult.MaxPrice
				size = proposalResult.Size
				proposalText = proposalResult.Proposal

				// Async save to Qdrant to populate our database
				go func() {
					upsertCtx, upsertCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer upsertCancel()
					// Proposal-time upsert: no realized outcome yet (returnPct=0, outcome=""),
					// and "" proposal keeps the default engine boilerplate.
					_ = qc.UpsertEvent(upsertCtx, ev.RawContent, embedding, contractID, action, maxPrice, 0, "", "")
				}()
			}
		}
	}

	// Pipeline failure ⇒ drop the event. An embedding/Qdrant/LLM failure is a
	// generation FAILURE, not a trading signal. The old rules fallback fabricated a
	// proposal from keyword matching — hardcoded mock tickers (BTC-100K-DEC), hardcoded
	// "fair values", and a RANDOMIZED size — which was harmless on synthetic tickers
	// (engine rejects "no live order book") but actively dangerous on a live Kalshi
	// match, where the fabricated fair value gated a real order (2026-07-10 logs show
	// rules-fallback+kalshi-match SELLs going out). Mirrors the [DECLINE]/[PAUSED]
	// drops above: a non-answer is not a trade.
	if contractID == "" {
		tr.RecordProposalSource(false) // count as a pipeline miss for the System-tab ratio
		tr.AddProposalLog(fmt.Sprintf("[DROP] %s — RAG pipeline produced no proposal (embed/Qdrant/LLM failure); dropping event, no fabricated fallback", ev.PayloadHash))
		return
	}

	// Record the source split for the System-tab pipeline-health metric: everything
	// past the drop above was decided by the LLM.
	tr.RecordProposalSource(true)

	// Override with the matched REAL Kalshi ticker so the trade is placed on a live
	// market. Before anchoring the limit price to the real book, require the
	// strategy's own fair-value estimate (LLM probability, or the rules-fallback
	// pseudo-probability) to clear the book by minEdge — previously maxPrice was
	// simply overwritten with the book price, so edge-vs-price was never checked
	// and every match traded at whatever the market asked.
	// Capture the strategy's fair value before the block below anchors maxPrice to the
	// book — after that the estimate is gone, and it is the one number breen needs to
	// re-check edge against the book it actually fills on (desk review fix #4).
	fairValue := maxPrice
	if realTicker != "" {
		contractID = realTicker
		if fairImplausible(action, maxPrice, realAsk, fairAskMaxRatio) {
			tr.AddProposalLog(fmt.Sprintf("[EDGE] %s %s skipped — fair %.2f > %.1f× ask %.2f: implausible estimate, not edge",
				action, realTicker, maxPrice, fairAskMaxRatio, realAsk))
			return
		}
		if edge, gated := edgeVsMarket(action, maxPrice, realBid, realAsk); gated {
			if need := requiredEdge(minEdge, realBid, realAsk); edge < need {
				tr.AddProposalLog(fmt.Sprintf("[EDGE] %s %s skipped — fair %.2f vs book %.2f/%.2f: edge %+.3f < required %.3f (min %.2f + spread)",
					action, realTicker, maxPrice, realBid, realAsk, edge, need, minEdge))
				return
			}
			// Edge confirmed — anchor the limit to the book so the order is fillable.
			if strings.ToUpper(action) == "SELL" {
				maxPrice = realBid
			} else {
				maxPrice = realAsk
			}
		}
		modelUsed += "+kalshi-match"
		if proposalText != "" {
			proposalText += " [matched live Kalshi market " + realTicker + "]"
		} else {
			proposalText = "Matched live Kalshi market " + realTicker + " via semantic similarity."
		}
	}

	// 1a-i. Signal enrichment (Phase 3b/3c + clodds-mining ADOPT #5): fold momentum,
	// order-book depth imbalance, cross-venue divergence, and headline sentiment into
	// the proposal's confidence and size on top of the base match score. Runs BEFORE the
	// bearish-signal branch below so an ask-heavy book raises a bear's conviction — the
	// enriched Confidence flows into breen's weightedConviction on the emitted signal.
	sigIn := SignalInputs{MatchScore: matchConfidence, Sentiment: scoreSentiment(ctx, ev.RawContent), Credibility: sourceCredibility(ev.SourceID, ev.SourceURL)}
	if mOK {
		// Reuse the pre-LLM read (matched market — momentum key == contractID).
		sigIn.TrendPct, sigIn.VolSurge = mTrend, mVol
		sigIn.Imbalance, sigIn.ImbalanceConf = mImb, mImbConf
	} else if realTicker == "" {
		// Unmatched (synthetic) contractID: no pre-read happened; try its own key.
		if t, v, imb, imbConf, ok := readMomentum(ctx, signalRedis, contractID); ok {
			sigIn.TrendPct, sigIn.VolSurge = t, v
			sigIn.Imbalance, sigIn.ImbalanceConf = imb, imbConf
		}
	}
	if marketMatcher != nil && realTicker != "" && len(embedding) > 0 {
		if _, edge, ok := marketMatcher.crossVenueEdge(ctx, embedding, contractID, realAsk); ok {
			sigIn.CrossEdge = edge
		}
	}
	conf, sizeMult, notes := compositeConfidence(sigIn, action)
	matchConfidence = conf
	if sizeMult != 1.0 {
		if ns := int(math.Round(float64(size) * sizeMult)); ns >= 1 {
			size = ns
		}
	}
	if len(notes) > 0 {
		proposalText = strings.TrimSpace(proposalText + " [signals: " + strings.Join(notes, ", ") + "]")
	}

	// 1a-ii. SELL means sell: we can't take a YES exit on a contract we don't hold. But
	// dropping the bear's view outright muzzled the bear personas — nearly all their
	// output was unheld SELLs (2026-07-10 forensics: 3 orders from 1,309 LLM calls).
	// Instead emit it as a BEARISH SIGNAL: breen's consensus weighs it against
	// concurrent BUYs on the same event so an active "this falls" opinion can veto or
	// shrink a long. It is never executed as an order (YES-only system). The check is
	// EXACT-contract, not family: family granularity let sells of sibling strikes pose
	// as exits (hold KXGDP-...-T2.0 ⇒ 56 phantom sells of T0.0/T0.5/T1.0 executed and
	// no-op'd, 2026-07-12). Exactly-held contracts still pass through as real exits;
	// known=false (breen outage) also passes through so exits are never suppressed.
	if strings.EqualFold(action, "SELL") {
		if held, known := riskStateReader.HeldContract(ctx, contractID); known && !held {
			markBearCooldown(contractID)
			emitBearishSignal(ctx, prodClient, outboundTopic, tr, contractID, ev, matchConfidence, modelUsed, proposalText)
			return
		}
	}

	// 1b. Apply the live stance knob: soft-demote trades that oppose the user's
	// stance (size scaled down) rather than dropping them, so the bot stays
	// responsive in a one-sided market. Skip only if demoted size rounds below 1.
	if modelControlsState != nil {
		stance := modelControlsState.Stance()
		if f := stanceDemoteFactor(stance, action); f < 1.0 {
			demoted := int(float64(size) * f)
			if demoted < 1 {
				tr.AddProposalLog(fmt.Sprintf("[STANCE] %s stance: dropped %s %s (opposes stance, size→0)", stance, action, contractID))
				return
			}
			tr.AddProposalLog(fmt.Sprintf("[STANCE] %s stance: demoted %s %s size %d→%d (opposes stance)", stance, action, contractID, size, demoted))
			size = demoted
		}
	}

	// 1c-i. Econ-calendar blackout (Phase 3c): don't open right before a scheduled
	// high-impact release affecting this market. No-op unless a calendar is loaded.
	if blocked, evt := econCalendar.Blackout(ev.RawContent+" "+contractID, time.Now()); blocked {
		tr.AddProposalLog(fmt.Sprintf("[BLACKOUT] %s skipped — within blackout window of %q", contractID, evt))
		return
	}

	// 1c-iii. Payoff-skew entry sizing + ceiling (post-mortem #3): expensive YES entries
	// have poor reward:risk and empirically no edge. Refuse BUYs above the ceiling and
	// scale the rest down by their skew. Only applies to entries (BUY), never exits.
	if strings.EqualFold(action, "BUY") {
		if minEntryPrice > 0 && maxPrice > 0 && maxPrice < minEntryPrice {
			tr.AddProposalLog(fmt.Sprintf("[FLOOR] %s skipped — entry %.3f below floor %.2f (tick/spread noise dominates sub-dime binaries)", contractID, maxPrice, minEntryPrice))
			return
		}
		if maxEntryPrice > 0 && maxPrice > maxEntryPrice {
			tr.AddProposalLog(fmt.Sprintf("[SKEW] %s skipped — entry %.2f above ceiling %.2f (poor reward:risk)", contractID, maxPrice, maxEntryPrice))
			return
		}
		if m := entrySkewSizeMult(maxPrice, entrySkewFloor); m < 1.0 {
			if ns := int(math.Round(float64(size) * m)); ns >= 1 {
				tr.AddProposalLog(fmt.Sprintf("[SKEW] %s entry %.2f skew ×%.2f: size %d→%d", contractID, maxPrice, m, size, ns))
				size = ns
			}
		}
	}

	// 1c-iv. Near-expiry entry guard (post-mortem #7): imminent resolutions are high
	// variance and would be force-exited straight away, so refuse them; downsize the
	// merely near-dated. Only when the ticker carries a parseable resolution date.
	if strings.EqualFold(action, "BUY") {
		if exp, ok := contractExpiry(contractID); ok {
			ttl := time.Until(exp)
			if ttl <= nearExpirySkip {
				tr.AddProposalLog(fmt.Sprintf("[EXPIRY] %s skipped — resolves in %v (≤ %v)", contractID, ttl.Round(time.Minute), nearExpirySkip))
				return
			}
			if ttl <= nearExpiryWindow && nearExpirySizeMult < 1.0 {
				if ns := int(math.Round(float64(size) * nearExpirySizeMult)); ns >= 1 {
					tr.AddProposalLog(fmt.Sprintf("[EXPIRY] %s near expiry (%v left): size %d→%d", contractID, ttl.Round(time.Hour), size, ns))
					size = ns
				}
			}
		}
	}

	// 2. Query Order Book from Redis
	ob, obErr := engine.orderBookFetcher.GetOrderBook(ctx, contractID)
	if obErr == nil && ob != nil {
		tr.SetOrderBook(ob)
	}

	// 3. Process trade allocation through Liquidity Impact Engine
	if betSizeMult != 1.0 {
		size = max(1, int(math.Round(float64(size)*betSizeMult)))
	}
	evalResult, err := engine.Evaluate(ctx, contractID, action, size, maxPrice)
	if err != nil {
		tr.AddProposalLog(fmt.Sprintf("[ERROR] Engine evaluation failed for %s: %v", contractID, err))
		return
	}

	// Derive iceberg slice boundaries from the square-root price-impact model. Note:
	// engine.Evaluate already computed evalResult.SlippagePct (absolute price terms) and
	// that value is authoritative for BOTH display and limit enforcement — we do NOT
	// overwrite it here. Previously this block replaced the enforced slippage with the
	// sqrt model's number, so the proposal advertised one slippage while the trade was
	// gated/shrunk on a wildly different one. CalculateImpact is now used only for slicing.
	impactCtx, impactCancel := context.WithTimeout(ctx, 2*time.Second)
	_, sliceBoundaries, impactErr := engine.CalculateImpact(impactCtx, contractID, action, evalResult.AdjustedSize)
	impactCancel()

	if impactErr != nil {
		tr.AddProposalLog(fmt.Sprintf("[IMPACT WARN] Slice-boundary calculation failed: %v", impactErr))
	}

	// Track processing latency
	tr.AddLatency(evalResult.EvaluationTime.Microseconds())

	// 4. Publish strategies proposed
	proposal := StrategyProposal{
		ContractID:      evalResult.ContractID,
		Action:          evalResult.Action,
		AveragePrice:    evalResult.AvgFillPrice,
		TotalSize:       evalResult.AdjustedSize,
		SlippagePct:     evalResult.SlippagePct,
		LimitBreached:   evalResult.LimitBreached,
		BreachReason:    evalResult.BreachReason,
		IcebergOrders:   evalResult.IcebergChunks,
		Timestamp:       time.Now(),
		EventHash:       ev.PayloadHash,
		ProcessingTime:  fmt.Sprintf("%dµs", evalResult.EvaluationTime.Microseconds()),
		ProposalText:    proposalText,
		SliceBoundaries: sliceBoundaries,
		SourceID:        ev.SourceID,
		SourceURL:       ev.SourceURL,
		ModelUsed:       modelUsed,
		EmbedLatencyMS:  embedLatency,
		QdrantLatencyMS: qdrantLatency,
		LLMLatencyMS:    llmLatency,
		StrategyID:      personaID,
		Persona:         personaID,
		Confidence:      matchConfidence,
		FairValue:       fairValue,
	}

	proposalBytes, err := json.Marshal(proposal)
	if err != nil {
		tr.AddProposalLog(fmt.Sprintf("[ERROR] Proposal marshal failed: %v", err))
		return
	}
	tr.IncrementProposed()

	// Sync outbound messages if Kafka producer is live
	if prodClient != nil {
		record := &kgo.Record{
			Topic: outboundTopic,
			Value: proposalBytes,
		}
		prodCtx, prodCancel := context.WithTimeout(ctx, 2*time.Second)
		if err := prodClient.ProduceSync(prodCtx, record).FirstErr(); err != nil {
			tr.AddProposalLog(fmt.Sprintf("[KAFKA ERR] Strategy emit failed: %v", err))
		}
		prodCancel()
	}

	// Log strategy proposal dynamically to rolling TUI log panel
	statusEmoji := "🟢"
	if evalResult.LimitBreached {
		statusEmoji = "🟡"
	}

	signalsStr := ""
	if len(notes) > 0 {
		signalsStr = " | signals: " + strings.Join(notes, ", ")
	}
	logMsg := fmt.Sprintf("%s [%s] %-4s %-4d %-12s @ %-5.2f | conf %.2f | fair %.3f | Slip: %.2f%% (Slices: %d) | model=%s%s (%dµs)",
		statusEmoji,
		personaID,
		evalResult.Action,
		evalResult.AdjustedSize,
		evalResult.ContractID,
		evalResult.AvgFillPrice,
		matchConfidence,
		fairValue,
		evalResult.SlippagePct*100,
		len(sliceBoundaries),
		modelUsed,
		signalsStr,
		evalResult.EvaluationTime.Microseconds(),
	)
	tr.AddProposalLog(logMsg)
}

// emitBearishSignal publishes a signal-only proposal (Bearish=true) for an unheld
// SELL: the persona's conviction that this market falls. breen's consensus weighs it
// against concurrent BUYs on the same event rather than executing it (YES-only
// system). No-op without a live producer (console mode).
func emitBearishSignal(ctx context.Context, prodClient *kgo.Client, outboundTopic string, tr *TelemetryRegistry, contractID string, ev GekkoIngestEvent, confidence float64, modelUsed, proposalText string) {
	tr.AddProposalLog(fmt.Sprintf("[HOLDINGS] SELL %s → bearish veto signal (no open position to exit)", contractID))
	if prodClient == nil || !consensusEnabled {
		return
	}
	signal := StrategyProposal{
		ContractID:   contractID,
		Action:       "SELL",
		Bearish:      true,
		Confidence:   confidence,
		EventHash:    ev.PayloadHash,
		Timestamp:    time.Now(),
		StrategyID:   personaID,
		Persona:      personaID,
		ModelUsed:    modelUsed,
		ProposalText: proposalText,
		SourceID:     ev.SourceID,
		SourceURL:    ev.SourceURL,
	}
	b, err := json.Marshal(signal)
	if err != nil {
		tr.AddProposalLog(fmt.Sprintf("[ERROR] Bearish signal marshal failed: %v", err))
		return
	}
	rec := &kgo.Record{Topic: outboundTopic, Value: b}
	prodCtx, prodCancel := context.WithTimeout(ctx, 2*time.Second)
	if err := prodClient.ProduceSync(prodCtx, rec).FirstErr(); err != nil {
		tr.AddProposalLog(fmt.Sprintf("[KAFKA ERR] Bearish signal emit failed: %v", err))
	}
	prodCancel()
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	val, ok := os.LookupEnv(key)
	if !ok {
		return defaultVal
	}
	b, err := strconv.ParseBool(val)
	if err != nil {
		return defaultVal
	}
	return b
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	val, ok := os.LookupEnv(key)
	if !ok {
		return defaultVal
	}
	i, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return i
}

func getEnvFloat(key string, defaultVal float64) float64 {
	val, ok := os.LookupEnv(key)
	if !ok {
		return defaultVal
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return defaultVal
	}
	return f
}
