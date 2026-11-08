package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// twitterSearchURL is the X API v2 recent-search endpoint (requires a bearer
// token with read access — Basic tier or higher).
const twitterSearchURL = "https://api.twitter.com/2/tweets/search/recent"

// costPerTweet is the approximate X API metered price per returned tweet, used
// only for budget logging. The hard control is the tweets/day budget.
const costPerTweet = 0.005

// twitterBudget is the persisted daily spend counter (so restarts don't reset it
// and blow through the credit budget).
type twitterBudget struct {
	Day      string `json:"day"`      // UTC date, YYYY-MM-DD
	Consumed int    `json:"consumed"` // tweets billed so far today
}

// TwitterConfig configures the Twitter/X ingestion source.
type TwitterConfig struct {
	Simulated    bool
	BearerToken  string
	Query        string // static fallback query (used when not market-driven, or no catalog)
	PollInterval time.Duration
	MaxResults   int
	DailyBudget  int    // max billed tweets per UTC day (0 = unlimited)
	BudgetFile   string // optional path to persist the daily counter across restarts
	MarketDriven bool   // build the query from the popular-market catalog in Redis
	TopMarkets   int    // how many top-volume markets to target
	SkipTop      int    // skip the N most-liquid (most efficient) markets
	Redis        *redis.Client
}

type TwitterSource struct {
	simulated    bool
	bearerToken  string
	query        string // static fallback
	pollInterval time.Duration
	maxResults   int
	dailyBudget  int    // max billed tweets per UTC day (0 = unlimited)
	budgetFile   string // optional path to persist the daily counter across restarts
	marketDriven bool
	topMarkets   int
	skipTop      int
	rdb          *redis.Client
	client       *http.Client
	sinceID      string // newest tweet id seen, for incremental polling

	budget          twitterBudget
	exhaustedLogged bool
	lastQuery       string // last query actually sent, to log only on change

	// metrics is a live snapshot for the /healthz status surface (read from the
	// monitor's HTTP goroutine, written by the poll goroutine — guarded by mu).
	mu          sync.Mutex
	tweetsSeen  int64     // tweets returned by the API since start
	lastTweetAt time.Time // when the most recent tweet was ingested
	rateLimited bool      // last poll hit a 429 (cleared on the next clean poll)
	lastPollAt  time.Time // when the last poll completed
	lastError   string    // last non-rate-limit poll error (empty when healthy)
	budgetUsed  int       // tweets billed today (snapshot of budget.Consumed under mu)
}

// recordPoll updates the health snapshot after a poll attempt: rate-limit state,
// last error (empty clears it), completion time, and today's budget spend. Called
// from the poll goroutine; Metrics reads these under the same lock.
func (t *TwitterSource) recordPoll(rateLimited bool, errMsg string) {
	t.mu.Lock()
	t.rateLimited = rateLimited
	t.lastError = errMsg
	t.lastPollAt = time.Now()
	t.budgetUsed = t.budget.Consumed
	t.mu.Unlock()
}

// TwitterMetrics is a point-in-time view of the Twitter/X source for the status
// dashboard. Zero-value fields are fine when the source is simulated or idle.
type TwitterMetrics struct {
	Live         bool      `json:"live"`
	BudgetUsed   int       `json:"budget_used"`   // tweets billed today
	DailyBudget  int       `json:"daily_budget"`  // 0 = unlimited
	TweetsSeen   int64     `json:"tweets_seen"`   // tweets ingested since start
	LastTweetAt  time.Time `json:"last_tweet_at"` // zero = none yet
	LastPollAt   time.Time `json:"last_poll_at"`  // zero = never polled
	RateLimited  bool      `json:"rate_limited"`  // last poll was 429'd
	LastError    string    `json:"last_error"`    // last poll error, empty when healthy
	PollInterval string    `json:"poll_interval"`
}

// Metrics returns a snapshot of the source's live health for the status page.
func (t *TwitterSource) Metrics() TwitterMetrics {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TwitterMetrics{
		Live:         !t.simulated,
		BudgetUsed:   t.budgetUsed,
		DailyBudget:  t.dailyBudget,
		TweetsSeen:   t.tweetsSeen,
		LastTweetAt:  t.lastTweetAt,
		LastPollAt:   t.lastPollAt,
		RateLimited:  t.rateLimited,
		LastError:    t.lastError,
		PollInterval: t.pollInterval.String(),
	}
}

// NewTwitterSource builds the Twitter/X source. In live mode it polls the X API
// v2 recent-search endpoint with the bearer token, throttled to DailyBudget tweets
// per day; in simulated mode it emits mock tweets. When MarketDriven, the query is
// rebuilt each poll from the popular-market catalog (falling back to Query).
func NewTwitterSource(cfg TwitterConfig) *TwitterSource {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 90 * time.Second
	}
	if cfg.MaxResults < 10 {
		cfg.MaxResults = 10 // API minimum
	}
	if cfg.MaxResults > 100 {
		cfg.MaxResults = 100 // API maximum
	}
	if cfg.TopMarkets <= 0 {
		cfg.TopMarkets = 20
	}
	return &TwitterSource{
		simulated:    cfg.Simulated,
		bearerToken:  cfg.BearerToken,
		query:        cfg.Query,
		pollInterval: cfg.PollInterval,
		maxResults:   cfg.MaxResults,
		dailyBudget:  cfg.DailyBudget,
		budgetFile:   cfg.BudgetFile,
		marketDriven: cfg.MarketDriven,
		topMarkets:   cfg.TopMarkets,
		skipTop:      cfg.SkipTop,
		rdb:          cfg.Redis,
		client:       &http.Client{Timeout: 15 * time.Second},
	}
}

// effectiveQuery returns the query to send: the popular-market query when
// market-driven and a catalog is available, otherwise the static fallback.
func (t *TwitterSource) effectiveQuery(ctx context.Context) string {
	if !t.marketDriven || t.rdb == nil {
		return t.query
	}
	markets := fetchTopMarkets(ctx, t.rdb, t.topMarkets, t.skipTop)
	return buildMarketQuery(markets, t.query, 480, 2)
}

type MockTweet struct {
	Username  string    `json:"username"`
	Text      string    `json:"text"`
	TweetID   string    `json:"tweet_id"`
	Likes     int       `json:"likes"`
	Retweets  int       `json:"retweets"`
	Timestamp time.Time `json:"timestamp"`
}

// twitterSearchResponse maps the fields we use from the X API v2 search payload.
type twitterSearchResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Text          string `json:"text"`
		AuthorID      string `json:"author_id"`
		CreatedAt     string `json:"created_at"`
		PublicMetrics struct {
			LikeCount    int `json:"like_count"`
			RetweetCount int `json:"retweet_count"`
		} `json:"public_metrics"`
	} `json:"data"`
	Includes struct {
		Users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"users"`
	} `json:"includes"`
	Meta struct {
		NewestID    string `json:"newest_id"`
		ResultCount int    `json:"result_count"`
	} `json:"meta"`
}

func (t *TwitterSource) Start(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if t.simulated {
		return t.startSimulation(ctx, output)
	}
	return t.startLive(ctx, output)
}

func (t *TwitterSource) startLive(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if t.bearerToken == "" {
		log.Println("[WARNING] Twitter LIVE mode requested but no bearer token configured; source idle.")
		<-ctx.Done()
		return nil
	}
	t.loadBudget()

	// Pace polls so the daily tweet budget spreads across the whole UTC day rather
	// than burning in the first hour (interval is never faster than the configured
	// floor). The daily cap in poll() is the hard guarantee regardless.
	interval := t.pollInterval
	if t.dailyBudget > 0 {
		pollsPerDay := t.dailyBudget / t.maxResults
		if pollsPerDay < 1 {
			pollsPerDay = 1
		}
		if paced := 24 * time.Hour / time.Duration(pollsPerDay); paced > interval {
			interval = paced
		}
	}
	mode := "static query"
	if t.marketDriven && t.rdb != nil {
		mode = fmt.Sprintf("market-driven (top %d markets by volume, skip %d)", t.topMarkets, t.skipTop)
	}
	log.Printf("[INFO] Twitter Ingestion Source started in LIVE mode: budget %d tweets/day (~$%.2f/day at $%.4f/tweet), poll every %v, %d/req, targeting %s",
		t.dailyBudget, float64(t.dailyBudget)*costPerTweet, costPerTweet, interval, t.maxResults, mode)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	t.poll(ctx, output)
	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Twitter Ingestion Source shutting down...")
			return nil
		case <-ticker.C:
			t.poll(ctx, output)
		}
	}
}

// poll runs one X API v2 recent-search request and emits new tweets. Errors are
// logged and swallowed so the loop keeps running (next tick retries).
func (t *TwitterSource) poll(ctx context.Context, output chan<- *GekkoIngestEvent) {
	// Budget gate: never exceed the daily tweet allowance (the cost control).
	reqMax := t.maxResults
	if t.dailyBudget > 0 {
		t.rolloverBudget()
		remaining := t.dailyBudget - t.budget.Consumed
		if remaining <= 0 {
			if !t.exhaustedLogged {
				log.Printf("[INFO] Twitter daily budget reached (%d tweets ≈ $%.2f); pausing until UTC rollover.",
					t.dailyBudget, float64(t.dailyBudget)*costPerTweet)
				t.exhaustedLogged = true
			}
			return
		}
		if remaining < reqMax {
			reqMax = remaining
		}
		if reqMax < 10 {
			reqMax = 10 // API minimum; tiny (<$0.05) overshoot on the final poll of the day
		}
	}

	// Rebuild the query from the popular-market catalog each poll so it tracks the
	// current top markets (since_id stays valid across query changes).
	query := t.effectiveQuery(ctx)
	if query == "" {
		log.Println("[WARNING] Twitter: empty query (no catalog and no fallback); skipping poll.")
		return
	}
	if query != t.lastQuery {
		log.Printf("[INFO] Twitter query updated: %s", query)
		t.lastQuery = query
	}

	q := url.Values{}
	q.Set("query", query)
	q.Set("max_results", strconv.Itoa(reqMax))
	q.Set("tweet.fields", "created_at,public_metrics")
	q.Set("expansions", "author_id")
	q.Set("user.fields", "username")
	if t.sinceID != "" {
		q.Set("since_id", t.sinceID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, twitterSearchURL+"?"+q.Encode(), nil)
	if err != nil {
		log.Printf("[ERROR] Twitter build request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+t.bearerToken)

	resp, err := t.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Twitter fetch failed: %v", err)
		t.recordPoll(false, err.Error())
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		log.Println("[WARNING] Twitter rate limited (429) — retrying next interval. Consider a larger TWITTER_POLL_INTERVAL.")
		t.recordPoll(true, "")
		return
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		log.Printf("[ERROR] Twitter API %d (token lacks recent-search access? Basic tier+ required): %s", resp.StatusCode, body)
		t.recordPoll(false, fmt.Sprintf("API %d", resp.StatusCode))
		return
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		log.Printf("[ERROR] Twitter API status %d: %s", resp.StatusCode, body)
		t.recordPoll(false, fmt.Sprintf("API %d", resp.StatusCode))
		return
	}

	var sr twitterSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		log.Printf("[ERROR] Twitter decode failed: %v", err)
		return
	}
	if sr.Meta.NewestID != "" {
		t.sinceID = sr.Meta.NewestID // advance the incremental cursor
	}
	// Bill the returned tweets against the daily budget and persist.
	if t.dailyBudget > 0 && sr.Meta.ResultCount > 0 {
		t.budget.Consumed += sr.Meta.ResultCount
		t.saveBudget()
	}
	// A clean poll clears any prior rate-limit/error state and records ingest.
	t.recordPoll(false, "")
	if sr.Meta.ResultCount > 0 {
		t.mu.Lock()
		t.tweetsSeen += int64(sr.Meta.ResultCount)
		t.lastTweetAt = time.Now()
		t.mu.Unlock()
	}

	usernames := make(map[string]string, len(sr.Includes.Users))
	for _, u := range sr.Includes.Users {
		usernames[u.ID] = u.Username
	}

	emitted := 0
	for _, tw := range sr.Data {
		username := usernames[tw.AuthorID]
		if username == "" {
			username = "unknown"
		}
		ts := time.Now()
		if parsed, err := time.Parse(time.RFC3339, tw.CreatedAt); err == nil {
			ts = parsed
		}

		payload := MockTweet{
			Username:  "@" + username,
			Text:      tw.Text,
			TweetID:   tw.ID,
			Likes:     tw.PublicMetrics.LikeCount,
			Retweets:  tw.PublicMetrics.RetweetCount,
			Timestamp: ts,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			continue
		}

		event := &GekkoIngestEvent{
			SourceID:             SourceTwitter,
			RawContent:           string(raw),
			SourceURL:            fmt.Sprintf("https://twitter.com/%s/status/%s", username, tw.ID),
			ExtractedTimestampNS: ts.UnixNano(),
		}
		event.PayloadHash = event.ComputeHash()

		select {
		case output <- event:
			emitted++
		case <-ctx.Done():
			return
		default:
			log.Println("[WARNING] ingest channel full, dropping live tweet")
		}
	}
	if emitted > 0 {
		log.Printf("[INFO] Twitter: %d new tweet(s)", emitted)
	}
}

// rolloverBudget resets the daily counter when the UTC day changes.
func (t *TwitterSource) rolloverBudget() {
	today := time.Now().UTC().Format("2006-01-02")
	if t.budget.Day != today {
		t.budget = twitterBudget{Day: today, Consumed: 0}
		t.exhaustedLogged = false
		t.saveBudget()
	}
}

// loadBudget restores today's spend from the budget file (if configured), so a
// restart doesn't reset the day's consumption. Stale (previous-day) state is
// dropped by rolloverBudget on the next poll.
func (t *TwitterSource) loadBudget() {
	today := time.Now().UTC().Format("2006-01-02")
	t.budget = twitterBudget{Day: today, Consumed: 0}
	if t.budgetFile == "" {
		return
	}
	data, err := os.ReadFile(t.budgetFile)
	if err != nil {
		return
	}
	var b twitterBudget
	if json.Unmarshal(data, &b) == nil && b.Day == today {
		t.budget = b
		log.Printf("[INFO] Twitter budget restored: %d/%d tweets already used today.", b.Consumed, t.dailyBudget)
	}
}

func (t *TwitterSource) saveBudget() {
	if t.budgetFile == "" {
		return
	}
	if data, err := json.Marshal(t.budget); err == nil {
		_ = os.WriteFile(t.budgetFile, data, 0o644)
	}
}

func (t *TwitterSource) startSimulation(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()

	handles := []string{"@elonmusk", "@VitalikButerin", "@SecYellen", "@KobeTrader"}
	tweets := []string{
		"Prediction markets represent the cleanest aggregation of real-world probabilities. Kahuna is looking good.",
		"What are the scaling implications for high-throughput clob on L2 layers? Researching Gamma.",
		"Macro indicators are pointing toward lower volatility profiles in Q3. We will monitor closely.",
		"Just loaded up on more Kalshi YES contracts for the Fed Rate decision. Easy money!",
	}

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	log.Println("[INFO] Twitter Ingestion Source started in SIMULATED mode")

	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Twitter Ingestion Source shutting down...")
			return nil
		case <-ticker.C:
			idx := rnd.Intn(len(tweets))
			username := handles[rnd.Intn(len(handles))]
			tweetID := fmt.Sprintf("%d", 1800000000000000000+rnd.Int63n(100000000000000))

			tweet := MockTweet{
				Username:  username,
				Text:      tweets[idx],
				TweetID:   tweetID,
				Likes:     rnd.Intn(50000),
				Retweets:  rnd.Intn(10000),
				Timestamp: time.Now(),
			}

			raw, err := json.Marshal(tweet)
			if err != nil {
				log.Printf("[ERROR] Twitter marshal error: %v", err)
				continue
			}

			event := &GekkoIngestEvent{
				SourceID:             SourceTwitter,
				RawContent:           string(raw),
				SourceURL:            fmt.Sprintf("https://twitter.com/%s/status/%s", username[1:], tweetID),
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
