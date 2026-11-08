package compliance

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gekko/kahuna/ingest"
)

func TestGatekeeper_Validate_Success(t *testing.T) {
	gk := NewGatekeeper()

	// Valid public event from Kalshi
	event := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceKalshi,
		RawContent:           `{"type": "market_lifecycle_v2", "market_id": "TEST-001"}`,
		SourceURL:            "https://api.kalshi.com/v2/market_lifecycle_v2",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	event.PayloadHash = event.ComputeHash()

	valid, err := gk.Validate(event)
	if err != nil {
		t.Fatalf("Expected event to validate successfully, got err: %v", err)
	}
	if !valid {
		t.Fatal("Expected event to be valid")
	}

	// Valid public event from Reuters
	reutersEvent := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceReuters,
		RawContent:           `<item><title>Breaking News</title></item>`,
		SourceURL:            "https://www.reuters.com/world/update",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	reutersEvent.PayloadHash = reutersEvent.ComputeHash()

	valid, err = gk.Validate(reutersEvent)
	if err != nil {
		t.Fatalf("Expected reuters event to validate, got err: %v", err)
	}
	if !valid {
		t.Fatal("Expected reuters event to be valid")
	}

	// Valid public event from Google News (Reuters feed fallback)
	googleNewsEvent := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceReuters,
		RawContent:           `<item><title>Google News</title></item>`,
		SourceURL:            "https://news.google.com/rss/articles/CBMi",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	googleNewsEvent.PayloadHash = googleNewsEvent.ComputeHash()

	valid, err = gk.Validate(googleNewsEvent)
	if err != nil {
		t.Fatalf("Expected google news event to validate, got err: %v", err)
	}
	if !valid {
		t.Fatal("Expected google news event to be valid")
	}

	// Valid public event from Finnhub — attributed to finnhub.io (real article URL
	// lives in the payload, so publisher domains needn't be allowlisted).
	finnhubEvent := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceFinnhub,
		RawContent:           `{"headline":"Fed holds rates","source":"Reuters","url":"https://www.reuters.com/markets/fed"}`,
		SourceURL:            "https://finnhub.io/#news-5085164",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	finnhubEvent.PayloadHash = finnhubEvent.ComputeHash()

	valid, err = gk.Validate(finnhubEvent)
	if err != nil {
		t.Fatalf("Expected finnhub event to validate, got err: %v", err)
	}
	if !valid {
		t.Fatal("Expected finnhub event to be valid")
	}

	// Valid public event from FRED — attributed to api.stlouisfed.org.
	fredEvent := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceFRED,
		RawContent:           "U.S. Unemployment Rate (UNRATE) updated to 4.1% for 2026-06-01. Source: FRED.",
		SourceURL:            "https://api.stlouisfed.org/graph/?id=UNRATE",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	fredEvent.PayloadHash = fredEvent.ComputeHash()

	valid, err = gk.Validate(fredEvent)
	if err != nil {
		t.Fatalf("Expected FRED event to validate, got err: %v", err)
	}
	if !valid {
		t.Fatal("Expected FRED event to be valid")
	}

	// Valid public event from Reddit — attributed to the post permalink on
	// www.reddit.com (pulled via oauth.reddit.com, which never appears here).
	redditEvent := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceReddit,
		RawContent:           `{"subreddit":"economics","title":"Fed holds rates steady"}`,
		SourceURL:            "https://www.reddit.com/r/economics/comments/abc123/fed_holds_rates/",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	redditEvent.PayloadHash = redditEvent.ComputeHash()

	valid, err = gk.Validate(redditEvent)
	if err != nil {
		t.Fatalf("Expected Reddit event to validate, got err: %v", err)
	}
	if !valid {
		t.Fatal("Expected Reddit event to be valid")
	}

	// Valid public event from the NOAA weather (METAR) source.
	weatherEvent := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceWeather,
		RawContent:           `{"station":"KNYC","city":"New York City","temp_f":82,"day_high_f":84}`,
		SourceURL:            "https://aviationweather.gov/data/metar?ids=KNYC",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	weatherEvent.PayloadHash = weatherEvent.ComputeHash()

	valid, err = gk.Validate(weatherEvent)
	if err != nil {
		t.Fatalf("Expected weather event to validate, got err: %v", err)
	}
	if !valid {
		t.Fatal("Expected weather event to be valid")
	}
}

func TestGatekeeper_Validate_FailPrivateChatWebhooks(t *testing.T) {
	gk := NewGatekeeper()

	// Discord Webhook link in content
	event1 := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceTwitter,
		RawContent:           `Check out this webhook: https://discord.com/api/webhooks/12345/abcde`,
		SourceURL:            "https://twitter.com/news/status/123",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	event1.PayloadHash = event1.ComputeHash()

	valid, err := gk.Validate(event1)
	if err == nil || valid {
		t.Fatal("Expected validation to fail due to Discord webhook in RawContent")
	}

	// Telegram bot API in URL
	event2 := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceKalshi,
		RawContent:           `Valid payload content`,
		SourceURL:            "https://api.telegram.org/bot12345:token/sendMessage",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	event2.PayloadHash = event2.ComputeHash()

	valid, err = gk.Validate(event2)
	if err == nil || valid {
		t.Fatal("Expected validation to fail due to Telegram URL")
	}
}

func TestGatekeeper_Validate_FailPrivateAddresses(t *testing.T) {
	gk := NewGatekeeper()

	// Localhost address in URL
	event1 := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourcePolymarket,
		RawContent:           `{"market_id": "test"}`,
		SourceURL:            "http://127.0.0.1:8080/ws",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	event1.PayloadHash = event1.ComputeHash()

	valid, err := gk.Validate(event1)
	if err == nil || valid {
		t.Fatal("Expected validation to fail due to loopback IP in SourceURL")
	}

	// Private IP network
	event2 := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceReuters,
		RawContent:           `<xml>test</xml>`,
		SourceURL:            "https://192.168.1.50/feed",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	event2.PayloadHash = event2.ComputeHash()

	valid, err = gk.Validate(event2)
	if err == nil || valid {
		t.Fatal("Expected validation to fail due to private subnet IP in SourceURL")
	}
}

func TestGatekeeper_Validate_HashMismatch(t *testing.T) {
	gk := NewGatekeeper()

	event := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceTwitter,
		RawContent:           "Twitter content",
		SourceURL:            "https://twitter.com/realDonaldTrump/status/9876",
		ExtractedTimestampNS: time.Now().UnixNano(),
		PayloadHash:          "incorrect_hash_value",
	}

	valid, err := gk.Validate(event)
	if err == nil || valid {
		t.Fatal("Expected validation to fail due to cryptographic hash mismatch")
	}
}

func TestQuarantiner_Quarantine(t *testing.T) {
	tempDir := t.TempDir()
	fallbackDir := t.TempDir()

	quar := NewQuarantiner(tempDir, fallbackDir)

	event := &ingest.GekkoIngestEvent{
		SourceID:             ingest.SourceKalshi,
		RawContent:           "unauthorized chat link here: t.me/joinchat/123",
		SourceURL:            "https://api.kalshi.com/v2/market_lifecycle_v2",
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	event.PayloadHash = event.ComputeHash()

	err := quar.Quarantine(event, "test quarantine reason")
	if err != nil {
		t.Fatalf("Quarantine function failed: %v", err)
	}

	// Verify file exists in tempDir
	files, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("Failed to read quarantine directory: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("Expected exactly 1 quarantined file, found: %d", len(files))
	}

	// Verify filename structure
	filename := files[0].Name()
	if !filepath.HasPrefix(filename, "quarantine_KALSHI_") {
		t.Errorf("Unexpected quarantined filename pattern: %s", filename)
	}
}
