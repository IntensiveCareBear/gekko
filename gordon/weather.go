package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

// The weather strategy is a DETERMINISTIC specialist that bypasses the LLM. It runs
// in a dedicated gordon (GORDON_WEATHER_MODE=true, persona "weather"): it consumes
// only WEATHER observations, matches them to Kalshi daily high-temperature buckets
// from the discovered catalog, and — for Phase 1 — proposes BUYs only on buckets the
// day's observed high has ALREADY cleared, where the YES outcome is settled fact and
// independent of how much more the day warms. See docs/weather-trading-proposal.md.

// stationToSeries maps a METAR station (kahuna's weather ingest) to the Kalshi daily
// high-temp series it prices. Confirmed against each series' NWS settlement source
// (e.g. Chicago resolves off Midway/KMDW, LA off LAX).
var stationToSeries = map[string]string{
	"KNYC": "KXHIGHNY",
	"KMDW": "KXHIGHCHI",
	"KMIA": "KXHIGHMIA",
	"KAUS": "KXHIGHAUS",
	"KDEN": "KXHIGHDEN",
	"KLAX": "KXHIGHLAX",
	"KPHL": "KXHIGHPHIL",
}

// weatherObs mirrors kahuna's emitted WeatherObs ingest payload.
type weatherObs struct {
	Station   string  `json:"station"`
	City      string  `json:"city"`
	TempF     float64 `json:"temp_f"`
	DayHighF  float64 `json:"day_high_f"`
	LocalDate string  `json:"local_date"` // YYYY-MM-DD in the station's tz
}

// WeatherConfig holds the strategy's knobs (all env-tunable).
type WeatherConfig struct {
	ClearMarginF  float64 // require day-high ≥ threshold + margin before a bucket counts as cleared (NWS-climate-vs-METAR slack)
	MinEdge       float64 // require fair − ask ≥ this to propose (cleared-bucket path)
	MaxAsk        float64 // skip buckets already priced at/above this (no room left)
	SettleHaircut float64 // fair = 1.0 − haircut for a cleared bucket (residual settlement risk)
	BaseSize      int     // contracts per proposal (breen re-sizes and risk-gates)

	// Phase 2 predictive pricing (forecast-bias model).
	PredictiveEnabled bool    // price still-live buckets from the NWS forecast + learned bias
	PredictiveMinEdge float64 // require fair − ask ≥ this to propose predictively (thinner edges ⇒ higher bar)
	PredictiveHaircut float64 // shave the model probability before comparing to ask (model risk)
	PredictiveMinAsk  float64 // ignore near-zero asks (illiquid dust) on the predictive path
	PredictiveMaxEdge float64 // skip edges above this: a huge gap to the market means the model is wrong, not that the market is
	MinSigmaF         float64 // floor on the learned sigma: it is fit to late forecasts but applied to day-ahead ones
	// StationMinSigmaF raises the floor for stations whose day-ahead error ran well past it
	// (10-13 review: AUS mean error +3.65°F, LAX mean |error| 5.2°F over 5 days, thin data).
	StationMinSigmaF map[string]float64
}

// parseStationSigma parses "KAUS=4.0,KLAX=5.0"; malformed entries are dropped.
func parseStationSigma(s string) map[string]float64 {
	m := map[string]float64{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if f, err := strconv.ParseFloat(v, 64); ok && err == nil && f > 0 {
			m[strings.ToUpper(strings.TrimSpace(k))] = f
		}
	}
	return m
}

// minSigma is the sigma floor for a station: the larger of the global and per-station floor.
func (c WeatherConfig) minSigma(station string) float64 {
	return math.Max(c.MinSigmaF, c.StationMinSigmaF[station])
}

// DefaultWeatherConfig reads the strategy knobs from the environment.
func DefaultWeatherConfig() WeatherConfig {
	return WeatherConfig{
		ClearMarginF:      getEnvFloat("WEATHER_CLEAR_MARGIN_F", 1.0),
		MinEdge:           getEnvFloat("WEATHER_MIN_EDGE", 0.03),
		MaxAsk:            getEnvFloat("WEATHER_MAX_ASK", 0.98),
		SettleHaircut:     getEnvFloat("WEATHER_SETTLE_HAIRCUT", 0.0),
		BaseSize:          getEnvInt("WEATHER_BASE_SIZE", 100),
		PredictiveEnabled: getEnvBool("WEATHER_PREDICTIVE", true),
		PredictiveMinEdge: getEnvFloat("WEATHER_PREDICTIVE_MIN_EDGE", 0.05),
		PredictiveHaircut: getEnvFloat("WEATHER_PREDICTIVE_HAIRCUT", 0.0),
		PredictiveMinAsk:  getEnvFloat("WEATHER_PREDICTIVE_MIN_ASK", 0.03),
		PredictiveMaxEdge: getEnvFloat("WEATHER_PREDICTIVE_MAX_EDGE", 0.25),
		MinSigmaF:         getEnvFloat("WEATHER_MIN_SIGMA_F", 3.0),
		StationMinSigmaF:  parseStationSigma(getEnv("WEATHER_STATION_MIN_SIGMA_F", "KAUS=4.0,KLAX=5.0")),
	}
}

type WeatherStrategy struct {
	rdb           *redis.Client
	prod          *kgo.Client
	outboundTopic string
	cfg           WeatherConfig
	tr            *TelemetryRegistry
	bias          *BiasTable     // Phase 2: per-station forecast-bias model (nil ⇒ predictive off)
	forecasts     *forecastCache // Phase 2: latest NWS forecast per station-date
	ledger        *pickLedger    // forecasts actually priced with (nil ⇒ off)
}

func NewWeatherStrategy(redisAddr string, prod *kgo.Client, outboundTopic string, cfg WeatherConfig, tr *TelemetryRegistry) *WeatherStrategy {
	ws := &WeatherStrategy{
		rdb:           redis.NewClient(&redis.Options{Addr: redisAddr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second}),
		prod:          prod,
		outboundTopic: outboundTopic,
		cfg:           cfg,
		tr:            tr,
		forecasts:     newForecastCache(),
	}
	if cfg.PredictiveEnabled {
		ws.bias = NewBiasTable(getEnv("WEATHER_BIAS_FILE", "runs/weather/bias.json"))
		ws.ledger = newPickLedger(defaultPickLedgerPath())
	}
	return ws
}

// bucketKind classifies a Kalshi high-temp bucket by its resolution shape.
type bucketKind int

const (
	bucketAbove bucketKind = iota // "X° or above": YES if high ≥ X
	bucketBelow                   // "X° or below": YES if high ≤ X
	bucketRange                   // "X° to Y°":    YES if X ≤ high ≤ Y
)

type tempBucket struct {
	kind bucketKind
	lo   float64 // above: threshold; range: low edge
	hi   float64 // below: threshold; range: high edge
}

var (
	reAbove = regexp.MustCompile(`(-?\d+)\s*°?\s*or above`)
	reBelow = regexp.MustCompile(`(-?\d+)\s*°?\s*or below`)
	reRange = regexp.MustCompile(`(-?\d+)\s*°\s*to\s*(-?\d+)\s*°`)
)

// parseTempBucket parses the bucket shape from a market title/subtitle. marketdata
// appends the Kalshi yes_sub_title to the title (e.g. "… — 89° or above"), which is
// the authoritative resolution text, so we match the human phrasing rather than
// inferring off strike arithmetic. Pure.
func parseTempBucket(title string) (tempBucket, bool) {
	if m := reAbove.FindStringSubmatch(title); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return tempBucket{kind: bucketAbove, lo: v}, true
		}
	}
	if m := reBelow.FindStringSubmatch(title); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return tempBucket{kind: bucketBelow, hi: v}, true
		}
	}
	if m := reRange.FindStringSubmatch(title); m != nil {
		lo, e1 := strconv.ParseFloat(m[1], 64)
		hi, e2 := strconv.ParseFloat(m[2], 64)
		if e1 == nil && e2 == nil {
			return tempBucket{kind: bucketRange, lo: lo, hi: hi}, true
		}
	}
	return tempBucket{}, false
}

var reEventDate = regexp.MustCompile(`^(\d{2})([A-Z]{3})(\d{2})$`)

// eventDateFromTicker extracts the local calendar date a high-temp market resolves
// on from its ticker's event segment, e.g. "KXHIGHNY-26JUL11-T88" → "2026-07-11".
// Pure.
func eventDateFromTicker(ticker string) (string, bool) {
	parts := strings.Split(ticker, "-")
	if len(parts) < 2 {
		return "", false
	}
	m := reEventDate.FindStringSubmatch(parts[1])
	if m == nil {
		return "", false
	}
	mon, ok := monthAbbr[m[2]]
	if !ok {
		return "", false
	}
	yy, _ := strconv.Atoi(m[1])
	dd, _ := strconv.Atoi(m[3])
	return fmt.Sprintf("20%02d-%02d-%02d", yy, int(mon), dd), true
}

// clearedBucketEdge decides whether a bucket is a Phase-1 tradeable cleared YES and
// returns the edge (fair − ask). Only "X° or above" buckets qualify: once the day's
// observed high has reached X (plus a safety margin), that YES is settled regardless
// of further warming. "or below" and range buckets can still be pushed out of the
// money by later warming, so they are never cleared-YES intraday. Pure.
func clearedBucketEdge(b tempBucket, dayHighF, ask float64, cfg WeatherConfig) (edge float64, ok bool) {
	if b.kind != bucketAbove {
		return 0, false
	}
	if dayHighF < b.lo+cfg.ClearMarginF {
		return 0, false // not cleared yet (with margin for NWS-vs-METAR slack)
	}
	if ask <= 0 || ask >= cfg.MaxAsk {
		return 0, false // no ask, or already priced with no room
	}
	fair := 1.0 - cfg.SettleHaircut
	edge = fair - ask
	if edge < cfg.MinEdge {
		return 0, false
	}
	return edge, true
}

// clearedPick is one qualifying cleared-bucket BUY.
type clearedPick struct {
	market catalogMarket
	bucket tempBucket
	edge   float64
}

// selectClearedBuckets returns the cleared "X° or above" buckets for the observation's
// series and local date that clear the edge/price gates. Pure — the whole selection is
// unit-testable without Redis/Kafka.
func selectClearedBuckets(markets []catalogMarket, series string, obs weatherObs, cfg WeatherConfig) []clearedPick {
	prefix := series + "-"
	var picks []clearedPick
	for _, m := range markets {
		if !strings.HasPrefix(m.Ticker, prefix) {
			continue
		}
		evDate, ok := eventDateFromTicker(m.Ticker)
		if !ok || evDate != obs.LocalDate {
			continue // a different day's event, or unparseable
		}
		b, ok := parseTempBucket(m.Title)
		if !ok {
			continue
		}
		edge, ok := clearedBucketEdge(b, obs.DayHighF, m.YesAsk, cfg)
		if !ok {
			continue
		}
		picks = append(picks, clearedPick{market: m, bucket: b, edge: edge})
	}
	return picks
}

// OnObservation prices the cleared high-temp buckets for one weather observation and
// emits a BUY proposal per qualifying bucket.
func (ws *WeatherStrategy) OnObservation(ctx context.Context, ev GekkoIngestEvent, obs weatherObs) {
	series, ok := stationToSeries[strings.ToUpper(obs.Station)]
	if !ok {
		return // station we don't map to a market
	}
	markets, err := ws.loadCatalog(ctx)
	if err != nil {
		ws.tr.AddProposalLog(fmt.Sprintf("[WEATHER] catalog read failed: %v", err))
		return
	}
	picks := selectClearedBuckets(markets, series, obs, ws.cfg)
	proposed := make(map[string]bool, len(picks))
	for _, p := range picks {
		ws.emit(ctx, ev, p.market, obs, p.bucket, p.edge)
		proposed[p.market.Ticker] = true
	}
	if len(picks) > 0 {
		ws.tr.AddProposalLog(fmt.Sprintf("[WEATHER] %s (%s) day-high %.1f°F → %d cleared bucket BUY(s) on %s",
			obs.Station, obs.City, obs.DayHighF, len(picks), series))
	}

	// Phase 2: price still-live buckets from the forecast-bias model. Only when the
	// station is seasoned and today's forecast is cached; cleared buckets already
	// emitted above are skipped so a market isn't proposed twice.
	preds := ws.selectPredictiveBuckets(markets, series, obs, proposed)
	n := 0
	for _, p := range preds {
		// Model-only thesis doesn't strengthen hour to hour: no pyramiding into a held contract.
		if held, known := riskStateReader.HeldContract(ctx, p.market.Ticker); known && held {
			continue
		}
		ws.emitPredictive(ctx, ev, p, obs)
		n++
	}
	if n > 0 {
		ws.tr.AddProposalLog(fmt.Sprintf("[WEATHER] %s (%s) day-high %.1f°F → %d predictive BUY(s) on %s",
			obs.Station, obs.City, obs.DayHighF, n, series))
	}
}

// OnForecast caches the NWS daily-high forecast carried by a WEATHER_FORECAST event so
// OnObservation can price live buckets against it.
func (ws *WeatherStrategy) OnForecast(ev GekkoIngestEvent) {
	var fc struct {
		Station string  `json:"station"`
		Date    string  `json:"date"`
		HighF   float64 `json:"high_f"`
	}
	if err := json.Unmarshal([]byte(ev.RawContent), &fc); err != nil {
		ws.tr.AddProposalLog(fmt.Sprintf("[WEATHER] bad forecast payload: %v", err))
		return
	}
	if fc.Station == "" || fc.Date == "" {
		return
	}
	ws.forecasts.put(strings.ToUpper(fc.Station), fc.Date, fc.HighF)
}

// predictivePick is one forecast-model BUY on a still-live bucket.
type predictivePick struct {
	market catalogMarket
	bucket tempBucket
	prob   float64 // model P(YES)
	edge   float64 // prob(after haircut) − ask
	mu     float64 // predicted final high (forecast + bias), for the proposal text
}

// selectPredictiveBuckets prices every live bucket for the observation's series/date
// from Normal(forecast+bias, sigma) truncated below by the running high, and returns
// the underpriced-YES buckets clearing the predictive edge/ask gates. Skips tickers in
// `skip` (already emitted as cleared). Returns nil when predictive pricing is off, the
// station isn't seasoned, or no forecast is cached — so it lands dark and self-gates.
func (ws *WeatherStrategy) selectPredictiveBuckets(markets []catalogMarket, series string, obs weatherObs, skip map[string]bool) []predictivePick {
	if !ws.cfg.PredictiveEnabled || ws.bias == nil {
		return nil
	}
	station := strings.ToUpper(obs.Station)
	entry, ok := ws.bias.Lookup(station)
	if !ok {
		return nil // no seasoned model for this station yet
	}
	forecast, ok := ws.forecasts.get(station, obs.LocalDate)
	if !ok {
		return nil // no forecast cached for today
	}
	mu := forecast + entry.Bias
	ws.ledger.record(pickLedgerRow{TS: time.Now(), Station: station, Date: obs.LocalDate, Forecast: forecast,
		Bias: entry.Bias, Sigma: math.Max(entry.Sigma, ws.cfg.minSigma(station)), Mu: mu, DayHigh: obs.DayHighF})
	prefix := series + "-"
	var picks []predictivePick
	for _, m := range markets {
		if skip[m.Ticker] || !strings.HasPrefix(m.Ticker, prefix) {
			continue
		}
		if evDate, ok := eventDateFromTicker(m.Ticker); !ok || evDate != obs.LocalDate {
			continue
		}
		b, ok := parseTempBucket(m.Title)
		if !ok {
			continue
		}
		if m.YesAsk < ws.cfg.PredictiveMinAsk || m.YesAsk >= ws.cfg.MaxAsk {
			continue // dust or no room left
		}
		prob := bucketYesProb(b, mu, math.Max(entry.Sigma, ws.cfg.minSigma(station)), obs.DayHighF)
		fair := prob - ws.cfg.PredictiveHaircut
		edge := fair - m.YesAsk
		if edge < ws.cfg.PredictiveMinEdge || (ws.cfg.PredictiveMaxEdge > 0 && edge > ws.cfg.PredictiveMaxEdge) {
			continue
		}
		picks = append(picks, predictivePick{market: m, bucket: b, prob: prob, edge: edge, mu: mu})
	}
	return picks
}

// loadCatalog reads the discovered Kalshi market catalog marketdata publishes.
func (ws *WeatherStrategy) loadCatalog(ctx context.Context) ([]catalogMarket, error) {
	val, err := ws.rdb.Get(ctx, "gekko:kalshi_markets").Result()
	if err != nil {
		return nil, err
	}
	var markets []catalogMarket
	if err := json.Unmarshal([]byte(val), &markets); err != nil {
		return nil, err
	}
	return markets, nil
}

// emit builds and publishes the BUY proposal for a cleared bucket. breen owns final
// sizing, the live-book entry gate, and risk — this just states the deterministic
// edge with a limit anchored at the current ask.
func (ws *WeatherStrategy) emit(ctx context.Context, ev GekkoIngestEvent, m catalogMarket, obs weatherObs, b tempBucket, edge float64) {
	fair := 1.0 - ws.cfg.SettleHaircut
	text := fmt.Sprintf("Weather: %s high %.1f°F already ≥ %.0f°F — bucket %q cleared YES; fair %.2f vs ask %.2f (edge %.2f).",
		obs.City, obs.DayHighF, b.lo, m.Ticker, fair, m.YesAsk, edge)
	ws.tr.AddProposalLog("[WEATHER] " + text)
	if ws.prod == nil {
		return
	}
	proposal := StrategyProposal{
		ContractID:       m.Ticker,
		Action:           "BUY",
		AveragePrice:     m.YesAsk,
		TotalSize:        ws.cfg.BaseSize,
		Timestamp:        time.Now(),
		EventHash:        ev.PayloadHash,
		ProposalText:     text,
		SourceID:         ev.SourceID,
		SourceURL:        ev.SourceURL,
		ModelUsed:        "weather-deterministic",
		StrategyID:       personaID,
		Persona:          personaID,
		Confidence:       0.99,  // a cleared bucket is settled fact
		FairValue:        1.0,   // ...so its fair value is the full dollar, not 0.99
		SettleResolution: "YES", // the day's high already cleared the "or above" threshold
	}
	b2, err := json.Marshal(proposal)
	if err != nil {
		ws.tr.AddProposalLog(fmt.Sprintf("[WEATHER] proposal marshal failed: %v", err))
		return
	}
	ws.tr.IncrementProposed()
	rec := &kgo.Record{Topic: ws.outboundTopic, Value: b2}
	prodCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	if err := ws.prod.ProduceSync(prodCtx, rec).FirstErr(); err != nil {
		ws.tr.AddProposalLog(fmt.Sprintf("[WEATHER] emit failed: %v", err))
	}
	cancel()
}

// emitPredictive publishes a BUY for a still-live bucket the forecast-bias model finds
// underpriced. Unlike a cleared bucket this is NOT settled fact — Confidence carries the
// model probability and SettleResolution is left empty so breen weights it like any
// other conviction-scored signal.
func (ws *WeatherStrategy) emitPredictive(ctx context.Context, ev GekkoIngestEvent, p predictivePick, obs weatherObs) {
	m := p.market
	text := fmt.Sprintf("Weather(model): %s predicted final high %.1f°F (forecast-bias) — bucket %q fair %.2f vs ask %.2f (edge %.2f); day-high so far %.1f°F.",
		obs.City, p.mu, m.Ticker, p.prob, m.YesAsk, p.edge, obs.DayHighF)
	ws.tr.AddProposalLog("[WEATHER] " + text)
	if ws.prod == nil {
		return
	}
	proposal := StrategyProposal{
		ContractID:   m.Ticker,
		Action:       "BUY",
		AveragePrice: m.YesAsk,
		TotalSize:    ws.cfg.BaseSize,
		Timestamp:    time.Now(),
		EventHash:    ev.PayloadHash,
		ProposalText: text,
		SourceID:     ev.SourceID,
		SourceURL:    ev.SourceURL,
		ModelUsed:    "weather-forecast-bias",
		StrategyID:   personaID,
		Persona:      personaID,
		Confidence:   p.prob, // model probability, not settled fact
		FairValue:    p.prob, // ...and here it IS the fair value, for breen's edge gate
	}
	b2, err := json.Marshal(proposal)
	if err != nil {
		ws.tr.AddProposalLog(fmt.Sprintf("[WEATHER] predictive marshal failed: %v", err))
		return
	}
	ws.tr.IncrementProposed()
	rec := &kgo.Record{Topic: ws.outboundTopic, Value: b2}
	prodCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	if err := ws.prod.ProduceSync(prodCtx, rec).FirstErr(); err != nil {
		ws.tr.AddProposalLog(fmt.Sprintf("[WEATHER] predictive emit failed: %v", err))
	}
	cancel()
}
