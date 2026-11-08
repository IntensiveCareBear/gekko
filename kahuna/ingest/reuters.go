package ingest

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"time"
)

type ReutersSource struct {
	feeds        []string
	simulated    bool
	pollInterval time.Duration
	client       *http.Client
	state        map[string]*feedState
}

// feedState tracks per-feed polling state for dedupe, conditional GETs and backoff.
type feedState struct {
	etag         string
	lastModified string
	seen         map[string]bool // item link -> already emitted
	seenOrder    []string        // FIFO for bounded eviction
	failures     int             // consecutive failures (drives backoff)
	nextEligible time.Time       // earliest next poll (backoff gate)
}

const maxSeenPerFeed = 500

// NewReutersSource builds a keyless RSS news source. feeds is a list of RSS URLs
// (must be on compliance-authorized domains, e.g. news.google.com / reuters.com).
func NewReutersSource(feeds []string, simulated bool, pollInterval time.Duration) *ReutersSource {
	if pollInterval <= 0 {
		pollInterval = 60 * time.Second
	}
	state := make(map[string]*feedState, len(feeds))
	for _, f := range feeds {
		state[f] = &feedState{seen: make(map[string]bool)}
	}
	return &ReutersSource{
		feeds:        feeds,
		simulated:    simulated,
		pollInterval: pollInterval,
		client:       &http.Client{Timeout: 10 * time.Second},
		state:        state,
	}
}

// RSS definitions for standard XML parsing
type RssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Channel RssChannel `xml:"channel"`
}

type RssChannel struct {
	Title string    `xml:"title"`
	Link  string    `xml:"link"`
	Items []RssItem `xml:"item"`
}

type RssItem struct {
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Link        string `xml:"link"`
	PubDate     string `xml:"pubDate"`
}

func (r *ReutersSource) Start(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if r.simulated {
		return r.startSimulation(ctx, output)
	}
	return r.startLive(ctx, output)
}

func (r *ReutersSource) startSimulation(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	headlines := []string{
		"Federal Reserve signals potential interest rate cuts next quarter",
		"Tech stocks rally as market indexes touch all-time high",
		"Oil prices fluctuate amid shifting global supply forecasts",
		"Bitcoin approaches key milestone as trading volume surges",
	}

	bodies := []string{
		"The Federal Reserve committee met today, suggesting rate adjustment paths depending on inflation targets.",
		"Major technology companies drove Nasdaq and S&P 500 up today on strong corporate earnings statements.",
		"Crude benchmarks registered volatility following statements from OPEC representatives regarding export levels.",
		"Cryptocurrency markets showed significant movement with increased activity across decentralized and spot exchanges.",
	}

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	log.Println("[INFO] Reuters RSS Ingestion Source started in SIMULATED mode")

	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Reuters RSS Ingestion Source shutting down...")
			return nil
		case <-ticker.C:
			idx := rnd.Intn(len(headlines))
			pubTime := time.Now().Format(time.RFC1123Z)

			// Construct simulated RSS item
			item := RssItem{
				Title:       headlines[idx],
				Description: bodies[idx],
				Link:        fmt.Sprintf("https://www.reuters.com/business/economy/market-update-%d", rnd.Int63()),
				PubDate:     pubTime,
			}

			raw, err := xml.Marshal(item)
			if err != nil {
				log.Printf("[ERROR] Reuters RSS XML marshal error: %v", err)
				continue
			}

			event := &GekkoIngestEvent{
				SourceID:             SourceReuters,
				RawContent:           string(raw),
				SourceURL:            item.Link,
				ExtractedTimestampNS: time.Now().UnixNano(),
			}
			event.PayloadHash = event.ComputeHash()

			select {
			case output <- event:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func (r *ReutersSource) startLive(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if len(r.feeds) == 0 {
		log.Println("[WARNING] Reuters RSS source has no feeds configured; nothing to poll.")
		<-ctx.Done()
		return nil
	}

	// Tick frequently; each feed is gated by its own pollInterval + backoff window.
	tick := r.pollInterval / 4
	if tick < 5*time.Second {
		tick = 5 * time.Second
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	log.Printf("[INFO] RSS news source started in LIVE mode: %d feed(s), poll interval %v", len(r.feeds), r.pollInterval)
	r.pollDue(ctx, output)

	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] RSS news source shutting down...")
			return nil
		case <-ticker.C:
			r.pollDue(ctx, output)
		}
	}
}

// pollDue polls each feed that is past its next-eligible time.
func (r *ReutersSource) pollDue(ctx context.Context, output chan<- *GekkoIngestEvent) {
	now := time.Now()
	for _, feed := range r.feeds {
		st := r.state[feed]
		if now.Before(st.nextEligible) {
			continue
		}
		r.pollFeed(ctx, feed, st, output)
	}
}

func (r *ReutersSource) pollFeed(ctx context.Context, url string, st *feedState, output chan<- *GekkoIngestEvent) {
	// Default next poll is one interval out; failures push it further via backoff.
	defer func() {
		if st.failures > 0 {
			backoff := r.pollInterval * time.Duration(1<<minInt(st.failures, 5)) // cap at 32×
			st.nextEligible = time.Now().Add(backoff)
		} else {
			st.nextEligible = time.Now().Add(r.pollInterval)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		st.failures++
		log.Printf("[ERROR] RSS build request failed for %s: %v", url, err)
		return
	}
	// Conditional GET to be polite and save bandwidth/quota.
	if st.etag != "" {
		req.Header.Set("If-None-Match", st.etag)
	}
	if st.lastModified != "" {
		req.Header.Set("If-Modified-Since", st.lastModified)
	}
	req.Header.Set("User-Agent", "GekkoNewsBot/1.0 (+rss)")

	resp, err := r.client.Do(req)
	if err != nil {
		st.failures++
		log.Printf("[ERROR] RSS fetch failed for %s: %v", url, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		st.failures = 0 // healthy, just nothing new
		return
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		st.failures++
		log.Printf("[WARNING] RSS %s returned %d — backing off (%d consecutive)", url, resp.StatusCode, st.failures)
		return
	}
	if resp.StatusCode != http.StatusOK {
		st.failures++
		log.Printf("[ERROR] RSS %s unexpected status %d", url, resp.StatusCode)
		return
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		st.failures++
		return
	}

	var feed RssFeed
	if err := xml.Unmarshal(bodyBytes, &feed); err != nil {
		st.failures++
		log.Printf("[ERROR] RSS %s XML parse failed: %v", url, err)
		return
	}

	// Success: store validators and reset backoff.
	st.failures = 0
	st.etag = resp.Header.Get("ETag")
	st.lastModified = resp.Header.Get("Last-Modified")

	emitted := 0
	for _, item := range feed.Channel.Items {
		dedupeKey := item.Link
		if dedupeKey == "" {
			dedupeKey = item.Title
		}
		if st.seen[dedupeKey] {
			continue // already emitted this item
		}
		r.markSeen(st, dedupeKey)

		itemTime := time.Now()
		if parsed, err := time.Parse(time.RFC1123, item.PubDate); err == nil {
			itemTime = parsed
		} else if parsed, err := time.Parse(time.RFC1123Z, item.PubDate); err == nil {
			itemTime = parsed
		} else if parsed, err := time.Parse(time.RFC3339, item.PubDate); err == nil {
			itemTime = parsed
		}

		raw, err := xml.Marshal(item)
		if err != nil {
			continue
		}

		event := &GekkoIngestEvent{
			SourceID:             SourceReuters,
			RawContent:           string(raw),
			SourceURL:            item.Link,
			ExtractedTimestampNS: itemTime.UnixNano(),
		}
		event.PayloadHash = event.ComputeHash()

		select {
		case output <- event:
			emitted++
		case <-ctx.Done():
			return
		default:
			log.Println("[WARNING] News output channel full, dropping live RSS item")
		}
	}
	if emitted > 0 {
		log.Printf("[INFO] RSS %s: %d new item(s)", url, emitted)
	}
}

// markSeen records a dedupe key with bounded FIFO eviction.
func (r *ReutersSource) markSeen(st *feedState, key string) {
	st.seen[key] = true
	st.seenOrder = append(st.seenOrder, key)
	if len(st.seenOrder) > maxSeenPerFeed {
		old := st.seenOrder[0]
		st.seenOrder = st.seenOrder[1:]
		delete(st.seen, old)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
