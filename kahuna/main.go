package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gekko/kahuna/compliance"
	"gekko/kahuna/config"
	"gekko/kahuna/ingest"
	"gekko/kahuna/kafka"
	"gekko/kahuna/monitor"

	"github.com/redis/go-redis/v9"
)

func main() {
	log.Println("[INFO] Initializing Kahuna Ingestion Worker Engine...")

	// 1. Load Configurations
	cfg := config.LoadConfig()
	log.Printf("[INFO] Loaded configuration. Kafka Enabled: %v | Log Level: %s", cfg.KafkaEnabled, cfg.LogLevel)

	// 2. Setup Compliance Gatekeeper & Quarantiner
	gatekeeper := compliance.NewGatekeeper()
	quarantiner := compliance.NewQuarantiner(cfg.QuarantineDir, cfg.QuarantineFallback)

	// 3. Setup Kafka Producer (Fallback to ConsoleProducer if disabled)
	var producer kafka.Producer
	if cfg.KafkaEnabled {
		log.Printf("[INFO] Initializing production Kafka producer targeting: %v (Topic: %s)", cfg.KafkaBrokers, cfg.KafkaTopic)
		producer = kafka.NewKafkaProducer(cfg.KafkaBrokers, cfg.KafkaTopic)
	} else {
		log.Println("[INFO] Kafka publishing is disabled. Falling back to Console Producer.")
		producer = kafka.NewConsoleProducer()
	}
	defer func() {
		log.Println("[INFO] Closing Kafka/Console producer...")
		if err := producer.Close(); err != nil {
			log.Printf("[ERROR] Failed to close producer: %v", err)
		}
	}()

	// 4. Lifecycle management with context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize & start monitor GUI if enabled
	var mon *monitor.Monitor
	if cfg.MonitorEnabled {
		mon = monitor.NewMonitor()
		go func() {
			if err := mon.StartServer(ctx, cfg.MonitorAddr); err != nil {
				log.Printf("[ERROR] Monitor server encountered error: %v", err)
			}
		}()
	}

	// 5. Initialize Ingestion Sources. Simulated sources are skipped by default so
	// no synthetic data enters the pipeline (set ALLOW_SIMULATED_SOURCES=true for dev).
	// Redis client for market-driven Twitter targeting (reads the popular-market
	// catalogs). Only needed when Twitter is live and market-driven.
	var twitterRedis *redis.Client
	if !cfg.TwitterSimulated && cfg.TwitterMarketDriven {
		twitterRedis = redis.NewClient(&redis.Options{
			Addr:        cfg.RedisAddr,
			DialTimeout: 2 * time.Second,
			ReadTimeout: 2 * time.Second,
		})
	}

	twitterSrc := ingest.NewTwitterSource(ingest.TwitterConfig{
		Simulated:    cfg.TwitterSimulated,
		BearerToken:  cfg.TwitterBearerToken,
		Query:        cfg.TwitterQuery,
		PollInterval: cfg.TwitterPollInterval,
		MaxResults:   cfg.TwitterMaxResults,
		DailyBudget:  cfg.TwitterDailyBudget,
		BudgetFile:   cfg.TwitterBudgetFile,
		MarketDriven: cfg.TwitterMarketDriven,
		TopMarkets:   cfg.TwitterTopMarkets,
		SkipTop:      cfg.TwitterSkipTop,
		Redis:        twitterRedis,
	})

	finnhubSrc := ingest.NewFinnhubSource(ingest.FinnhubConfig{
		Simulated:      cfg.FinnhubSimulated,
		APIKey:         cfg.FinnhubAPIKey,
		Categories:     cfg.FinnhubCategories,
		PollInterval:   cfg.FinnhubPollInterval,
		MaxCallsPerMin: cfg.FinnhubMaxCallsPerMin,
	})

	fredSrc := ingest.NewFREDSource(ingest.FREDConfig{
		Simulated:      cfg.FREDSimulated,
		APIKey:         cfg.FREDAPIKey,
		Series:         cfg.FREDSeries,
		PollInterval:   cfg.FREDPollInterval,
		MaxCallsPerMin: cfg.FREDMaxCallsPerMin,
		CalendarDays:   cfg.FREDCalendarDays,
	})

	redditSrc := ingest.NewRedditSource(ingest.RedditConfig{
		Simulated:      cfg.RedditSimulated,
		ClientID:       cfg.RedditClientID,
		ClientSecret:   cfg.RedditClientSecret,
		RSS:            cfg.RedditRSS,
		UserAgent:      cfg.RedditUserAgent,
		Subreddits:     cfg.RedditSubreddits,
		Keywords:       cfg.RedditKeywords,
		PollInterval:   cfg.RedditPollInterval,
		MaxResults:     cfg.RedditMaxResults,
		MaxCallsPerMin: cfg.RedditMaxCallsPerMin,
	})

	weatherStations := ingest.ParseWeatherStations(cfg.WeatherStations)
	weatherSrc := ingest.NewWeatherSource(ingest.WeatherConfig{
		Simulated:      cfg.WeatherSimulated,
		Stations:       weatherStations,
		PollInterval:   cfg.WeatherPollInterval,
		MaxCallsPerMin: cfg.WeatherMaxCallsPerMin,
		LookbackHours:  cfg.WeatherLookbackHours,
	})

	// Phase 2 forecast-bias model: the NWS forecast source records the daily-high
	// forecast per station; the METAR source finalizes the realized high at each local
	// day rollover and joins them into the forward corpus, from which a per-station bias
	// table is recomputed for gordon's predictive pricing. All keyless, self-gating.
	var forecastSrc *ingest.WeatherForecastSource
	if cfg.WeatherForecastEnabled && !cfg.WeatherSimulated {
		forecastStore := ingest.NewForecastStore(cfg.WeatherForecastStore)
		corpus := ingest.NewCorpusRecorder(cfg.WeatherHistoryFile, cfg.WeatherBiasFile, forecastStore)
		weatherSrc.SetCorpusRecorder(corpus)
		forecastSrc = ingest.NewWeatherForecastSource(ingest.WeatherForecastConfig{
			Enabled:        true,
			Stations:       weatherStations,
			PollInterval:   cfg.WeatherForecastPoll,
			MaxCallsPerMin: cfg.WeatherMaxCallsPerMin,
			Store:          forecastStore,
		})
	}

	candidates := []struct {
		name      string
		simulated bool
		src       ingest.IngestionSource
	}{
		{"kalshi", cfg.KalshiSimulated, ingest.NewKalshiSource(cfg.KalshiWSURL, cfg.KalshiSimulated)},
		{"polymarket", cfg.PolymarketSimulated, ingest.NewPolymarketSource(cfg.PolymarketWSURL, cfg.PolymarketSimulated)},
		{"reuters", cfg.ReutersSimulated, ingest.NewReutersSource(cfg.NewsRSSFeeds, cfg.ReutersSimulated, cfg.NewsPollInterval)},
		{"twitter", cfg.TwitterSimulated, twitterSrc},
		{"finnhub", cfg.FinnhubSimulated, finnhubSrc},
		{"fred", cfg.FREDSimulated, fredSrc},
		{"reddit", cfg.RedditSimulated, redditSrc},
		{"weather", cfg.WeatherSimulated, weatherSrc},
	}
	var sources []ingest.IngestionSource
	for _, c := range candidates {
		if c.simulated && !cfg.AllowSimulatedSources {
			log.Printf("[INFO] Skipping SIMULATED ingestion source %q (no synthetic data). Configure it for live, or set ALLOW_SIMULATED_SOURCES=true.", c.name)
			continue
		}
		log.Printf("[INFO] Ingestion source %q enabled (%s).", c.name, map[bool]string{true: "SIMULATED", false: "live"}[c.simulated])
		sources = append(sources, c.src)
		// Register the Twitter source with the monitor for the status dashboard's
		// External APIs card (only when it's actually enabled).
		if c.name == "twitter" && mon != nil {
			mon.SetTwitterSource(twitterSrc)
		}
		if c.name == "finnhub" && mon != nil {
			mon.SetFinnhubSource(finnhubSrc)
		}
		if c.name == "fred" && mon != nil {
			mon.SetFREDSource(fredSrc)
		}
		if c.name == "reddit" && mon != nil {
			mon.SetRedditSource(redditSrc)
		}
		if c.name == "weather" && mon != nil {
			mon.SetWeatherSource(weatherSrc)
		}
	}
	// The NWS forecast source is gated on its own flag (not the simulated toggle), so
	// it's registered outside the candidate loop.
	if forecastSrc != nil {
		log.Printf("[INFO] Ingestion source %q enabled (live).", "weather-forecast")
		sources = append(sources, forecastSrc)
	}
	if len(sources) == 0 {
		log.Println("[WARN] No live ingestion sources enabled — Kahuna will emit no events. Set REUTERS_SIMULATED=false (real news) or ALLOW_SIMULATED_SOURCES=true.")
	}

	// On-demand news for gordon's activity trigger (Phase 3). Idle unless gordon publishes a
	// request; replies over Redis and never touches the Kafka topic.
	if os.Getenv("ONDEMAND_INGEST") != "false" {
		perMin, _ := strconv.Atoi(os.Getenv("ONDEMAND_INGEST_PER_MIN"))
		odRedis := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, DialTimeout: 2 * time.Second})
		defer odRedis.Close()
		go ingest.NewOnDemandIngest(odRedis, perMin).Start(ctx)
	}

	// 6. Fan-in channel for unified ingestion events
	ingestChan := make(chan *ingest.GekkoIngestEvent, cfg.BufferCapacity)

	// Start Ingestion Sources in concurrent goroutines
	var wg sync.WaitGroup
	for _, source := range sources {
		wg.Add(1)
		go func(src ingest.IngestionSource) {
			defer wg.Done()
			if err := src.Start(ctx, ingestChan); err != nil {
				log.Printf("[ERROR] Ingestion source failed: %v", err)
			}
		}(source)
	}

	// Close channel when all ingestion sources complete/shutdown
	go func() {
		wg.Wait()
		close(ingestChan)
		log.Println("[INFO] All ingestion sources finished. Unified event channel closed.")
	}()

	// 7. Handle system signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Signal waiter goroutine
	go func() {
		sig := <-sigChan
		log.Printf("[INFO] Received signal: %v. Initiating graceful shutdown...", sig)
		cancel() // Cancel context to stop ingestion sources
	}()

	// 8. Ingestion & Filtering Pipeline loop
	log.Println("[INFO] Kahuna Ingestion Loop active. Processing incoming streams...")
	processedCount := 0
	quarantinedCount := 0

	var throttle <-chan time.Time
	if cfg.RateLimit > 0 {
		ticker := time.NewTicker(time.Second / time.Duration(cfg.RateLimit))
		defer ticker.Stop()
		throttle = ticker.C
		log.Printf("[INFO] Rate limiting active. Throttling pipeline to %d events/sec", cfg.RateLimit)
	}

	for event := range ingestChan {
		if throttle != nil {
			select {
			case <-throttle:
			case <-ctx.Done():
				break
			}
		}

		// Pass through compliance gatekeeper
		valid, err := gatekeeper.Validate(event)
		if !valid {
			reason := "Validation failed"
			if err != nil {
				reason = err.Error()
			}
			quarantinedCount++
			if qErr := quarantiner.Quarantine(event, reason); qErr != nil {
				log.Printf("[ERROR] Failed to quarantine event: %v", qErr)
			}
			if mon != nil {
				mon.RecordEvent(event, "QUARANTINED", reason)
			}
			continue
		}

		// Ship valid events to Kafka
		// Since Produce handles timeouts internal to its call, this keeps the loop robust
		_ = producer.Produce(ctx, event)
		processedCount++
		if mon != nil {
			mon.RecordEvent(event, "PROCESSED", "")
		}
	}

	// Give a final summary on shutdown
	log.Printf("[INFO] Kahuna Ingestion Worker stopped. Pipeline summary: Processed: %d | Quarantined: %d", processedCount, quarantinedCount)
	// Give a small buffer time for system warnings to flush
	time.Sleep(500 * time.Millisecond)
}
