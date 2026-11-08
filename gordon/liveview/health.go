package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	healthStartTime    = time.Now()
	proposalsBroadcast atomic.Int64
)

// HealthResponse is the common shape every Gekko component returns from /healthz.
type HealthResponse struct {
	Component     string                 `json:"component"`
	Status        string                 `json:"status"`
	UptimeSeconds float64                `json:"uptime_seconds"`
	Stats         map[string]interface{} `json:"stats"`
}

// componentStatus is one entry in the aggregated /api/status response.
type componentStatus struct {
	Component     string                 `json:"component"`
	Reachable     bool                   `json:"reachable"`
	Status        string                 `json:"status"`
	UptimeSeconds float64                `json:"uptime_seconds"`
	Stats         map[string]interface{} `json:"stats,omitempty"`
	Error         string                 `json:"error,omitempty"`
}

// liveviewHealth builds liveview's own health snapshot.
func liveviewHealth() HealthResponse {
	clientsMu.Lock()
	clientCount := len(clients)
	clientsMu.Unlock()

	proposalsMu.RLock()
	buffered := len(proposals)
	proposalsMu.RUnlock()

	return HealthResponse{
		Component:     "liveview",
		Status:        "ok",
		UptimeSeconds: time.Since(healthStartTime).Seconds(),
		Stats: map[string]interface{}{
			"proposals_broadcast": proposalsBroadcast.Load(),
			"proposals_buffered":  buffered,
			"sse_clients":         clientCount,
		},
	}
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(liveviewHealth())
}

// fetchHealth queries one component's /healthz and maps it into a componentStatus.
func fetchHealth(name, url string) componentStatus {
	cs := componentStatus{Component: name, Reachable: false, Status: "unreachable"}
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		cs.Error = err.Error()
		return cs
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		cs.Error = "non-200 status from healthz"
		return cs
	}

	var hr HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		cs.Error = "invalid health payload"
		return cs
	}

	cs.Reachable = true
	cs.Status = hr.Status
	cs.UptimeSeconds = hr.UptimeSeconds
	cs.Stats = hr.Stats
	return cs
}

// statInt / statFloat read a numeric stat from a decoded health payload (JSON
// numbers decode as float64).
func statInt(m map[string]interface{}, k string) int64 {
	if f, ok := m[k].(float64); ok {
		return int64(f)
	}
	return 0
}
func statFloat(m map[string]interface{}, k string) float64 {
	if f, ok := m[k].(float64); ok {
		return f
	}
	return 0
}
func statStr(m map[string]interface{}, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// statStrSlice reads a []string stat (JSON arrays decode as []interface{}), used
// for the per-gordon recent-activity feed. Returns an empty slice when absent so
// the JSON re-encodes as [] rather than null.
func statStrSlice(m map[string]interface{}, k string) []string {
	out := []string{}
	if arr, ok := m[k].([]interface{}); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// gordonHealthURLs returns the per-persona gordon health endpoints: the CSV
// GORDON_HEALTH_URLS when set, else the single GORDON_HEALTH_URL (backward compat).
func gordonHealthURLs() []string {
	if csv := getEnv("GORDON_HEALTH_URLS", ""); csv != "" {
		var out []string
		for _, u := range strings.Split(csv, ",") {
			if s := strings.TrimSpace(u); s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{getEnv("GORDON_HEALTH_URL", "http://localhost:8092/healthz")}
}

// aggregateGordon polls every gordon persona and folds them into one component:
// additive counters (events_consumed, proposals_generated, kafka_offset_lag) are
// summed across personas; avg_latency_ms is averaged. This makes the dashboard's
// "Proposals Generated" reflect ALL personas, comparable to the executive's
// global "Orders Executed".
func aggregateGordon(urls []string) componentStatus {
	res := make([]componentStatus, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) { defer wg.Done(); res[i] = fetchHealth("gordon", u) }(i, u)
	}
	wg.Wait()
	return foldGordon(res)
}

// foldGordon folds per-persona health results into one gordon component (pure;
// no network, so it's unit-testable). Additive counters are summed across
// reachable personas; avg_latency_ms is averaged.
func foldGordon(res []componentStatus) componentStatus {
	cs := componentStatus{Component: "gordon", Status: "unreachable"}
	var sumConsumed, sumProposed, sumLag int64
	var sumLLM, sumFallback, sumWaits, sumTimeouts, sumFailOpen int64
	var sumPregate int64
	var latSum, waitMsSum, maxUptime float64
	// Hosted-backend persona aggregation — only some personas run a hosted backend,
	// so these stay zero unless at least one reports backend_* telemetry. Backend-
	// scoped (works for any hosted backend, not just Gemini).
	var backendPersonas int
	var backendCallsToday, backendMaxRPD, backendPrimaryOK, backendPrimaryErr, backendFallbacks, backendLastOK int64
	backendName, backendModel := "", ""
	reachable := 0
	personas := make([]map[string]interface{}, 0, len(res))
	for _, h := range res {
		name := statStr(h.Stats, "persona")
		if name == "" {
			name = "unknown"
		}
		provider := statStr(h.Stats, "backend") // "" ⇒ local Ollama
		if provider == "" {
			provider = "ollama"
		}
		personas = append(personas, map[string]interface{}{
			"persona":             name,
			"reachable":           h.Reachable,
			"proposals_generated": statInt(h.Stats, "proposals_generated"),
			"llm_proposals":       statInt(h.Stats, "llm_proposals"),
			"llm_fallbacks":       statInt(h.Stats, "llm_fallbacks"),
			"pregate_skips":       statInt(h.Stats, "pregate_skips"),
			"provider":            provider,
			"model":               statStr(h.Stats, "model"),
			"stance":              statStr(h.Stats, "stance"),
			"risk_tier":           statStr(h.Stats, "risk_tier"),
			"match_threshold":     statFloat(h.Stats, "match_threshold"),
			"nudge":               statStr(h.Stats, "nudge"),
			"recent_activity":     statStrSlice(h.Stats, "recent_activity"),
		})
		if !h.Reachable {
			continue
		}
		reachable++
		// Hosted-backend telemetry is for the External APIs card, so count ONLY personas
		// on a non-local backend. Every persona now publishes `backend` (incl. "ollama"),
		// so an empty/"ollama" value must not be treated as hosted (it would mislabel the
		// Gemini card as "ollama").
		if b := statStr(h.Stats, "backend"); b != "" && b != "ollama" {
			backendPersonas++
			backendCallsToday += statInt(h.Stats, "backend_calls_today")
			backendMaxRPD += statInt(h.Stats, "backend_max_rpd")
			backendPrimaryOK += statInt(h.Stats, "backend_primary_ok")
			backendPrimaryErr += statInt(h.Stats, "backend_primary_err")
			backendFallbacks += statInt(h.Stats, "backend_fallbacks")
			if ok := statInt(h.Stats, "backend_last_ok_unix"); ok > backendLastOK {
				backendLastOK = ok
			}
			backendName = b
			backendModel = statStr(h.Stats, "backend_model")
		}
		sumConsumed += statInt(h.Stats, "events_consumed")
		sumProposed += statInt(h.Stats, "proposals_generated")
		sumLag += statInt(h.Stats, "kafka_offset_lag")
		latSum += statFloat(h.Stats, "avg_latency_ms")
		sumLLM += statInt(h.Stats, "llm_proposals")
		sumFallback += statInt(h.Stats, "llm_fallbacks")
		sumPregate += statInt(h.Stats, "pregate_skips")
		sumWaits += statInt(h.Stats, "llm_slot_waits")
		sumTimeouts += statInt(h.Stats, "llm_slot_timeouts")
		sumFailOpen += statInt(h.Stats, "llm_slot_fail_open")
		waitMsSum += statFloat(h.Stats, "llm_slot_avg_wait_ms")
		if h.UptimeSeconds > maxUptime {
			maxUptime = h.UptimeSeconds
		}
	}
	cs.Reachable = reachable > 0
	switch {
	case reachable == len(res) && reachable > 0:
		cs.Status = "ok"
	case reachable > 0:
		cs.Status = "degraded"
	default:
		cs.Error = "no gordon personas reachable"
	}
	avgLat := 0.0
	avgWaitMs := 0.0
	if reachable > 0 {
		avgLat = latSum / float64(reachable)
		avgWaitMs = waitMsSum / float64(reachable)
	}
	cs.UptimeSeconds = maxUptime
	cs.Stats = map[string]interface{}{
		"events_consumed":     sumConsumed,
		"proposals_generated": sumProposed,
		"kafka_offset_lag":    sumLag,
		"avg_latency_ms":      avgLat,
		"personas_reachable":  reachable,
		"personas_total":      len(res),
		"personas":            personas,
		// LLM reliability, summed across personas (fallback ratio = fallbacks /
		// (fallbacks + proposals)); frontend computes the ratio so aggregation stays
		// correct (a ratio-of-sums, not a mean of per-persona ratios).
		"llm_proposals":        sumLLM,
		"llm_fallbacks":        sumFallback,
		"pregate_skips":        sumPregate,
		"llm_slot_waits":       sumWaits,
		"llm_slot_timeouts":    sumTimeouts,
		"llm_slot_fail_open":   sumFailOpen,
		"llm_slot_avg_wait_ms": avgWaitMs,
	}
	// Surface hosted-backend telemetry only when a hosted-backend persona is present,
	// so the frontend renders the external-API card just for A/B runs.
	if backendPersonas > 0 {
		cs.Stats["backend_personas"] = backendPersonas
		cs.Stats["backend"] = backendName
		cs.Stats["backend_model"] = backendModel
		cs.Stats["backend_calls_today"] = backendCallsToday
		cs.Stats["backend_max_rpd"] = backendMaxRPD
		cs.Stats["backend_primary_ok"] = backendPrimaryOK
		cs.Stats["backend_primary_err"] = backendPrimaryErr
		cs.Stats["backend_fallbacks"] = backendFallbacks
		cs.Stats["backend_last_ok_unix"] = backendLastOK
	}
	return cs
}

// handleStatus aggregates /healthz from every component into a single payload
// for the frontend status page. The frontend reaches this via the Vite /api proxy.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Remote components and their health URLs (overridable via env). Gordon is
	// handled separately because it may be several personas behind one component.
	type target struct{ name, url string }
	targets := []target{
		{"broker-worker", getEnv("BROKER_HEALTH_URL", "http://localhost:8090/healthz")},
		{"breen", getEnv("EXECUTIVE_HEALTH_URL", "http://localhost:8091/healthz")},
		{"kahuna", getEnv("KAHUNA_HEALTH_URL", "http://localhost:8085/healthz")},
	}
	// marketdata is optional — only health-check it when explicitly enabled.
	if mdURL := getEnv("MARKETDATA_HEALTH_URL", ""); mdURL != "" {
		targets = append(targets, target{"marketdata", mdURL})
	}

	results := make([]componentStatus, len(targets)+1)

	// liveview's own health (always reachable — we are it).
	lh := liveviewHealth()
	results[0] = componentStatus{
		Component:     lh.Component,
		Reachable:     true,
		Status:        lh.Status,
		UptimeSeconds: lh.UptimeSeconds,
		Stats:         lh.Stats,
	}

	// Fan out to the remote components concurrently.
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(idx int, tg target) {
			defer wg.Done()
			results[idx+1] = fetchHealth(tg.name, tg.url)
		}(i, t)
	}
	wg.Wait()

	// Gordon: aggregate across all personas into one component.
	results = append(results, aggregateGordon(gordonHealthURLs()))

	healthy := 0
	for _, c := range results {
		if c.Reachable && c.Status == "ok" {
			healthy++
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"generated_at":    time.Now().Format(time.RFC3339),
		"healthy_count":   healthy,
		"component_count": len(results),
		"components":      results,
	})
}
