package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

type SourceID string

const (
	SourceKalshi      SourceID = "KALSHI"
	SourcePolymarket  SourceID = "POLYMARKET"
	SourceReuters     SourceID = "REUTERS"
	SourceTwitter     SourceID = "TWITTER"
	SourceFinnhub     SourceID = "FINNHUB"
	SourceFRED        SourceID = "FRED"
	SourceReddit      SourceID = "REDDIT"
	SourceWeather     SourceID = "WEATHER"
	// SourceWeatherForecast carries the NWS daily-high FORECAST (api.weather.gov),
	// distinct from SourceWeather's realized METAR observations. Phase 2 (forecast-bias
	// model) consumes it to price still-live temperature buckets.
	SourceWeatherForecast SourceID = "WEATHER_FORECAST"
)

// GekkoIngestEvent represents the universal structure of ingested public events.
type GekkoIngestEvent struct {
	SourceID             SourceID `json:"SourceID"`
	RawContent           string   `json:"RawContent"`
	SourceURL            string   `json:"SourceURL"`
	ExtractedTimestampNS int64    `json:"ExtractedTimestampNS"`
	PayloadHash          string   `json:"PayloadHash"`
}

// ComputeHash computes the SHA-256 hash of the RawContent to populate PayloadHash.
func (e *GekkoIngestEvent) ComputeHash() string {
	h := sha256.New()
	h.Write([]byte(e.RawContent))
	return hex.EncodeToString(h.Sum(nil))
}

// IngestionSource defines the lifecycle contract for all event source plugins.
type IngestionSource interface {
	Start(ctx context.Context, output chan<- *GekkoIngestEvent) error
}
