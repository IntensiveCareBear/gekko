package monitor

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"gekko/kahuna/ingest"
)

//go:embed dashboard.html
var dashboardHTML string

type EventLog struct {
	Timestamp   time.Time       `json:"timestamp"`
	SourceID    ingest.SourceID `json:"source_id"`
	SourceURL   string          `json:"source_url"`
	Status      string          `json:"status"` // "PROCESSED" or "QUARANTINED"
	Reason      string          `json:"reason,omitempty"`
	PayloadHash string          `json:"payload_hash"`
	RawContent  string          `json:"raw_content"`
	Length      int             `json:"length"`
}

type SourceMetrics struct {
	ReceivedCount    int64      `json:"received_count"`
	ProcessedCount   int64      `json:"processed_count"`
	QuarantinedCount int64      `json:"quarantined_count"`
	LastEventTime    *time.Time `json:"last_event_time"`
}

type SystemStats struct {
	NumGoroutine int    `json:"num_goroutine"`
	AllocMB      uint64 `json:"alloc_mb"`
	SysMB        uint64 `json:"sys_mb"`
	NumGC        uint32 `json:"num_gc"`
	GoVersion    string `json:"go_version"`
}

type StatsResponse struct {
	UptimeSeconds    float64                            `json:"uptime_seconds"`
	TotalReceived    int64                              `json:"total_received"`
	TotalProcessed   int64                              `json:"total_processed"`
	TotalQuarantined int64                              `json:"total_quarantined"`
	SourceMetrics    map[ingest.SourceID]*SourceMetrics `json:"source_metrics"`
	RecentEvents     []EventLog                         `json:"recent_events"`
	System           SystemStats                        `json:"system"`
}

// twitterReporter is the slice of the Twitter source the monitor needs for the
// status surface (kept as an interface so the monitor doesn't own the source).
type twitterReporter interface {
	Metrics() ingest.TwitterMetrics
}

// finnhubReporter is the equivalent slice of the Finnhub news source.
type finnhubReporter interface {
	Metrics() ingest.FinnhubMetrics
}

// fredReporter is the slice of the FRED source the monitor needs: health metrics
// plus the release-schedule calendar it serves for Gordon's econ blackout.
type fredReporter interface {
	Metrics() ingest.FREDMetrics
	CalendarJSON() []byte
}

// redditReporter is the equivalent slice of the Reddit source.
type redditReporter interface {
	Metrics() ingest.RedditMetrics
}

// weatherReporter is the equivalent slice of the Weather (METAR) source.
type weatherReporter interface {
	Metrics() ingest.WeatherMetrics
}

type Monitor struct {
	mu               sync.RWMutex
	startTime        time.Time
	totalReceived    int64
	totalProcessed   int64
	totalQuarantined int64
	sourceMetrics    map[ingest.SourceID]*SourceMetrics
	recentEvents     []EventLog
	maxRecentEvents  int
	twitter          twitterReporter // nil when Twitter isn't an enabled source
	finnhub          finnhubReporter // nil when Finnhub isn't an enabled source
	fred             fredReporter    // nil when FRED isn't an enabled source
	reddit           redditReporter  // nil when Reddit isn't an enabled source
	weather          weatherReporter // nil when Weather isn't an enabled source
}

// SetTwitterSource registers the live Twitter/X source so /healthz can surface
// its budget and ingest health for the frontend External APIs card.
func (m *Monitor) SetTwitterSource(t twitterReporter) {
	m.mu.Lock()
	m.twitter = t
	m.mu.Unlock()
}

// SetFinnhubSource registers the live Finnhub news source for the status surface.
func (m *Monitor) SetFinnhubSource(f finnhubReporter) {
	m.mu.Lock()
	m.finnhub = f
	m.mu.Unlock()
}

// SetFREDSource registers the live FRED source for the status surface and the
// /econ_calendar.json feed Gordon consumes.
func (m *Monitor) SetFREDSource(f fredReporter) {
	m.mu.Lock()
	m.fred = f
	m.mu.Unlock()
}

// SetRedditSource registers the live Reddit source for the status surface.
func (m *Monitor) SetRedditSource(r redditReporter) {
	m.mu.Lock()
	m.reddit = r
	m.mu.Unlock()
}

// SetWeatherSource registers the live Weather (METAR) source for the status surface.
func (m *Monitor) SetWeatherSource(w weatherReporter) {
	m.mu.Lock()
	m.weather = w
	m.mu.Unlock()
}

func NewMonitor() *Monitor {
	return &Monitor{
		startTime:       time.Now(),
		sourceMetrics:   make(map[ingest.SourceID]*SourceMetrics),
		recentEvents:    make([]EventLog, 0),
		maxRecentEvents: 50,
	}
}

func (m *Monitor) RecordEvent(event *ingest.GekkoIngestEvent, status string, reason string) {
	if event == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	m.totalReceived++
	if status == "PROCESSED" {
		m.totalProcessed++
	} else if status == "QUARANTINED" {
		m.totalQuarantined++
	}

	sm, ok := m.sourceMetrics[event.SourceID]
	if !ok {
		sm = &SourceMetrics{}
		m.sourceMetrics[event.SourceID] = sm
	}
	sm.ReceivedCount++
	if status == "PROCESSED" {
		sm.ProcessedCount++
	} else if status == "QUARANTINED" {
		sm.QuarantinedCount++
	}
	sm.LastEventTime = &now

	// Prepend to recent events list
	logEntry := EventLog{
		Timestamp:   now,
		SourceID:    event.SourceID,
		SourceURL:   event.SourceURL,
		Status:      status,
		Reason:      reason,
		PayloadHash: event.PayloadHash,
		RawContent:  event.RawContent,
		Length:      len(event.RawContent),
	}
	m.recentEvents = append([]EventLog{logEntry}, m.recentEvents...)
	if len(m.recentEvents) > m.maxRecentEvents {
		m.recentEvents = m.recentEvents[:m.maxRecentEvents]
	}
}

func (m *Monitor) StartServer(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", m.handleStats)
	mux.HandleFunc("/healthz", m.handleHealth)
	mux.HandleFunc("/econ_calendar.json", m.handleEconCalendar)
	mux.HandleFunc("/", m.handleIndex)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Graceful shutdown listener
	go func() {
		<-ctx.Done()
		log.Println("[INFO] Shutting down monitor HTTP server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("[ERROR] Monitor server shutdown failed: %v", err)
		}
	}()

	log.Printf("[INFO] Monitor GUI HTTP server listening on http://%s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (m *Monitor) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	m.mu.RLock()
	defer m.mu.RUnlock()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	response := StatsResponse{
		UptimeSeconds:    time.Since(m.startTime).Seconds(),
		TotalReceived:    m.totalReceived,
		TotalProcessed:   m.totalProcessed,
		TotalQuarantined: m.totalQuarantined,
		SourceMetrics:    m.sourceMetrics,
		RecentEvents:     m.recentEvents,
		System: SystemStats{
			NumGoroutine: runtime.NumGoroutine(),
			AllocMB:      memStats.Alloc / 1024 / 1024,
			SysMB:        memStats.Sys / 1024 / 1024,
			NumGC:        memStats.NumGC,
			GoVersion:    runtime.Version(),
		},
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("[ERROR] Failed to encode statistics JSON: %v", err)
	}
}

// handleHealth returns the common Gekko /healthz shape for the status dashboard.
func (m *Monitor) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := map[string]interface{}{
		"total_received":    m.totalReceived,
		"total_processed":   m.totalProcessed,
		"total_quarantined": m.totalQuarantined,
	}
	// Twitter/X ingest health — flattened into twitter_* keys so the frontend can
	// surface it as an External APIs card (present only when Twitter is enabled).
	if m.twitter != nil {
		tm := m.twitter.Metrics()
		stats["twitter_live"] = tm.Live
		stats["twitter_budget_used"] = tm.BudgetUsed
		stats["twitter_daily_budget"] = tm.DailyBudget
		stats["twitter_tweets_seen"] = tm.TweetsSeen
		stats["twitter_rate_limited"] = tm.RateLimited
		stats["twitter_poll_interval"] = tm.PollInterval
		if !tm.LastTweetAt.IsZero() {
			stats["twitter_last_tweet_unix"] = tm.LastTweetAt.Unix()
		}
		if !tm.LastPollAt.IsZero() {
			stats["twitter_last_poll_unix"] = tm.LastPollAt.Unix()
		}
		if tm.LastError != "" {
			stats["twitter_last_error"] = tm.LastError
		}
	}
	// Finnhub news ingest health — flattened into finnhub_* keys for its own External
	// APIs card (present only when Finnhub is enabled).
	if m.finnhub != nil {
		fm := m.finnhub.Metrics()
		stats["finnhub_live"] = fm.Live
		stats["finnhub_articles_seen"] = fm.ArticlesSeen
		stats["finnhub_rate_limited"] = fm.RateLimited
		stats["finnhub_poll_interval"] = fm.PollInterval
		stats["finnhub_categories"] = fm.Categories
		stats["finnhub_max_calls_per_min"] = fm.MaxCallsPerMin
		if !fm.LastArticleAt.IsZero() {
			stats["finnhub_last_article_unix"] = fm.LastArticleAt.Unix()
		}
		if !fm.LastPollAt.IsZero() {
			stats["finnhub_last_poll_unix"] = fm.LastPollAt.Unix()
		}
		if fm.LastError != "" {
			stats["finnhub_last_error"] = fm.LastError
		}
	}

	// FRED macro-data ingest health — flattened into fred_* keys for its own External
	// APIs card (present only when FRED is enabled).
	if m.fred != nil {
		rm := m.fred.Metrics()
		stats["fred_live"] = rm.Live
		stats["fred_releases_seen"] = rm.ReleasesSeen
		stats["fred_series"] = rm.Series
		stats["fred_poll_interval"] = rm.PollInterval
		stats["fred_calendar_events"] = rm.CalendarEvents
		stats["fred_max_calls_per_min"] = rm.MaxCallsPerMin
		if rm.CalendarError != "" {
			stats["fred_calendar_error"] = rm.CalendarError
		}
		if rm.LastRelease != "" {
			stats["fred_last_release"] = rm.LastRelease
		}
		if !rm.LastReleaseAt.IsZero() {
			stats["fred_last_release_unix"] = rm.LastReleaseAt.Unix()
		}
		if !rm.LastPollAt.IsZero() {
			stats["fred_last_poll_unix"] = rm.LastPollAt.Unix()
		}
		if rm.LastError != "" {
			stats["fred_last_error"] = rm.LastError
		}
	}

	// Reddit social ingest health — flattened into reddit_* keys for its own External
	// APIs card (present only when Reddit is enabled).
	if m.reddit != nil {
		dm := m.reddit.Metrics()
		stats["reddit_live"] = dm.Live
		stats["reddit_posts_seen"] = dm.PostsSeen
		stats["reddit_rate_limited"] = dm.RateLimited
		stats["reddit_poll_interval"] = dm.PollInterval
		stats["reddit_subreddits"] = dm.Subreddits
		stats["reddit_max_calls_per_min"] = dm.MaxCallsPerMin
		if !dm.LastPostAt.IsZero() {
			stats["reddit_last_post_unix"] = dm.LastPostAt.Unix()
		}
		if !dm.LastPollAt.IsZero() {
			stats["reddit_last_poll_unix"] = dm.LastPollAt.Unix()
		}
		if dm.LastError != "" {
			stats["reddit_last_error"] = dm.LastError
		}
	}

	// Weather (METAR) ingest health — flattened into weather_* keys for its own
	// External APIs card (present only when Weather is enabled).
	if m.weather != nil {
		wm := m.weather.Metrics()
		stats["weather_live"] = wm.Live
		stats["weather_obs_seen"] = wm.ObsSeen
		stats["weather_poll_interval"] = wm.PollInterval
		stats["weather_stations"] = wm.Stations
		stats["weather_max_calls_per_min"] = wm.MaxCallsPerMin
		// Flatten per-station highs into individual numeric keys (a nested object here
		// would crash the dashboard's generic stat renderer, which only handles
		// primitives).
		for station, high := range wm.DayHighs {
			stats["weather_high_"+station] = high
		}
		if !wm.LastObsAt.IsZero() {
			stats["weather_last_obs_unix"] = wm.LastObsAt.Unix()
		}
		if !wm.LastPollAt.IsZero() {
			stats["weather_last_poll_unix"] = wm.LastPollAt.Unix()
		}
		if wm.LastError != "" {
			stats["weather_last_error"] = wm.LastError
		}
	}

	resp := map[string]interface{}{
		"component":      "kahuna",
		"status":         "ok",
		"uptime_seconds": time.Since(m.startTime).Seconds(),
		"stats":          stats,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[ERROR] Failed to encode health JSON: %v", err)
	}
}

// handleEconCalendar serves the FRED-derived release schedule in Gordon's
// EconEvent shape (JSON array). Point Gordon's ECON_CALENDAR_URL here so its
// blackout window is driven by live official release dates. Empty array when FRED
// isn't enabled or hasn't built a calendar yet (Gordon treats that as no blackout).
func (m *Monitor) handleEconCalendar(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	m.mu.RLock()
	fred := m.fred
	m.mu.RUnlock()

	if fred == nil {
		_, _ = w.Write([]byte("[]"))
		return
	}
	_, _ = w.Write(fred.CalendarJSON())
}

func (m *Monitor) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	ua := r.Header.Get("User-Agent")
	if strings.Contains(strings.ToLower(ua), "curl") || strings.Contains(strings.ToLower(ua), "wget") {
		m.serveCLIDashboard(w)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := fmt.Fprint(w, dashboardHTML); err != nil {
		log.Printf("[ERROR] Failed to write index response: %v", err)
	}
}

func (m *Monitor) serveCLIDashboard(w http.ResponseWriter) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	uptime := time.Since(m.startTime).Round(time.Second)

	fmt.Fprintf(w, "\033[1;36m====================================================\033[0m\n")
	fmt.Fprintf(w, "       \033[1;35mKAHUNA INGESTION WORKER MONITOR (CLI)\033[0m\n")
	fmt.Fprintf(w, "\033[1;36m====================================================\033[0m\n")
	fmt.Fprintf(w, " Uptime:       %v\n", uptime)
	fmt.Fprintf(w, " Total Events: %d\n", m.totalReceived)
	fmt.Fprintf(w, " Processed:    \033[1;32m%d\033[0m\n", m.totalProcessed)
	fmt.Fprintf(w, " Quarantined:  \033[1;31m%d\033[0m\n", m.totalQuarantined)
	fmt.Fprintf(w, "\033[1;36m----------------------------------------------------\033[0m\n")
	fmt.Fprintf(w, " \033[1;33mSource Stats:\033[0m\n")

	// Print in consistent order of SourceIDs
	sources := []ingest.SourceID{ingest.SourceKalshi, ingest.SourcePolymarket, ingest.SourceReuters, ingest.SourceTwitter}
	for _, id := range sources {
		sm, ok := m.sourceMetrics[id]
		lastSeen := "never"
		if ok && sm.LastEventTime != nil {
			lastSeen = time.Since(*sm.LastEventTime).Round(time.Millisecond*100).String() + " ago"
		}
		var recv, proc, quar int64
		if ok {
			recv, proc, quar = sm.ReceivedCount, sm.ProcessedCount, sm.QuarantinedCount
		}
		fmt.Fprintf(w, "  %-12s: Recv=%-6d Proc=\033[1;32m%-6d\033[0m Quar=\033[1;31m%-6d\033[0m (Last: %s)\n",
			id, recv, proc, quar, lastSeen)
	}

	// Print any other dynamic source
	for id, sm := range m.sourceMetrics {
		known := false
		for _, kid := range sources {
			if kid == id {
				known = true
				break
			}
		}
		if !known {
			lastSeen := "never"
			if sm.LastEventTime != nil {
				lastSeen = time.Since(*sm.LastEventTime).Round(time.Millisecond*100).String() + " ago"
			}
			fmt.Fprintf(w, "  %-12s: Recv=%-6d Proc=\033[1;32m%-6d\033[0m Quar=\033[1;31m%-6d\033[0m (Last: %s)\n",
				id, sm.ReceivedCount, sm.ProcessedCount, sm.QuarantinedCount, lastSeen)
		}
	}

	fmt.Fprintf(w, "\033[1;36m----------------------------------------------------\033[0m\n")
	fmt.Fprintf(w, " \033[1;33mSystem Info:\033[0m\n")
	fmt.Fprintf(w, "  Goroutines:  %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "  Allocated:   %d MB\n", memStats.Alloc/1024/1024)
	fmt.Fprintf(w, "  Sys Memory:  %d MB\n", memStats.Sys/1024/1024)
	fmt.Fprintf(w, "  GC Runs:     %d\n", memStats.NumGC)
	fmt.Fprintf(w, "\033[1;36m====================================================\033[0m\n")
}
