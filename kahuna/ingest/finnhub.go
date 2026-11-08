package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// finnhubNewsURL is the Finnhub market-news endpoint (free tier). We authenticate
// with the token as a query param per Finnhub's docs; SourceURL on emitted events
// points at finnhub.io (the authorized source), never the token. A var (not const)
// so tests can point it at a mock server.
var finnhubNewsURL = "https://finnhub.io/api/v1/news"

// finnhubSourceRef is the compliance-authorized provenance host for emitted events.
// Finnhub aggregates many publishers whose domains aren't individually allowlisted,
// so we attribute the event to finnhub.io (the source we actually pull from) and
// carry the original publisher URL inside the payload.
const finnhubSourceRef = "https://finnhub.io"

const maxSeenFinnhub = 1000

// FinnhubConfig configures the Finnhub market-news ingestion source.
type FinnhubConfig struct {
	Simulated      bool
	APIKey         string
	Categories     []string      // e.g. ["general","crypto"]; one API call each per poll
	PollInterval   time.Duration // how often to poll each category
	MaxCallsPerMin int           // free-tier guard (Finnhub free = 60/min); paces HTTP calls
}

// FinnhubArticle is the structured payload emitted for each news item, mirroring the
// shape of MockTweet/RssItem so downstream embedding sees headline + summary + source.
type FinnhubArticle struct {
	Headline  string    `json:"headline"`
	Summary   string    `json:"summary"`
	Source    string    `json:"source"`   // original publisher (e.g. "Reuters", "CNBC")
	Category  string    `json:"category"` // Finnhub feed category
	URL       string    `json:"url"`      // original article URL (provenance; not the SourceURL)
	Timestamp time.Time `json:"timestamp"`
}

// finnhubNewsItem maps the fields we use from the Finnhub /news payload.
type finnhubNewsItem struct {
	Category string `json:"category"`
	Datetime int64  `json:"datetime"` // unix seconds
	Headline string `json:"headline"`
	ID       int64  `json:"id"`
	Source   string `json:"source"`
	Summary  string `json:"summary"`
	URL      string `json:"url"`
}

type FinnhubSource struct {
	simulated      bool
	apiKey         string
	categories     []string
	pollInterval   time.Duration
	minCallSpacing time.Duration // enforced gap between HTTP calls (free-tier pacing)
	client         *http.Client

	lastCallAt time.Time      // last HTTP call, for rate pacing (poll goroutine only)
	seen       map[int64]bool // article id -> already emitted
	seenOrder  []int64        // FIFO for bounded eviction

	// metrics snapshot for the /healthz External APIs card (written by the poll
	// goroutine, read by the monitor HTTP goroutine — guarded by mu).
	mu            sync.Mutex
	articlesSeen  int64
	lastArticleAt time.Time
	lastPollAt    time.Time
	rateLimited   bool
	lastError     string
}

// NewFinnhubSource builds the Finnhub news source. Live when an API key is present;
// otherwise idle. Categories with no explicit config default to general + crypto.
func NewFinnhubSource(cfg FinnhubConfig) *FinnhubSource {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 60 * time.Second
	}
	if cfg.MaxCallsPerMin <= 0 {
		cfg.MaxCallsPerMin = 30 // half of Finnhub's 60/min free limit, leaving headroom
	}
	if len(cfg.Categories) == 0 {
		cfg.Categories = []string{"general", "crypto"}
	}
	return &FinnhubSource{
		simulated:      cfg.Simulated,
		apiKey:         cfg.APIKey,
		categories:     cfg.Categories,
		pollInterval:   cfg.PollInterval,
		minCallSpacing: time.Minute / time.Duration(cfg.MaxCallsPerMin),
		client:         &http.Client{Timeout: 15 * time.Second},
		seen:           make(map[int64]bool),
	}
}

// FinnhubMetrics is a point-in-time view of the source for the status dashboard.
type FinnhubMetrics struct {
	Live           bool      `json:"live"`
	ArticlesSeen   int64     `json:"articles_seen"`
	LastArticleAt  time.Time `json:"last_article_at"`
	LastPollAt     time.Time `json:"last_poll_at"`
	RateLimited    bool      `json:"rate_limited"`
	LastError      string    `json:"last_error"`
	PollInterval   string    `json:"poll_interval"`
	Categories     string    `json:"categories"`
	MaxCallsPerMin int       `json:"max_calls_per_min"`
}

// Metrics returns a snapshot of the source's live health for the status page.
func (f *FinnhubSource) Metrics() FinnhubMetrics {
	f.mu.Lock()
	defer f.mu.Unlock()
	return FinnhubMetrics{
		Live:           !f.simulated,
		ArticlesSeen:   f.articlesSeen,
		LastArticleAt:  f.lastArticleAt,
		LastPollAt:     f.lastPollAt,
		RateLimited:    f.rateLimited,
		LastError:      f.lastError,
		PollInterval:   f.pollInterval.String(),
		Categories:     strings.Join(f.categories, ","),
		MaxCallsPerMin: int(time.Minute / f.minCallSpacing),
	}
}

func (f *FinnhubSource) recordPoll(rateLimited bool, errMsg string) {
	f.mu.Lock()
	f.rateLimited = rateLimited
	f.lastError = errMsg
	f.lastPollAt = time.Now()
	f.mu.Unlock()
}

func (f *FinnhubSource) Start(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if f.simulated {
		log.Println("[INFO] Finnhub source is SIMULATED/idle (no API key); nothing to poll.")
		<-ctx.Done()
		return nil
	}
	if f.apiKey == "" {
		log.Println("[WARNING] Finnhub LIVE mode requested but no API key configured; source idle.")
		<-ctx.Done()
		return nil
	}

	log.Printf("[INFO] Finnhub Ingestion Source started in LIVE mode: categories %v, poll every %v, ≤%d calls/min (free tier is 60/min)",
		f.categories, f.pollInterval, int(time.Minute/f.minCallSpacing))

	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()
	f.pollAll(ctx, output)
	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Finnhub Ingestion Source shutting down...")
			return nil
		case <-ticker.C:
			f.pollAll(ctx, output)
		}
	}
}

// pollAll fetches each configured category once, paced under the free-tier call limit.
func (f *FinnhubSource) pollAll(ctx context.Context, output chan<- *GekkoIngestEvent) {
	for _, cat := range f.categories {
		if ctx.Err() != nil {
			return
		}
		f.throttle(ctx)
		f.pollCategory(ctx, cat, output)
	}
}

// throttle enforces the minimum spacing between HTTP calls so a fast poll interval or
// multiple categories can't exceed the free-tier calls/min limit.
func (f *FinnhubSource) throttle(ctx context.Context) {
	if wait := f.minCallSpacing - time.Since(f.lastCallAt); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	f.lastCallAt = time.Now()
}

// redactKey renders err with every occurrence of the API key masked. net/http errors embed
// the full request URL, and these sources authenticate with a query parameter.
func redactKey(err error, key string) string {
	msg := err.Error()
	if key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, key, "REDACTED")
}

func (f *FinnhubSource) pollCategory(ctx context.Context, category string, output chan<- *GekkoIngestEvent) {
	q := url.Values{}
	q.Set("category", category)
	q.Set("token", f.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finnhubNewsURL+"?"+q.Encode(), nil)
	if err != nil {
		log.Printf("[ERROR] Finnhub build request: %v", err)
		return
	}

	resp, err := f.client.Do(req)
	if err != nil {
		msg := redactKey(err, f.apiKey)
		log.Printf("[ERROR] Finnhub fetch failed: %s", msg)
		f.recordPoll(false, msg)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		log.Println("[WARNING] Finnhub rate limited (429) — backing off. Lower FINNHUB_MAX_CALLS_PER_MIN or raise FINNHUB_POLL_INTERVAL.")
		f.recordPoll(true, "")
		return
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Printf("[ERROR] Finnhub API %d (bad/invalid API key?): %s", resp.StatusCode, body)
		f.recordPoll(false, fmt.Sprintf("API %d", resp.StatusCode))
		return
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Printf("[ERROR] Finnhub API status %d: %s", resp.StatusCode, body)
		f.recordPoll(false, fmt.Sprintf("API %d", resp.StatusCode))
		return
	}

	var items []finnhubNewsItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		log.Printf("[ERROR] Finnhub decode failed: %v", err)
		f.recordPoll(false, "decode error")
		return
	}
	f.recordPoll(false, "") // clean poll

	emitted := 0
	for _, it := range items {
		if it.ID == 0 || it.Headline == "" {
			continue
		}
		if f.seen[it.ID] {
			continue
		}
		f.markSeen(it.ID)

		ts := time.Now()
		if it.Datetime > 0 {
			ts = time.Unix(it.Datetime, 0)
		}
		payload := FinnhubArticle{
			Headline:  it.Headline,
			Summary:   it.Summary,
			Source:    it.Source,
			Category:  it.Category,
			URL:       it.URL,
			Timestamp: ts,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		event := &GekkoIngestEvent{
			SourceID:             SourceFinnhub,
			RawContent:           string(raw),
			SourceURL:            fmt.Sprintf("%s/#news-%d", finnhubSourceRef, it.ID),
			ExtractedTimestampNS: ts.UnixNano(),
		}
		event.PayloadHash = event.ComputeHash()

		select {
		case output <- event:
			emitted++
		case <-ctx.Done():
			return
		default:
			log.Println("[WARNING] ingest channel full, dropping Finnhub article")
		}
	}
	if emitted > 0 {
		f.mu.Lock()
		f.articlesSeen += int64(emitted)
		f.lastArticleAt = time.Now()
		f.mu.Unlock()
		log.Printf("[INFO] Finnhub %s: %d new article(s)", category, emitted)
	}
}

// markSeen records an article id with bounded FIFO eviction.
func (f *FinnhubSource) markSeen(id int64) {
	f.seen[id] = true
	f.seenOrder = append(f.seenOrder, id)
	if len(f.seenOrder) > maxSeenFinnhub {
		old := f.seenOrder[0]
		f.seenOrder = f.seenOrder[1:]
		delete(f.seen, old)
	}
}
