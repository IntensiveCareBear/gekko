package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ── Econ calendar blackout (Phase 3c) ───────────────────────────────────────
//
// Avoid opening positions in the minutes before a scheduled high-impact release
// (FOMC, CPI, payrolls): volatility/uncertainty spikes and a fresh bet is closer
// to a coin flip. Fully optional — disabled unless ECON_CALENDAR_FILE or
// ECON_CALENDAR_URL is configured. Source is a JSON array of EconEvent.

type EconEvent struct {
	Time     time.Time `json:"time"`
	Title    string    `json:"title"`
	Keywords []string  `json:"keywords"`
}

type EconCalendar struct {
	file   string
	url    string
	window time.Duration

	mu     sync.RWMutex
	events []EconEvent
}

// NewEconCalendar returns nil when no source is configured (feature disabled).
func NewEconCalendar(file, url string, window time.Duration) *EconCalendar {
	if file == "" && url == "" {
		return nil
	}
	if window <= 0 {
		window = 30 * time.Minute
	}
	return &EconCalendar{file: file, url: url, window: window}
}

// run loads the calendar once and (for a URL source) refreshes periodically.
func (c *EconCalendar) run(ctx context.Context, interval time.Duration) {
	if c == nil {
		return
	}
	c.refresh(ctx)
	if c.url == "" || interval <= 0 {
		return // static file: no refresh loop
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refresh(ctx)
		}
	}
}

func (c *EconCalendar) refresh(ctx context.Context) {
	var raw []byte
	if c.file != "" {
		b, err := os.ReadFile(c.file)
		if err != nil {
			log.Printf("[WARN] econ calendar file %s: %v (blackout disabled this cycle)", c.file, err)
			return
		}
		raw = b
	} else {
		req, err := http.NewRequestWithContext(ctx, "GET", c.url, nil)
		if err != nil {
			return
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			log.Printf("[WARN] econ calendar url %s: %v", c.url, err)
			return
		}
		defer resp.Body.Close()
		raw, _ = io.ReadAll(resp.Body)
	}
	var events []EconEvent
	if err := json.Unmarshal(raw, &events); err != nil {
		log.Printf("[WARN] econ calendar decode: %v", err)
		return
	}
	c.mu.Lock()
	c.events = events
	c.mu.Unlock()
	log.Printf("[INFO] Econ calendar loaded: %d events (blackout window %v).", len(events), c.window)
}

// Blackout reports whether text matches a calendar event whose release is within
// the blackout window from now. Match is by keyword substring (case-insensitive).
func (c *EconCalendar) Blackout(text string, now time.Time) (bool, string) {
	if c == nil {
		return false, ""
	}
	lc := strings.ToLower(text)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.events {
		// In the [release-window, release] pre-print window?
		if now.Before(e.Time.Add(-c.window)) || now.After(e.Time) {
			continue
		}
		for _, kw := range e.Keywords {
			if kw != "" && strings.Contains(lc, strings.ToLower(kw)) {
				return true, e.Title
			}
		}
	}
	return false, ""
}

// ── External news-sentiment API (Phase 3c) ──────────────────────────────────
//
// When NEWS_SENTIMENT_URL is set, headline sentiment comes from that service
// (POST {"text": "..."} → {"score": -1..1}); otherwise callers fall back to the
// built-in lexicon (headlineSentiment).

type SentimentClient struct {
	url  string
	http *http.Client
}

// NewSentimentClient returns nil when NEWS_SENTIMENT_URL is unset (use lexicon).
func NewSentimentClient(url string, timeout time.Duration) *SentimentClient {
	if url == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &SentimentClient{url: url, http: &http.Client{Timeout: timeout}}
}

// Score returns a polarity in [-1,1]; ok=false on any error so the caller falls
// back to the lexicon.
func (s *SentimentClient) Score(ctx context.Context, text string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, "POST", s.url, bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var out struct {
		Score float64 `json:"score"`
	}
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &out) != nil {
		return 0, false
	}
	return clamp(out.Score, -1, 1), true
}

// Package-level instances, configured at startup from env (nil = disabled).
var (
	econCalendar    *EconCalendar
	sentimentClient *SentimentClient
)

// scoreSentiment uses the external API when configured, else the lexicon.
func scoreSentiment(ctx context.Context, text string) float64 {
	if sentimentClient != nil {
		if s, ok := sentimentClient.Score(ctx, text); ok {
			return s
		}
	}
	return headlineSentiment(text)
}
