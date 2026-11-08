package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Weather FORECAST ingest (Phase 2). Pulls the NWS official daily-high forecast per
// station from api.weather.gov — keyless, only a polite User-Agent + calls/min guard.
// The realized METAR high (weather.go) tells us what the temperature DID; this tells
// us what NWS EXPECTS it to be. Phase 2's forecast-bias model learns the per-station
// residual between the two and prices still-live temperature buckets from it.
//
// Two-step NWS pattern: /points/{lat},{lon} returns a station-stable gridpoint
// forecast URL (resolved once and cached), then /gridpoints/.../forecast returns
// day/night periods carrying a temperature (already °F) and a startTime date. The
// daytime period's temperature for a local date is that day's forecast high.
var nwsForecastBase = "https://api.weather.gov"

// nwsForecastRef is the compliance-authorized provenance host for emitted events.
const nwsForecastRef = "https://api.weather.gov"

// nwsUserAgent identifies the client, as api.weather.gov requires (it 403s a blank
// UA). Contact is a placeholder mailbox, per NWS guidance.
const nwsUserAgent = "gekko-weather/1.0 (https://github.com/gekko; contact: gekko-ops@example.com)"

// ForecastRecord is one station's forecast high for one local calendar day.
type ForecastRecord struct {
	Station    string    `json:"station"`
	Date       string    `json:"date"`   // YYYY-MM-DD, station-local
	HighF      float64   `json:"high_f"` // NWS daytime-period forecast high
	CapturedAt time.Time `json:"captured_at"`
}

// ForecastStore is a small file-backed map of the latest forecast per (station, date),
// shared between the forecast source (writer) and the METAR corpus recorder (reader at
// day rollover). File-backed so a mid-day restart doesn't lose the day's forecast.
// Latest-write-wins: the residual hour-of-day effect between an early and late forecast
// is deferred to the richer regressor (see docs/weather-phase2-forecast-bias.md).
type ForecastStore struct {
	mu   sync.Mutex
	path string
	m    map[string]ForecastRecord // "station|date" -> record
}

func forecastKey(station, date string) string { return station + "|" + date }

// NewForecastStore loads any persisted forecasts from path (missing/corrupt ⇒ empty).
func NewForecastStore(path string) *ForecastStore {
	fs := &ForecastStore{path: path, m: map[string]ForecastRecord{}}
	if raw, err := os.ReadFile(path); err == nil {
		var recs []ForecastRecord
		if json.Unmarshal(raw, &recs) == nil {
			for _, r := range recs {
				fs.m[forecastKey(r.Station, r.Date)] = r
			}
		}
	}
	return fs
}

// Put records (or refreshes) the forecast for a station-date and persists the store.
func (fs *ForecastStore) Put(r ForecastRecord) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.m[forecastKey(r.Station, r.Date)] = r
	fs.pruneLocked()
	fs.saveLocked()
}

// Get returns the forecast for a station-date, if recorded.
func (fs *ForecastStore) Get(station, date string) (ForecastRecord, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	r, ok := fs.m[forecastKey(station, date)]
	return r, ok
}

// pruneLocked drops forecasts older than ~10 days so the file can't grow unbounded.
func (fs *ForecastStore) pruneLocked() {
	if len(fs.m) <= 256 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -10).Format("2006-01-02")
	for k, r := range fs.m {
		if r.Date < cutoff {
			delete(fs.m, k)
		}
	}
}

// saveLocked writes the store atomically (temp + rename). Caller holds fs.mu.
func (fs *ForecastStore) saveLocked() {
	if fs.path == "" {
		return
	}
	recs := make([]ForecastRecord, 0, len(fs.m))
	for _, r := range fs.m {
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Station != recs[j].Station {
			return recs[i].Station < recs[j].Station
		}
		return recs[i].Date < recs[j].Date
	})
	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(fs.path), 0o755); err != nil {
		log.Printf("[WARNING] Weather forecast: mkdir for store failed: %v", err)
		return
	}
	tmp := fs.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("[WARNING] Weather forecast: write store failed: %v", err)
		return
	}
	if err := os.Rename(tmp, fs.path); err != nil {
		log.Printf("[WARNING] Weather forecast: rename store failed: %v", err)
	}
}

// WeatherForecastConfig configures the NWS forecast source.
type WeatherForecastConfig struct {
	Enabled        bool
	Stations       []WeatherStation
	PollInterval   time.Duration // NWS forecasts refresh a few times/day; hourly polling is ample
	MaxCallsPerMin int
	Store          *ForecastStore
}

// nwsPointsResp is the subset of /points we read.
type nwsPointsResp struct {
	Properties struct {
		Forecast string `json:"forecast"` // gridpoint forecast URL
	} `json:"properties"`
}

// nwsForecastResp is the subset of /gridpoints/.../forecast we read.
type nwsForecastResp struct {
	Properties struct {
		Periods []struct {
			IsDaytime       bool    `json:"isDaytime"`
			Temperature     float64 `json:"temperature"`
			TemperatureUnit string  `json:"temperatureUnit"`
			StartTime       string  `json:"startTime"` // RFC3339 with local offset
		} `json:"periods"`
	} `json:"properties"`
}

type WeatherForecastSource struct {
	enabled        bool
	stations       []WeatherStation
	pollInterval   time.Duration
	minCallSpacing time.Duration
	store          *ForecastStore
	client         *http.Client

	lastCallAt time.Time
	gridURL    map[string]string // ICAO -> cached gridpoint forecast URL
	locs       map[string]*time.Location

	mu         sync.Mutex
	lastPollAt time.Time
	lastError  string
	forecasts  int64
}

// NewWeatherForecastSource builds the NWS forecast source. It forecasts only stations
// carrying coordinates; coordinate-less custom stations are skipped with a warning.
func NewWeatherForecastSource(cfg WeatherForecastConfig) *WeatherForecastSource {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Hour
	}
	if cfg.MaxCallsPerMin <= 0 {
		cfg.MaxCallsPerMin = 15
	}
	var stations []WeatherStation
	locs := map[string]*time.Location{}
	for _, s := range cfg.Stations {
		if !s.HasCoords() {
			log.Printf("[WARNING] Weather forecast: station %s has no coordinates; forecast skipped for it.", s.ICAO)
			continue
		}
		stations = append(stations, s)
		if _, ok := locs[s.TZ]; !ok {
			loc, err := time.LoadLocation(s.TZ)
			if err != nil {
				loc = time.UTC
			}
			locs[s.TZ] = loc
		}
	}
	return &WeatherForecastSource{
		enabled:        cfg.Enabled,
		stations:       stations,
		pollInterval:   cfg.PollInterval,
		minCallSpacing: time.Minute / time.Duration(cfg.MaxCallsPerMin),
		store:          cfg.Store,
		client:         &http.Client{Timeout: 20 * time.Second},
		gridURL:        map[string]string{},
		locs:           locs,
	}
}

func (w *WeatherForecastSource) Start(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if !w.enabled || len(w.stations) == 0 {
		log.Println("[INFO] Weather forecast source disabled/idle; nothing to poll.")
		<-ctx.Done()
		return nil
	}
	icaos := make([]string, 0, len(w.stations))
	for _, s := range w.stations {
		icaos = append(icaos, s.ICAO)
	}
	log.Printf("[INFO] Weather FORECAST source started (NWS api.weather.gov, keyless): stations %v, poll every %v, ≤%d calls/min",
		icaos, w.pollInterval, int(time.Minute/w.minCallSpacing))

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	w.pollAll(ctx, output)
	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Weather forecast source shutting down...")
			return nil
		case <-ticker.C:
			w.pollAll(ctx, output)
		}
	}
}

func (w *WeatherForecastSource) pollAll(ctx context.Context, output chan<- *GekkoIngestEvent) {
	for _, s := range w.stations {
		if ctx.Err() != nil {
			return
		}
		w.pollStation(ctx, s, output)
	}
}

func (w *WeatherForecastSource) throttle(ctx context.Context) {
	if wait := w.minCallSpacing - time.Since(w.lastCallAt); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	w.lastCallAt = time.Now()
}

func (w *WeatherForecastSource) recordPoll(errMsg string) {
	w.mu.Lock()
	w.lastError = errMsg
	w.lastPollAt = time.Now()
	w.mu.Unlock()
}

// getJSON does a paced, UA-stamped GET and decodes JSON into v.
func (w *WeatherForecastSource) getJSON(ctx context.Context, url string, v interface{}) error {
	w.throttle(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", nwsUserAgent)
	req.Header.Set("Accept", "application/geo+json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// resolveGrid resolves and caches a station's gridpoint forecast URL via /points.
func (w *WeatherForecastSource) resolveGrid(ctx context.Context, s WeatherStation) (string, error) {
	if u, ok := w.gridURL[s.ICAO]; ok && u != "" {
		return u, nil
	}
	var pr nwsPointsResp
	pt := fmt.Sprintf("%s/points/%.4f,%.4f", nwsForecastBase, s.Lat, s.Lon)
	if err := w.getJSON(ctx, pt, &pr); err != nil {
		return "", err
	}
	if pr.Properties.Forecast == "" {
		return "", fmt.Errorf("no forecast URL for %s", s.ICAO)
	}
	w.gridURL[s.ICAO] = pr.Properties.Forecast
	return pr.Properties.Forecast, nil
}

func (w *WeatherForecastSource) pollStation(ctx context.Context, s WeatherStation, output chan<- *GekkoIngestEvent) {
	grid, err := w.resolveGrid(ctx, s)
	if err != nil {
		log.Printf("[ERROR] Weather forecast resolve grid %s failed: %v", s.ICAO, err)
		w.recordPoll(err.Error())
		return
	}
	var fr nwsForecastResp
	if err := w.getJSON(ctx, grid, &fr); err != nil {
		log.Printf("[ERROR] Weather forecast fetch %s failed: %v", s.ICAO, err)
		w.recordPoll(err.Error())
		// A stale grid URL (NWS occasionally re-tiles) — drop the cache so next poll re-resolves.
		delete(w.gridURL, s.ICAO)
		return
	}
	w.recordPoll("")

	loc := w.locs[s.TZ]
	if loc == nil {
		loc = time.UTC
	}
	highs := forecastHighsByDate(fr, loc)
	if len(highs) == 0 {
		return
	}
	now := time.Now()
	for _, date := range sortedKeys(highs) {
		high := highs[date]
		w.store.Put(ForecastRecord{Station: s.ICAO, Date: date, HighF: high, CapturedAt: now})
	}

	// Emit a forecast event for today's high so the decision layer sees it live.
	today := now.In(loc).Format("2006-01-02")
	high, ok := highs[today]
	if !ok {
		return
	}
	payload := ForecastRecord{Station: s.ICAO, Date: today, HighF: high, CapturedAt: now}
	raw, err := json.Marshal(struct {
		ForecastRecord
		City     string `json:"city"`
		Sentence string `json:"sentence"`
	}{payload, s.City, fmt.Sprintf("%s (%s) forecast high %.0f°F for %s (NWS). Source: NWS/api.weather.gov.", s.City, s.ICAO, high, today)})
	if err != nil {
		return
	}
	event := &GekkoIngestEvent{
		SourceID:             SourceWeatherForecast,
		RawContent:           string(raw),
		SourceURL:            nwsForecastRef + "/points/" + fmt.Sprintf("%.4f,%.4f", s.Lat, s.Lon),
		ExtractedTimestampNS: now.UnixNano(),
	}
	event.PayloadHash = event.ComputeHash()
	select {
	case output <- event:
	case <-ctx.Done():
		return
	default:
		log.Println("[WARNING] ingest channel full, dropping Weather forecast")
		return
	}
	w.mu.Lock()
	w.forecasts++
	w.mu.Unlock()
	log.Printf("[INFO] Weather forecast %s (%s): high %.0f°F for %s", s.ICAO, s.City, high, today)
}

// forecastHighsByDate reduces NWS periods to a date->forecast-high map, keeping the
// daytime period per local date (the day's forecast high). Pure.
func forecastHighsByDate(fr nwsForecastResp, loc *time.Location) map[string]float64 {
	out := map[string]float64{}
	for _, p := range fr.Properties.Periods {
		if !p.IsDaytime {
			continue
		}
		t, err := time.Parse(time.RFC3339, p.StartTime)
		if err != nil {
			continue
		}
		date := t.In(loc).Format("2006-01-02")
		temp := p.Temperature
		if p.TemperatureUnit == "C" {
			temp = cToF(temp)
		}
		if _, seen := out[date]; !seen {
			out[date] = temp
		}
	}
	return out
}

func sortedKeys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
