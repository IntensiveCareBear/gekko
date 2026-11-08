package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Proposal-LLM providers a persona can run on. Ollama is the local default;
// Gemini is a hosted API persona for A/B-testing against the local fleet.
// Embeddings ALWAYS stay on Ollama (nomic-embed-text) regardless of provider —
// the Qdrant collections are built in that vector space.
const (
	ProviderOllama = "ollama"
	ProviderGemini = "gemini"
)

const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"

// readSecret returns the inline value when set, else the trimmed contents of
// file. Mirrors kahuna/config so API keys can live in a gitignored local file
// (e.g. GEMINI_API_KEY.txt) instead of the environment or the repo.
func readSecret(inline, file string) string {
	if s := strings.TrimSpace(inline); s != "" {
		return s
	}
	if file != "" {
		if b, err := os.ReadFile(file); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// splitCSV splits a comma-separated list, trimming blanks.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// GeminiRateLimiter paces requests to the Gemini API. The free AI Studio tier
// has hard per-model RPM and requests-per-day caps, so an API persona throttles
// itself instead of relying on 429s: minimum spacing between calls (RPM) plus a
// daily budget (RPD, UTC day). In-process state only — Google enforces the real
// limits server-side, and a 429 after a restart takes the same fallback path as
// any other generation failure.
type GeminiRateLimiter struct {
	mu          sync.Mutex
	minGap      time.Duration
	maxPerDay   int
	nextAllowed time.Time
	day         string // UTC date the counter belongs to
	used        int
	rdb         *redis.Client // optional: shares the daily count across the fleet and restarts
	budgetKey   string        // Redis key prefix; "<prefix>:<YYYY-MM-DD>" is the counter
}

// WithRedis makes the daily budget fleet-wide and restart-proof: one INCR counter
// per UTC day (48h TTL) instead of per-process memory. Redis errors fall back to
// the local counter — Google enforces the real quota either way.
func (g *GeminiRateLimiter) WithRedis(rdb *redis.Client, keyPrefix string) *GeminiRateLimiter {
	g.rdb, g.budgetKey = rdb, keyPrefix
	return g
}

func (g *GeminiRateLimiter) dayKey(day string) string { return g.budgetKey + ":" + day }

func NewGeminiRateLimiter(rpm, rpd int) *GeminiRateLimiter {
	if rpm <= 0 {
		rpm = 8
	}
	if rpd <= 0 {
		rpd = 900
	}
	return &GeminiRateLimiter{minGap: time.Minute / time.Duration(rpm), maxPerDay: rpd}
}

// Wait blocks until this call's RPM slot opens, or errors immediately when the
// daily budget is spent or the wait would blow the ctx deadline. The slot is
// reserved before sleeping so concurrent callers space out correctly.
func (g *GeminiRateLimiter) Wait(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	today := time.Now().UTC().Format("2006-01-02")
	if g.day != today {
		g.day = today
		g.used = 0
	}
	if g.used >= g.maxPerDay {
		g.mu.Unlock()
		return fmt.Errorf("daily budget exhausted (%d requests)", g.maxPerDay)
	}
	if g.rdb != nil {
		rctx, cancel := context.WithTimeout(context.Background(), time.Second)
		n, err := g.rdb.Incr(rctx, g.dayKey(today)).Result()
		if err == nil {
			g.rdb.Expire(rctx, g.dayKey(today), 48*time.Hour)
			g.used = int(n) // adopt the fleet-wide count
		}
		if err == nil && int(n) > g.maxPerDay {
			g.rdb.Decr(rctx, g.dayKey(today)) // don't let denied calls inflate the count
			cancel()
			g.mu.Unlock()
			return fmt.Errorf("daily budget exhausted (%d requests)", g.maxPerDay)
		}
		cancel()
		if err == nil {
			g.used-- // the common g.used++ below re-adds this call
		}
	}
	now := time.Now()
	wait := g.nextAllowed.Sub(now)
	if wait < 0 {
		wait = 0
	}
	slotStart := now
	if g.nextAllowed.After(now) {
		slotStart = g.nextAllowed
	}
	g.nextAllowed = slotStart.Add(g.minGap)
	g.used++
	g.mu.Unlock()

	if wait == 0 {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(dl) {
		return fmt.Errorf("rate pacing (%s wait) exceeds deadline", wait)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// Snapshot returns today's usage against the daily budget and the RPM cap the
// limiter is pacing to, for the health endpoint. Safe on a nil limiter.
func (g *GeminiRateLimiter) Snapshot() (usedToday, maxPerDay, rpm int) {
	if g == nil {
		return 0, 0, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// The daily counter rolls over on the next Wait; report 0 if the stored day
	// is stale so the dashboard doesn't show yesterday's spend after midnight UTC.
	today := time.Now().UTC().Format("2006-01-02")
	used := g.used
	if g.day != today {
		used = 0
	}
	if g.rdb != nil {
		rctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if n, err := g.rdb.Get(rctx, g.dayKey(today)).Int(); err == nil {
			used = n
		}
		cancel()
	}
	if g.minGap > 0 {
		rpm = int(time.Minute / g.minGap)
	}
	return used, g.maxPerDay, rpm
}

// The Gemini generateContent call and its telemetry now live on geminiBackend in
// backend.go; this file keeps the shared rate limiter, secret loading, and the
// provider constants.
