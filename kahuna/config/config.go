package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Default RSS feeds — all on compliance-authorized news.google.com so items pass
// the gatekeeper. Add other domains to kahuna/compliance/filter.go before using them.
const defaultNewsFeeds = "https://news.google.com/rss/search?q=federal+reserve+OR+inflation+OR+interest+rates&hl=en-US&gl=US&ceid=US:en," +
	"https://news.google.com/rss/search?q=stock+market+OR+S%26P+500+OR+nasdaq&hl=en-US&gl=US&ceid=US:en," +
	"https://news.google.com/rss/search?q=bitcoin+OR+crypto+OR+ethereum&hl=en-US&gl=US&ceid=US:en," +
	"https://news.google.com/rss/search?q=jobs+report+OR+unemployment+OR+CPI+OR+GDP&hl=en-US&gl=US&ceid=US:en," +
	"https://news.google.com/rss/search?q=election+OR+congress+OR+supreme+court&hl=en-US&gl=US&ceid=US:en," +
	"https://news.google.com/rss/search?q=geopolitics+OR+OPEC+OR+oil+prices&hl=en-US&gl=US&ceid=US:en"

// defaultTwitterQuery is an X API v2 recent-search query biased toward events
// that map to Kalshi/Polymarket contracts (macro, rates, crypto, politics).
// Override with TWITTER_QUERY. Keep under the tier's query-length limit (512 chars
// on Basic). Operators: https://developer.x.com/en/docs/twitter-api/tweets/search
const defaultTwitterQuery = `(Federal Reserve OR "interest rate" OR inflation OR CPI OR recession OR "jobs report" OR GDP OR Bitcoin OR Ethereum OR election OR "Supreme Court") -is:retweet -is:reply lang:en`

type Config struct {
	KafkaEnabled          bool
	KafkaBrokers          []string
	KafkaTopic            string
	LogLevel              string
	QuarantineDir         string
	QuarantineFallback    string
	KalshiWSURL           string
	KalshiSimulated       bool
	PolymarketWSURL       string
	PolymarketSimulated   bool
	ReutersRSSURL         string
	ReutersSimulated      bool
	NewsRSSFeeds          []string
	NewsPollInterval      time.Duration
	TwitterSimulated      bool
	TwitterBearerToken    string
	TwitterQuery          string
	TwitterPollInterval   time.Duration
	TwitterMaxResults     int
	TwitterDailyBudget    int
	TwitterBudgetFile     string
	TwitterMarketDriven   bool
	TwitterTopMarkets     int
	TwitterSkipTop        int
	FinnhubSimulated      bool
	FinnhubAPIKey         string
	FinnhubCategories     []string
	FinnhubPollInterval   time.Duration
	FinnhubMaxCallsPerMin int
	FREDSimulated         bool
	FREDAPIKey            string
	FREDSeries            []string
	FREDPollInterval      time.Duration
	FREDMaxCallsPerMin    int
	FREDCalendarDays      int
	RedditSimulated       bool
	RedditClientID        string
	RedditClientSecret    string
	RedditRSS             bool
	RedditUserAgent       string
	RedditSubreddits      []string
	RedditKeywords        []string
	RedditPollInterval    time.Duration
	RedditMaxResults      int
	RedditMaxCallsPerMin  int
	WeatherSimulated      bool
	WeatherStations       string // "ICAO:City:TZ" tuples, comma-separated; empty = default set
	WeatherPollInterval   time.Duration
	WeatherMaxCallsPerMin int
	WeatherLookbackHours  int
	// Phase 2 forecast-bias model (docs/weather-phase2-forecast-bias.md).
	WeatherForecastEnabled bool          // pull NWS daily-high forecast + accumulate the forward corpus
	WeatherForecastPoll    time.Duration // NWS refreshes a few times/day; hourly is ample
	WeatherForecastStore   string        // forecasts.json (latest forecast per station-date)
	WeatherHistoryFile     string        // history.jsonl (forward (forecast, realized) corpus)
	WeatherBiasFile        string        // bias.json (per-station bias table gordon reads)
	RedisAddr             string
	BufferCapacity        int
	MonitorEnabled        bool
	MonitorAddr           string
	RateLimit             int
	// When false (default), simulated ingestion sources are skipped so no synthetic
	// data enters the pipeline; only sources configured for live data run.
	AllowSimulatedSources bool
}

func LoadConfig() *Config {
	brokersEnv := getEnv("KAFKA_BROKERS", "localhost:9092")
	brokers := strings.Split(brokersEnv, ",")
	for i, b := range brokers {
		brokers[i] = strings.TrimSpace(b)
	}

	// Twitter goes live automatically when a bearer token is provided (inline or
	// via file); otherwise it stays simulated.
	twitterToken := readSecret(getEnv("TWITTER_BEARER_TOKEN", ""), getEnv("TWITTER_BEARER_TOKEN_FILE", ""))

	// Finnhub goes live automatically when an API key is provided. The default key
	// file is finhub.txt (gitignored) to match the operator's local setup.
	finnhubKey := readSecret(getEnv("FINNHUB_API_KEY", ""), getEnv("FINNHUB_API_KEY_FILE", "finhub.txt"))

	// FRED goes live automatically when an API key is provided (free key from
	// fredaccount.stlouisfed.org). Default key file is fred.txt (gitignored).
	fredKey := readSecret(getEnv("FRED_API_KEY", ""), getEnv("FRED_API_KEY_FILE", "fred.txt"))

	// Reddit: the OAuth API needs a client id + secret (inline env, or a gitignored
	// reddit.txt holding one line "client_id:client_secret"). Reddit closed self-serve
	// API app signup in 2026, so the keyless RSS mode (REDDIT_RSS, reads the public
	// .rss Atom feeds) is the default live path; credentials, when present, upgrade to
	// the richer API automatically.
	redditID, redditSecret := readRedditCreds(
		getEnv("REDDIT_CLIENT_ID", ""), getEnv("REDDIT_CLIENT_SECRET", ""),
		getEnv("REDDIT_CREDENTIALS_FILE", "reddit.txt"))
	redditRSS := getEnvBool("REDDIT_RSS", false)
	redditHasCreds := redditID != "" && redditSecret != ""

	return &Config{
		KafkaEnabled:        getEnvBool("KAFKA_ENABLED", false),
		KafkaBrokers:        brokers,
		KafkaTopic:          getEnv("KAFKA_TOPIC", "kahuna.ingest.public"),
		LogLevel:            getEnv("LOG_LEVEL", "info"),
		QuarantineDir:       getEnv("QUARANTINE_DIR", "/var/log/gekko/quarantine"),
		QuarantineFallback:  getEnv("QUARANTINE_FALLBACK", "./quarantine"),
		KalshiWSURL:         getEnv("KALSHI_WS_URL", "wss://api.kalshi.com/v2/market_lifecycle_v2"),
		KalshiSimulated:     getEnvBool("KALSHI_SIMULATED", true),
		PolymarketWSURL:     getEnvBoolWS("POLYMARKET_WS_URL", "wss://clob.polymarket.com/ws"),
		PolymarketSimulated: getEnvBool("POLYMARKET_SIMULATED", true),
		ReutersRSSURL:       getEnv("REUTERS_RSS_URL", "https://news.google.com/rss/search?q=reuters&hl=en-US&gl=US&ceid=US:en"), // Using a standard public RSS for fallback
		ReutersSimulated:    getEnvBool("REUTERS_SIMULATED", true),
		NewsRSSFeeds:        splitCSV(getEnv("NEWS_RSS_FEEDS", defaultNewsFeeds)),
		NewsPollInterval:    getEnvDuration("NEWS_POLL_INTERVAL", 60*time.Second),
		TwitterSimulated:    getEnvBool("TWITTER_SIMULATED", twitterToken == ""),
		TwitterBearerToken:  twitterToken,
		TwitterQuery:        getEnv("TWITTER_QUERY", defaultTwitterQuery),
		TwitterPollInterval: getEnvDuration("TWITTER_POLL_INTERVAL", 90*time.Second),
		TwitterMaxResults:   getEnvInt("TWITTER_MAX_RESULTS", 25),
		// Cost control: cap billed tweets/day. ~$0.005/tweet, so 1500/day ≈ $7.50/day.
		// The daily cap also paces polling (24h ÷ budget/max_results), so a bigger
		// budget = fresher data. Raise for fresher data, lower to stretch spend.
		TwitterDailyBudget: getEnvInt("TWITTER_DAILY_TWEET_BUDGET", 1500),
		TwitterBudgetFile:  getEnv("TWITTER_BUDGET_FILE", ""),
		// Target what's popular: build the query from the top volume-ranked markets
		// (skip the most-liquid/efficient via TWITTER_SKIP_TOP_MARKETS). Falls back to
		// TWITTER_QUERY when the catalog is unavailable.
		TwitterMarketDriven: getEnvBool("TWITTER_MARKET_DRIVEN", true),
		TwitterTopMarkets:   getEnvInt("TWITTER_TOP_MARKETS", 20),
		TwitterSkipTop:      getEnvInt("TWITTER_SKIP_TOP_MARKETS", 0),
		FinnhubSimulated:    getEnvBool("FINNHUB_SIMULATED", finnhubKey == ""),
		FinnhubAPIKey:       finnhubKey,
		FinnhubCategories:   splitCSV(getEnv("FINNHUB_NEWS_CATEGORIES", "general,forex,crypto,merger")),
		FinnhubPollInterval: getEnvDuration("FINNHUB_POLL_INTERVAL", 60*time.Second),
		// Free tier is 60 calls/min; default well under it (30/min) for headroom.
		FinnhubMaxCallsPerMin: getEnvInt("FINNHUB_MAX_CALLS_PER_MIN", 30),
		FREDSimulated:         getEnvBool("FRED_SIMULATED", fredKey == ""),
		FREDAPIKey:            fredKey,
		// Macro basket mapped to the Fed/CPI/GDP/jobs markets Gekko trades.
		FREDSeries:         splitCSV(getEnv("FRED_SERIES", "CPIAUCSL,CPILFESL,PCEPI,UNRATE,PAYEMS,ICSA,FEDFUNDS,DGS10,DGS2,T10Y2Y,GDPC1,VIXCLS,UMCSENT")),
		FREDPollInterval:   getEnvDuration("FRED_POLL_INTERVAL", 30*time.Minute),
		FREDMaxCallsPerMin: getEnvInt("FRED_MAX_CALLS_PER_MIN", 60),
		FREDCalendarDays:   getEnvInt("FRED_CALENDAR_DAYS", 14),
		// Live (not simulated/idle) when it has either OAuth creds or RSS enabled.
		RedditSimulated:    getEnvBool("REDDIT_SIMULATED", !(redditHasCreds || redditRSS)),
		RedditClientID:     redditID,
		RedditClientSecret: redditSecret,
		RedditRSS:          redditRSS,
		RedditUserAgent:    getEnv("REDDIT_USER_AGENT", "gekko-kahuna/1.0 (prediction-market research)"),
		// Topical subreddits mapped to the macro/crypto/politics markets Gekko trades.
		RedditSubreddits: splitCSV(getEnv("REDDIT_SUBREDDITS",
			"economics,finance,cryptocurrency,wallstreetbets,stocks,politics")),
		// Optional keyword filter (empty = keep every post from the curated subs).
		RedditKeywords:     splitCSV(getEnv("REDDIT_KEYWORDS", "")),
		RedditPollInterval: getEnvDuration("REDDIT_POLL_INTERVAL", 90*time.Second),
		RedditMaxResults:   getEnvInt("REDDIT_MAX_RESULTS", 25),
		// Gentle pacing (~4s between subreddit calls): the keyless RSS feed 429s
		// aggressively and there's no upside to hammering it (dedup recovers next cycle).
		RedditMaxCallsPerMin:  getEnvInt("REDDIT_MAX_CALLS_PER_MIN", 15),
		// Weather (METAR) is keyless, so "live" is just a toggle — default idle/dark
		// until deliberately enabled (WEATHER_SIMULATED=false) alongside the weather
		// strategy. Stations default to the full liquid Kalshi high-temp set.
		WeatherSimulated:      getEnvBool("WEATHER_SIMULATED", true),
		WeatherStations:       getEnv("WEATHER_STATIONS", ""),
		WeatherPollInterval:   getEnvDuration("WEATHER_POLL_INTERVAL", 10*time.Minute),
		WeatherMaxCallsPerMin: getEnvInt("WEATHER_MAX_CALLS_PER_MIN", 15),
		WeatherLookbackHours:  getEnvInt("WEATHER_LOOKBACK_HOURS", 26),
		// Phase 2 forecast source + forward corpus. Defaults on when the weather stack
		// is live (WEATHER_SIMULATED=false); the model self-gates and lands dark.
		WeatherForecastEnabled: getEnvBool("WEATHER_FORECAST_ENABLED", !getEnvBool("WEATHER_SIMULATED", true)),
		WeatherForecastPoll:    getEnvDuration("WEATHER_FORECAST_POLL", time.Hour),
		WeatherForecastStore:   getEnv("WEATHER_FORECAST_STORE", "runs/weather/forecasts.json"),
		WeatherHistoryFile:     getEnv("WEATHER_HISTORY_FILE", "runs/weather/history.jsonl"),
		WeatherBiasFile:        getEnv("WEATHER_BIAS_FILE", "runs/weather/bias.json"),
		RedisAddr:             getEnv("REDIS_ADDR", "localhost:6379"),
		BufferCapacity:        getEnvInt("BUFFER_CAPACITY", 1000), // Max capacity of events buffer
		MonitorEnabled:        getEnvBool("MONITOR_ENABLED", true),
		MonitorAddr:           getEnv("MONITOR_ADDR", ":8081"),
		RateLimit:             getEnvInt("RATE_LIMIT", 0),
		AllowSimulatedSources: getEnvBool("ALLOW_SIMULATED_SOURCES", false),
	}
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

// readSecret returns a trimmed secret from an inline value, else from a file path
// (e.g. twitter.txt). Empty string when neither is set or the file is unreadable.
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

// readRedditCreds resolves the Reddit client id + secret. Inline env wins; else
// the file is read as a single "client_id:client_secret" line (whitespace and a
// trailing newline are trimmed). Returns empty strings when unavailable, which
// keeps the source simulated/idle.
func readRedditCreds(inlineID, inlineSecret, file string) (id, secret string) {
	id = strings.TrimSpace(inlineID)
	secret = strings.TrimSpace(inlineSecret)
	if id != "" && secret != "" {
		return id, secret
	}
	if file == "" {
		return id, secret
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return id, secret
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, ':'); i >= 0 {
		if id == "" {
			id = strings.TrimSpace(line[:i])
		}
		if secret == "" {
			secret = strings.TrimSpace(line[i+1:])
		}
	}
	return id, secret
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

func getEnvBoolWS(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
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

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return defaultVal
}

// splitCSV splits a comma list, trimming whitespace and dropping empties.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
