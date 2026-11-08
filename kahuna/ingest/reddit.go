package ingest

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Reddit OAuth + API endpoints. We authenticate application-only (client
// credentials) against www.reddit.com and then read listings from
// oauth.reddit.com. Emitted events are attributed to the post permalink on
// www.reddit.com (allowlisted); the OAuth/API hosts are pull-only and never
// appear as an event SourceURL. Vars (not consts) so tests can point them at a
// mock server.
var (
	redditTokenURL = "https://www.reddit.com/api/v1/access_token"
	redditAPIBase  = "https://oauth.reddit.com"
	// redditRSSBase is the public web host serving the keyless .rss (Atom) feeds.
	redditRSSBase = "https://www.reddit.com"
)

const maxSeenReddit = 2000

// RedditConfig configures the Reddit ingestion source. Live when a client id +
// secret are present; otherwise idle. Reddit's free non-commercial tier allows
// ~100 queries/min per OAuth client — far more headroom than X — and is not
// metered per item, so there is no dollar budget, only a calls/min pacing guard.
type RedditConfig struct {
	Simulated      bool
	ClientID       string
	ClientSecret   string
	RSS            bool          // keyless mode: read the public .rss (Atom) feeds instead of the OAuth API
	UserAgent      string        // Reddit requires a unique, descriptive UA
	Subreddits     []string      // e.g. ["economics","cryptocurrency","wallstreetbets"]
	Keywords       []string      // optional: keep only posts whose title/body match one (empty = keep all)
	PollInterval   time.Duration // how often to poll each subreddit's /new listing
	MaxResults     int           // posts per subreddit per poll (1..100)
	MaxCallsPerMin int           // free tier is ~100/min; default well under it
}

// RedditPost is the structured payload emitted per post, mirroring the shape of
// MockTweet/FinnhubArticle so downstream embedding sees title + body + source.
type RedditPost struct {
	Subreddit   string    `json:"subreddit"`
	Author      string    `json:"author"`
	Title       string    `json:"title"`
	Selftext    string    `json:"selftext"` // truncated body (link/image posts have none)
	Score       int       `json:"score"`    // net upvotes (weak engagement prior)
	NumComments int       `json:"num_comments"`
	Permalink   string    `json:"permalink"`
	Timestamp   time.Time `json:"timestamp"`
}

// redditListing maps the fields we use from a /new listing response.
type redditListing struct {
	Data struct {
		Children []struct {
			Data struct {
				ID          string  `json:"id"`
				Name        string  `json:"name"` // fullname, e.g. "t3_abc123"
				Title       string  `json:"title"`
				Selftext    string  `json:"selftext"`
				Author      string  `json:"author"`
				Subreddit   string  `json:"subreddit"`
				Permalink   string  `json:"permalink"`
				Score       int     `json:"score"`
				NumComments int     `json:"num_comments"`
				CreatedUTC  float64 `json:"created_utc"`
				Over18      bool    `json:"over_18"`
				Stickied    bool    `json:"stickied"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type redditTokenResp struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// maxSelftextLen bounds the emitted body so a long post can't bloat the event or
// the embedding input; the title carries most of the signal anyway.
const maxSelftextLen = 1200

type RedditSource struct {
	simulated      bool
	clientID       string
	clientSecret   string
	useRSS         bool
	userAgent      string
	subreddits     []string
	keywords       []string
	pollInterval   time.Duration
	maxResults     int
	minCallSpacing time.Duration
	client         *http.Client

	lastCallAt time.Time       // last API call, for rate pacing (poll goroutine only)
	seen       map[string]bool // post fullname -> already emitted
	seenOrder  []string        // FIFO for bounded eviction

	// 429 backoff state (poll goroutine only, no mutex). A fixed ticker used to
	// re-poll at full rate through a 429, which just earned another 429 forever
	// (2,850 warnings / 14h on 2026-07-12). Now a rate-limited cycle skips whole
	// ticks, doubling the skip each consecutive 429 and resetting on a clean cycle.
	sawRateLimit bool      // any subreddit 429'd this cycle (reset at cycle start)
	backoffLevel int       // consecutive rate-limited cycles (0 = healthy)
	backoffUntil time.Time // suppress polling until this time

	token       string    // cached bearer token
	tokenExpiry time.Time // when the cached token expires

	// metrics snapshot for the /healthz External APIs card (written by the poll
	// goroutine, read by the monitor HTTP goroutine — guarded by mu).
	mu          sync.Mutex
	postsSeen   int64
	lastPostAt  time.Time
	lastPollAt  time.Time
	rateLimited bool
	lastError   string
}

// NewRedditSource builds the Reddit source. Live when client credentials are
// present; otherwise idle.
func NewRedditSource(cfg RedditConfig) *RedditSource {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 90 * time.Second
	}
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = 25
	}
	if cfg.MaxResults > 100 {
		cfg.MaxResults = 100 // API max per listing page
	}
	if cfg.MaxCallsPerMin <= 0 {
		cfg.MaxCallsPerMin = 60 // free tier is ~100/min; leave headroom
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "gekko-kahuna/1.0 (prediction-market research)"
	}
	if len(cfg.Subreddits) == 0 {
		cfg.Subreddits = []string{"economics", "finance", "cryptocurrency", "wallstreetbets", "stocks"}
	}
	return &RedditSource{
		simulated:      cfg.Simulated,
		clientID:       cfg.ClientID,
		clientSecret:   cfg.ClientSecret,
		useRSS:         cfg.RSS,
		userAgent:      cfg.UserAgent,
		subreddits:     cfg.Subreddits,
		keywords:       lowerAll(cfg.Keywords),
		pollInterval:   cfg.PollInterval,
		maxResults:     cfg.MaxResults,
		minCallSpacing: time.Minute / time.Duration(cfg.MaxCallsPerMin),
		client:         &http.Client{Timeout: 15 * time.Second},
		seen:           make(map[string]bool),
	}
}

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.ToLower(strings.TrimSpace(s)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// RedditMetrics is a point-in-time view of the source for the status dashboard.
type RedditMetrics struct {
	Live           bool      `json:"live"`
	PostsSeen      int64     `json:"posts_seen"`
	LastPostAt     time.Time `json:"last_post_at"`
	LastPollAt     time.Time `json:"last_poll_at"`
	RateLimited    bool      `json:"rate_limited"`
	LastError      string    `json:"last_error"`
	PollInterval   string    `json:"poll_interval"`
	Subreddits     string    `json:"subreddits"`
	MaxCallsPerMin int       `json:"max_calls_per_min"`
}

// Metrics returns a snapshot of the source's live health for the status page.
func (r *RedditSource) Metrics() RedditMetrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RedditMetrics{
		Live:           !r.simulated,
		PostsSeen:      r.postsSeen,
		LastPostAt:     r.lastPostAt,
		LastPollAt:     r.lastPollAt,
		RateLimited:    r.rateLimited,
		LastError:      r.lastError,
		PollInterval:   r.pollInterval.String(),
		Subreddits:     strings.Join(r.subreddits, ","),
		MaxCallsPerMin: int(time.Minute / r.minCallSpacing),
	}
}

func (r *RedditSource) recordPoll(rateLimited bool, errMsg string) {
	r.mu.Lock()
	r.rateLimited = rateLimited
	r.lastError = errMsg
	r.lastPollAt = time.Now()
	r.mu.Unlock()
}

func (r *RedditSource) Start(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if r.simulated {
		log.Println("[INFO] Reddit source is SIMULATED/idle; nothing to poll.")
		<-ctx.Done()
		return nil
	}

	// Prefer the OAuth API when credentials are present (richer metadata: score,
	// comment count). Otherwise fall back to the keyless public .rss (Atom) feeds —
	// no app registration required, which matters since Reddit closed self-serve API
	// signup in 2026 (see docs/data-sourcing-and-credibility-proposal.md).
	oauth := r.clientID != "" && r.clientSecret != ""
	if !oauth && !r.useRSS {
		log.Println("[WARNING] Reddit LIVE requested but no credentials and RSS disabled; source idle.")
		<-ctx.Done()
		return nil
	}
	mode, pollOne := "OAuth API", r.pollSubreddit
	if !oauth {
		mode, pollOne = "keyless RSS", r.pollSubredditRSS
	}

	log.Printf("[INFO] Reddit Ingestion Source started in LIVE mode (%s): subreddits %v, poll every %v, ≤%d calls/min",
		mode, r.subreddits, r.pollInterval, int(time.Minute/r.minCallSpacing))

	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	r.pollAll(ctx, output, pollOne)
	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Reddit Ingestion Source shutting down...")
			return nil
		case <-ticker.C:
			if time.Now().Before(r.backoffUntil) {
				continue // still backing off from an earlier 429; skip this tick
			}
			r.pollAll(ctx, output, pollOne)
		}
	}
}

// maxRedditBackoffLevel caps the exponential skip at 2^6 = 64 ticks (~96 min at
// the default 90s interval) so the source keeps probing for recovery.
const maxRedditBackoffLevel = 6

// pollAll fetches each configured subreddit once via pollOne, paced under the
// calls/min limit. A 429 from any subreddit aborts the rest of the cycle (no point
// hammering the other subreddits under the same limit) and schedules an exponential
// backoff; a clean cycle clears it.
func (r *RedditSource) pollAll(ctx context.Context, output chan<- *GekkoIngestEvent, pollOne func(context.Context, string, chan<- *GekkoIngestEvent)) {
	r.sawRateLimit = false
	for _, sub := range r.subreddits {
		if ctx.Err() != nil {
			return
		}
		r.throttle(ctx)
		pollOne(ctx, sub, output)
		if r.sawRateLimit {
			break // rate limited — stop the cycle, let the backoff below take over
		}
	}
	if r.sawRateLimit {
		if r.backoffLevel < maxRedditBackoffLevel {
			r.backoffLevel++
		}
		skip := r.pollInterval * time.Duration(int64(1)<<r.backoffLevel)
		r.backoffUntil = time.Now().Add(skip)
		log.Printf("[WARNING] Reddit rate limited (429) — backing off %v (level %d). Raise REDDIT_POLL_INTERVAL or reduce REDDIT_SUBREDDITS.",
			skip.Round(time.Second), r.backoffLevel)
		return
	}
	r.backoffLevel = 0
	r.backoffUntil = time.Time{}
}

// throttle enforces the minimum spacing between API calls so multiple subreddits
// can't exceed the free-tier calls/min limit.
func (r *RedditSource) throttle(ctx context.Context) {
	if wait := r.minCallSpacing - time.Since(r.lastCallAt); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	r.lastCallAt = time.Now()
}

// ensureToken returns a valid bearer token, refreshing via the client-credentials
// OAuth flow when the cache is empty or near expiry.
func (r *RedditSource) ensureToken(ctx context.Context) (string, error) {
	if r.token != "" && time.Now().Before(r.tokenExpiry.Add(-60*time.Second)) {
		return r.token, nil
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, redditTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(r.clientID, r.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", r.userAgent)

	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("reddit token endpoint %d: %s", resp.StatusCode, body)
	}
	var tr redditTokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("reddit token decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("reddit token endpoint returned empty access_token")
	}
	r.token = tr.AccessToken
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	r.tokenExpiry = time.Now().Add(ttl)
	return r.token, nil
}

func (r *RedditSource) pollSubreddit(ctx context.Context, sub string, output chan<- *GekkoIngestEvent) {
	token, err := r.ensureToken(ctx)
	if err != nil {
		log.Printf("[ERROR] Reddit auth failed: %v", err)
		r.recordPoll(false, "auth: "+err.Error())
		return
	}

	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", r.maxResults))
	endpoint := fmt.Sprintf("%s/r/%s/new?%s", redditAPIBase, url.PathEscape(sub), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		log.Printf("[ERROR] Reddit build request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", r.userAgent)

	resp, err := r.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Reddit fetch failed: %v", err)
		r.recordPoll(false, err.Error())
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		r.sawRateLimit = true // pollAll schedules the exponential backoff and logs it
		r.recordPoll(true, "")
		return
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		r.token = "" // force re-auth next poll
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Printf("[ERROR] Reddit API %d (token expired/invalid?): %s", resp.StatusCode, body)
		r.recordPoll(false, fmt.Sprintf("API %d", resp.StatusCode))
		return
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Printf("[ERROR] Reddit API status %d: %s", resp.StatusCode, body)
		r.recordPoll(false, fmt.Sprintf("API %d", resp.StatusCode))
		return
	}

	var listing redditListing
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		log.Printf("[ERROR] Reddit decode failed: %v", err)
		r.recordPoll(false, "decode error")
		return
	}
	r.recordPoll(false, "") // clean poll

	emitted := 0
	for _, child := range listing.Data.Children {
		d := child.Data
		if d.Name == "" || d.Title == "" {
			continue
		}
		if d.Over18 || d.Stickied {
			continue // skip NSFW and pinned/mod posts (announcements, not signal)
		}
		if r.seen[d.Name] {
			continue
		}
		if !r.matchesKeywords(d.Title, d.Selftext) {
			continue
		}
		r.markSeen(d.Name)

		ts := time.Now()
		if d.CreatedUTC > 0 {
			ts = time.Unix(int64(d.CreatedUTC), 0)
		}
		body := d.Selftext
		if len(body) > maxSelftextLen {
			body = body[:maxSelftextLen]
		}
		payload := RedditPost{
			Subreddit:   d.Subreddit,
			Author:      "u/" + d.Author,
			Title:       d.Title,
			Selftext:    body,
			Score:       d.Score,
			NumComments: d.NumComments,
			Permalink:   d.Permalink,
			Timestamp:   ts,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		event := &GekkoIngestEvent{
			SourceID:             SourceReddit,
			RawContent:           string(raw),
			SourceURL:            "https://www.reddit.com" + d.Permalink,
			ExtractedTimestampNS: ts.UnixNano(),
		}
		event.PayloadHash = event.ComputeHash()

		select {
		case output <- event:
			emitted++
		case <-ctx.Done():
			return
		default:
			log.Println("[WARNING] ingest channel full, dropping Reddit post")
		}
	}
	if emitted > 0 {
		r.mu.Lock()
		r.postsSeen += int64(emitted)
		r.lastPostAt = time.Now()
		r.mu.Unlock()
		log.Printf("[INFO] Reddit r/%s: %d new post(s)", sub, emitted)
	}
}

// redditAtomFeed maps the fields we use from a subreddit's public .rss (Atom) feed.
type redditAtomFeed struct {
	XMLName xml.Name          `xml:"feed"`
	Entries []redditAtomEntry `xml:"entry"`
}

type redditAtomEntry struct {
	ID     string `xml:"id"` // "t3_xxxx" fullname (same key space as the API)
	Title  string `xml:"title"`
	Author struct {
		Name string `xml:"name"` // "/u/username"
	} `xml:"author"`
	Link struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
	Category struct {
		Term string `xml:"term,attr"`
	} `xml:"category"`
	Published string `xml:"published"`
	Updated   string `xml:"updated"`
}

// pollSubredditRSS reads a subreddit's keyless public .rss (Atom) feed and emits
// new posts. This path carries no score/comment metadata (the feed doesn't include
// it), but needs no OAuth app — the fallback for Reddit's 2026 API lockdown. Reddit
// rate-limits the feed aggressively (429s), so a 429 just backs off to the next tick.
func (r *RedditSource) pollSubredditRSS(ctx context.Context, sub string, output chan<- *GekkoIngestEvent) {
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", r.maxResults))
	endpoint := fmt.Sprintf("%s/r/%s/new/.rss?%s", redditRSSBase, url.PathEscape(sub), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		log.Printf("[ERROR] Reddit RSS build request: %v", err)
		return
	}
	req.Header.Set("User-Agent", r.userAgent) // required — the default Go UA is 429'd

	resp, err := r.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Reddit RSS fetch failed: %v", err)
		r.recordPoll(false, err.Error())
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		r.sawRateLimit = true // pollAll schedules the exponential backoff and logs it
		r.recordPoll(true, "")
		return
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Printf("[ERROR] Reddit RSS status %d: %s", resp.StatusCode, body)
		r.recordPoll(false, fmt.Sprintf("RSS %d", resp.StatusCode))
		return
	}

	var feed redditAtomFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		log.Printf("[ERROR] Reddit RSS parse failed: %v", err)
		r.recordPoll(false, "parse error")
		return
	}
	r.recordPoll(false, "") // clean poll

	emitted := 0
	for _, e := range feed.Entries {
		if e.ID == "" || e.Title == "" || e.Link.Href == "" {
			continue
		}
		if r.seen[e.ID] {
			continue
		}
		if !r.matchesKeywords(e.Title, "") { // feed carries no usable body
			continue
		}
		r.markSeen(e.ID)

		ts := time.Now()
		if t, err := time.Parse(time.RFC3339, firstNonEmpty(e.Published, e.Updated)); err == nil {
			ts = t
		}
		payload := RedditPost{
			Subreddit: strings.TrimPrefix(e.Category.Term, "r/"),
			Author:    strings.TrimPrefix(e.Author.Name, "/"), // "/u/x" -> "u/x"
			Title:     e.Title,
			Permalink: e.Link.Href,
			Timestamp: ts,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		event := &GekkoIngestEvent{
			SourceID:             SourceReddit,
			RawContent:           string(raw),
			SourceURL:            e.Link.Href, // already a www.reddit.com permalink
			ExtractedTimestampNS: ts.UnixNano(),
		}
		event.PayloadHash = event.ComputeHash()

		select {
		case output <- event:
			emitted++
		case <-ctx.Done():
			return
		default:
			log.Println("[WARNING] ingest channel full, dropping Reddit RSS post")
		}
	}
	if emitted > 0 {
		r.mu.Lock()
		r.postsSeen += int64(emitted)
		r.lastPostAt = time.Now()
		r.mu.Unlock()
		log.Printf("[INFO] Reddit r/%s (RSS): %d new post(s)", sub, emitted)
	}
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// matchesKeywords reports whether the post should be kept: true when no keyword
// filter is configured, else true if the title or body contains any keyword.
func (r *RedditSource) matchesKeywords(title, body string) bool {
	if len(r.keywords) == 0 {
		return true
	}
	hay := strings.ToLower(title + " " + body)
	for _, kw := range r.keywords {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

// markSeen records a post fullname with bounded FIFO eviction.
func (r *RedditSource) markSeen(name string) {
	r.seen[name] = true
	r.seenOrder = append(r.seenOrder, name)
	if len(r.seenOrder) > maxSeenReddit {
		old := r.seenOrder[0]
		r.seenOrder = r.seenOrder[1:]
		delete(r.seen, old)
	}
}
