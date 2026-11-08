package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Weather ingest reads METAR/SPECI surface observations from NOAA's Aviation
// Weather Center Data API (aviationweather.gov) — free, keyless, no budget, only a
// calls/min pacing guard. It is the numeric ground truth behind Kalshi's daily
// high-temperature markets: the market's outcome is a mechanical function of the
// day's observed high at a specific station, so this feeds the decision layer real
// readings instead of opinions about the weather.
//
// Per poll, for each station we fetch a lookback window, filter observations to the
// station's LOCAL calendar day, and compute the running high so far (folding in the
// 6-hour max-temperature group when a synoptic report carries one). The latest new
// observation is emitted with that running high attached, so gordon's weather
// strategy can price already-cleared temperature buckets deterministically.
var weatherAPIBase = "https://aviationweather.gov/api/data/metar"

// weatherSourceRef is the compliance-authorized provenance host for emitted events.
const weatherSourceRef = "https://aviationweather.gov"

const maxSeenWeather = 4000

// WeatherStation maps an ICAO id to the Kalshi city it prices and the IANA timezone
// used to bound the local calendar day. The station MUST match the one Kalshi settles
// against (usually the NWS station named in the market's rules), or the high will be
// derived from the wrong site.
type WeatherStation struct {
	ICAO string  // e.g. "KNYC"
	City string  // human label matching the Kalshi market phrasing, e.g. "New York City"
	TZ   string  // IANA tz for the local calendar day, e.g. "America/New_York"
	Lat  float64 // station latitude, for the NWS forecast source (0 = unknown ⇒ no forecast)
	Lon  float64 // station longitude
}

// HasCoords reports whether the station carries lat/lon usable by the NWS forecast
// source. Exactly (0,0) — the Gulf of Guinea — is treated as "unset".
func (s WeatherStation) HasCoords() bool { return s.Lat != 0 || s.Lon != 0 }

// defaultWeatherStations is the full liquid Kalshi high-temp set. These are the
// primary NWS/airport stations; verify each against the specific market's resolution
// rules before trusting it for live money (paper trading tolerates small mismatches).
// Coordinates are the settlement site (NWS snaps them to the nearest forecast grid).
var defaultWeatherStations = []WeatherStation{
	{"KNYC", "New York City", "America/New_York", 40.78, -73.97},
	{"KMDW", "Chicago", "America/Chicago", 41.79, -87.75},
	{"KAUS", "Austin", "America/Chicago", 30.19, -97.67},
	{"KDEN", "Denver", "America/Denver", 39.85, -104.67},
	{"KLAX", "Los Angeles", "America/Los_Angeles", 33.94, -118.41},
	{"KMIA", "Miami", "America/New_York", 25.79, -80.29},
	{"KPHL", "Philadelphia", "America/New_York", 39.87, -75.24},
}

// stationCoords lets a 3-field "ICAO:City:TZ" spec still pick up forecast coordinates
// for a known station, so operators overriding WEATHER_STATIONS don't silently lose
// the Phase 2 forecast unless they list an unknown ICAO.
var stationCoords = func() map[string][2]float64 {
	m := make(map[string][2]float64, len(defaultWeatherStations))
	for _, s := range defaultWeatherStations {
		m[s.ICAO] = [2]float64{s.Lat, s.Lon}
	}
	return m
}()

// ParseWeatherStations parses an "ICAO:City:TZ" (or "ICAO:City:TZ:lat:lon")
// comma-separated spec into stations, returning the default liquid set when the spec
// is empty. Malformed tuples are skipped with a warning so one typo can't drop every
// station. A 3-field entry inherits coordinates from stationCoords when the ICAO is
// known; explicit lat/lon (5 fields) always wins.
func ParseWeatherStations(spec string) []WeatherStation {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return defaultWeatherStations
	}
	var out []WeatherStation
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		parts := strings.Split(tok, ":")
		if (len(parts) != 3 && len(parts) != 5) || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[2]) == "" {
			log.Printf("[WARNING] Weather: skipping malformed station spec %q (want ICAO:City:TZ or ICAO:City:TZ:lat:lon)", tok)
			continue
		}
		st := WeatherStation{
			ICAO: strings.TrimSpace(parts[0]),
			City: strings.TrimSpace(parts[1]),
			TZ:   strings.TrimSpace(parts[2]),
		}
		if len(parts) == 5 {
			lat, e1 := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
			lon, e2 := strconv.ParseFloat(strings.TrimSpace(parts[4]), 64)
			if e1 != nil || e2 != nil {
				log.Printf("[WARNING] Weather: bad lat/lon in station spec %q; forecast disabled for it", tok)
			} else {
				st.Lat, st.Lon = lat, lon
			}
		} else if c, ok := stationCoords[st.ICAO]; ok {
			st.Lat, st.Lon = c[0], c[1]
		}
		out = append(out, st)
	}
	if len(out) == 0 {
		return defaultWeatherStations
	}
	return out
}

// WeatherConfig configures the METAR ingestion source. Live unless simulated; the
// data needs no key, so it runs whenever enabled.
type WeatherConfig struct {
	Simulated      bool
	Stations       []WeatherStation
	PollInterval   time.Duration // how often to re-poll each station (METAR is ~hourly; poll finer to catch SPECI)
	MaxCallsPerMin int           // pacing guard (aviationweather has no published hard limit; stay polite)
	LookbackHours  int           // window fetched to reconstruct the local-day high (must span since local midnight)
}

// WeatherObs is the structured payload emitted per new observation. TempF is the
// reading at ObsTime; DayHighF is the running local-day maximum so far — the value
// gordon's cleared-bucket strategy keys off.
type WeatherObs struct {
	Station   string    `json:"station"`    // ICAO id
	City      string    `json:"city"`       // Kalshi city label
	TempF     float64   `json:"temp_f"`     // observed temperature at ObsTime
	DayHighF  float64   `json:"day_high_f"` // running high so far for LocalDate
	ObsTime   time.Time `json:"obs_time"`
	LocalDate string    `json:"local_date"` // YYYY-MM-DD in the station's tz
}

// metarOb maps the fields we use from an aviationweather.gov METAR JSON entry.
// temp/maxT are pointers so an absent reading is distinguishable from 0°C.
type metarOb struct {
	ICAOId  string   `json:"icaoId"`
	ObsTime int64    `json:"obsTime"` // unix seconds
	Temp    *float64 `json:"temp"`    // Celsius
	MaxT    *float64 `json:"maxT"`    // Celsius, 6-hour maximum (synoptic reports only)
	Name    string   `json:"name"`
}

type WeatherSource struct {
	simulated      bool
	stations       []WeatherStation
	pollInterval   time.Duration
	minCallSpacing time.Duration
	lookbackHours  int
	client         *http.Client

	lastCallAt time.Time // poll goroutine only, for pacing
	locs       map[string]*time.Location
	seen       map[string]bool // "ICAO|obsTime" -> already emitted
	seenOrder  []string        // FIFO for bounded eviction

	// Phase 2 forward corpus: per-station last-seen local date + the realized high at
	// that date, so a day rollover finalizes the prior day's (forecast, realized) pair.
	// Poll-goroutine only. recorder is nil when Phase 2 corpus logging is off.
	recorder *CorpusRecorder
	lastDate map[string]string
	lastHigh map[string]float64

	// metrics snapshot: written by the poll goroutine, read by the monitor HTTP
	// goroutine — guarded by mu.
	mu         sync.Mutex
	obsSeen    int64
	lastObsAt  time.Time
	lastPollAt time.Time
	lastError  string
	highs      map[string]float64 // ICAO -> latest computed local-day high (°F), for the status card
}

// NewWeatherSource builds the METAR source. Empty fields fall back to the full
// station set / a 10-minute cadence / a 26-hour lookback.
func NewWeatherSource(cfg WeatherConfig) *WeatherSource {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 10 * time.Minute
	}
	if cfg.MaxCallsPerMin <= 0 {
		cfg.MaxCallsPerMin = 15
	}
	if cfg.LookbackHours <= 0 {
		cfg.LookbackHours = 26 // spans since local midnight for any US tz, with margin
	}
	if len(cfg.Stations) == 0 {
		cfg.Stations = defaultWeatherStations
	}
	locs := make(map[string]*time.Location, len(cfg.Stations))
	for _, s := range cfg.Stations {
		if _, ok := locs[s.TZ]; ok {
			continue
		}
		loc, err := time.LoadLocation(s.TZ)
		if err != nil {
			log.Printf("[WARNING] Weather: unknown timezone %q for %s; falling back to UTC (local-day high may be off)", s.TZ, s.ICAO)
			loc = time.UTC
		}
		locs[s.TZ] = loc
	}
	return &WeatherSource{
		simulated:      cfg.Simulated,
		stations:       cfg.Stations,
		pollInterval:   cfg.PollInterval,
		minCallSpacing: time.Minute / time.Duration(cfg.MaxCallsPerMin),
		lookbackHours:  cfg.LookbackHours,
		client:         &http.Client{Timeout: 20 * time.Second},
		locs:           locs,
		seen:           make(map[string]bool),
		highs:          make(map[string]float64),
		lastDate:       make(map[string]string),
		lastHigh:       make(map[string]float64),
	}
}

// SetCorpusRecorder enables Phase 2 forward-corpus logging: on each local-day rollover
// the source finalizes the prior day's realized high and joins it with the recorded NWS
// forecast. Call before Start.
func (w *WeatherSource) SetCorpusRecorder(cr *CorpusRecorder) { w.recorder = cr }

// WeatherMetrics is a point-in-time view for the status dashboard's External APIs card.
type WeatherMetrics struct {
	Live           bool               `json:"live"`
	ObsSeen        int64              `json:"obs_seen"`
	LastObsAt      time.Time          `json:"last_obs_at"`
	LastPollAt     time.Time          `json:"last_poll_at"`
	LastError      string             `json:"last_error"`
	PollInterval   string             `json:"poll_interval"`
	Stations       string             `json:"stations"`
	DayHighs       map[string]float64 `json:"day_highs"` // ICAO -> latest local-day high (°F)
	MaxCallsPerMin int                `json:"max_calls_per_min"`
}

// Metrics returns a snapshot of the source's live health.
func (w *WeatherSource) Metrics() WeatherMetrics {
	w.mu.Lock()
	defer w.mu.Unlock()
	icaos := make([]string, 0, len(w.stations))
	for _, s := range w.stations {
		icaos = append(icaos, s.ICAO)
	}
	highs := make(map[string]float64, len(w.highs))
	for k, v := range w.highs {
		highs[k] = v
	}
	return WeatherMetrics{
		Live:           !w.simulated,
		ObsSeen:        w.obsSeen,
		LastObsAt:      w.lastObsAt,
		LastPollAt:     w.lastPollAt,
		LastError:      w.lastError,
		PollInterval:   w.pollInterval.String(),
		Stations:       strings.Join(icaos, ","),
		DayHighs:       highs,
		MaxCallsPerMin: int(time.Minute / w.minCallSpacing),
	}
}

func (w *WeatherSource) Start(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if w.simulated {
		log.Println("[INFO] Weather source is SIMULATED/idle; nothing to poll.")
		<-ctx.Done()
		return nil
	}

	icaos := make([]string, 0, len(w.stations))
	for _, s := range w.stations {
		icaos = append(icaos, s.ICAO)
	}
	log.Printf("[INFO] Weather (METAR) Ingestion Source started in LIVE mode: stations %v, poll every %v, ≤%d calls/min (aviationweather.gov, keyless)",
		icaos, w.pollInterval, int(time.Minute/w.minCallSpacing))

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	w.pollAll(ctx, output)
	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Weather Ingestion Source shutting down...")
			return nil
		case <-ticker.C:
			w.pollAll(ctx, output)
		}
	}
}

// pollAll polls each station once, paced under the calls/min limit.
func (w *WeatherSource) pollAll(ctx context.Context, output chan<- *GekkoIngestEvent) {
	for _, s := range w.stations {
		if ctx.Err() != nil {
			return
		}
		w.throttle(ctx)
		w.pollStation(ctx, s, output)
	}
}

// throttle enforces the minimum spacing between HTTP calls.
func (w *WeatherSource) throttle(ctx context.Context) {
	if wait := w.minCallSpacing - time.Since(w.lastCallAt); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	w.lastCallAt = time.Now()
}

func (w *WeatherSource) recordPoll(errMsg string) {
	w.mu.Lock()
	w.lastError = errMsg
	w.lastPollAt = time.Now()
	w.mu.Unlock()
}

func (w *WeatherSource) pollStation(ctx context.Context, s WeatherStation, output chan<- *GekkoIngestEvent) {
	q := url.Values{}
	q.Set("ids", s.ICAO)
	q.Set("format", "json")
	q.Set("hours", fmt.Sprintf("%d", w.lookbackHours))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, weatherAPIBase+"?"+q.Encode(), nil)
	if err != nil {
		log.Printf("[ERROR] Weather build request for %s: %v", s.ICAO, err)
		return
	}
	resp, err := w.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Weather fetch %s failed: %v", s.ICAO, err)
		w.recordPoll(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("[ERROR] Weather API %s status %d: %s", s.ICAO, resp.StatusCode, body)
		w.recordPoll(fmt.Sprintf("API %d", resp.StatusCode))
		return
	}
	var obs []metarOb
	if err := json.NewDecoder(resp.Body).Decode(&obs); err != nil {
		log.Printf("[ERROR] Weather decode %s: %v", s.ICAO, err)
		w.recordPoll("decode error")
		return
	}
	w.recordPoll("")

	loc := w.locs[s.TZ]
	if loc == nil {
		loc = time.UTC
	}
	latest, dayHigh, localDate, ok := reduceObs(obs, s.ICAO, loc, time.Now())
	if !ok {
		return // no usable temperature reading in the window yet
	}

	w.mu.Lock()
	w.highs[s.ICAO] = dayHigh
	w.mu.Unlock()

	// Phase 2 rollover: when today's local date has advanced past the last one we saw
	// for this station, the prior day is resolved — finalize its (forecast, realized)
	// pair. Runs every poll (before the dedupe return) so it fires even with no new obs.
	if w.recorder != nil {
		if prev := w.lastDate[s.ICAO]; prev != "" && localDate > prev {
			w.recorder.RecordResolved(s.ICAO, prev, w.lastHigh[s.ICAO])
		}
		w.lastDate[s.ICAO] = localDate
		w.lastHigh[s.ICAO] = dayHigh
	}

	key := s.ICAO + "|" + fmt.Sprintf("%d", latest.ObsTime)
	if w.seen[key] {
		return // already emitted this observation; the high map above still refreshed
	}
	w.markSeen(key)

	obsTime := time.Unix(latest.ObsTime, 0).UTC()
	tempF := cToF(*latest.Temp)
	payload := WeatherObs{
		Station:   s.ICAO,
		City:      s.City,
		TempF:     round1(tempF),
		DayHighF:  round1(dayHigh),
		ObsTime:   obsTime,
		LocalDate: localDate,
	}
	raw, err := json.Marshal(struct {
		WeatherObs
		Sentence string `json:"sentence"`
	}{payload, weatherSentence(payload, loc)})
	if err != nil {
		return
	}
	event := &GekkoIngestEvent{
		SourceID:             SourceWeather,
		RawContent:           string(raw),
		SourceURL:            weatherSourceRef + "/data/metar?ids=" + s.ICAO,
		ExtractedTimestampNS: obsTime.UnixNano(),
	}
	event.PayloadHash = event.ComputeHash()

	select {
	case output <- event:
	case <-ctx.Done():
		return
	default:
		log.Println("[WARNING] ingest channel full, dropping Weather observation")
		return
	}

	w.mu.Lock()
	w.obsSeen++
	w.lastObsAt = time.Now()
	w.mu.Unlock()
	log.Printf("[INFO] Weather %s (%s): %.1f°F now, local-day high %.1f°F", s.ICAO, s.City, round1(tempF), round1(dayHigh))
}

// reduceObs picks the latest observation that carries a temperature and computes the
// running high over the station's current local day, folding in the 6-hour max group
// where present. Pure given `now` — unit-tested directly. ok=false when no usable
// reading exists yet for today.
func reduceObs(obs []metarOb, icao string, loc *time.Location, now time.Time) (latest metarOb, dayHighF float64, localDate string, ok bool) {
	today := now.In(loc).Format("2006-01-02")
	localDate = today
	// Newest first, so the first temp-bearing ob on today's date is the latest reading.
	sort.Slice(obs, func(i, j int) bool { return obs[i].ObsTime > obs[j].ObsTime })

	highC := -1e9
	haveHigh := false
	for _, o := range obs {
		if o.ICAOId != "" && !strings.EqualFold(o.ICAOId, icao) {
			continue
		}
		if o.ObsTime <= 0 {
			continue
		}
		if time.Unix(o.ObsTime, 0).In(loc).Format("2006-01-02") != today {
			continue
		}
		if o.Temp != nil {
			if !ok {
				latest, ok = o, true // first (newest) today ob with a temperature
			}
			if *o.Temp > highC {
				highC, haveHigh = *o.Temp, true
			}
		}
		// The 6-hour max group captures an intra-window peak the hourly temps may miss.
		// It covers the six hours BEFORE the ob, so the first group after local midnight
		// is mostly yesterday evening — only fold it in when its window began today.
		if o.MaxT != nil && *o.MaxT > highC &&
			time.Unix(o.ObsTime-6*3600, 0).In(loc).Format("2006-01-02") == today {
			highC, haveHigh = *o.MaxT, true
		}
	}
	if !ok || !haveHigh {
		return metarOb{}, 0, localDate, false
	}
	return latest, cToF(highC), localDate, true
}

// weatherSentence renders a readable, embeddable/keyword-matchable line.
func weatherSentence(o WeatherObs, loc *time.Location) string {
	return fmt.Sprintf("%s (%s) temperature %.1f°F at %s; local-day high so far %.1f°F. Source: NOAA/aviationweather.gov.",
		o.City, o.Station, o.TempF, o.ObsTime.In(loc).Format("2006-01-02 15:04 MST"), o.DayHighF)
}

func (w *WeatherSource) markSeen(key string) {
	w.seen[key] = true
	w.seenOrder = append(w.seenOrder, key)
	if len(w.seenOrder) > maxSeenWeather {
		old := w.seenOrder[0]
		w.seenOrder = w.seenOrder[1:]
		delete(w.seen, old)
	}
}

func cToF(c float64) float64 { return c*9.0/5.0 + 32.0 }

func round1(f float64) float64 { return float64(int64(f*10+sign(f)*0.5)) / 10 }

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}
