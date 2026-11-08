package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FRED (Federal Reserve Bank of St. Louis) is a free, keyed API serving official
// macro time series — CPI, unemployment, fed funds, payrolls, GDP. Unlike the
// news feeds this is *numeric ground truth* for the macro/econ markets Gekko
// trades, so it feeds the decision layer real prints instead of headlines about
// them. Free tier is generous (~120 req/min); we pace well under it.
//
// The source does double duty:
//  1. Emits a GekkoIngestEvent whenever a watched series posts a new observation.
//  2. Builds a release-schedule calendar (from FRED's releases/dates endpoint)
//     that kahuna's monitor serves for Gordon's econ-blackout window — turning the
//     previously no-op ECON_CALENDAR_URL feature into a live feed.
var (
	fredObservationsURL = "https://api.stlouisfed.org/fred/series/observations"
	fredReleaseDatesURL = "https://api.stlouisfed.org/fred/releases/dates"
)

// fredSourceRef is the compliance-authorized provenance host for emitted events.
const fredSourceRef = "https://api.stlouisfed.org"

const maxSeenFRED = 2000

// seriesMeta gives a human title + unit suffix for a watched series id so emitted
// events read as sentences (good for embedding + keyword matching + the LLM).
type seriesMeta struct {
	Title string
	Unit  string // e.g. "%", " (index)", "K"; "" when unitless
}

// defaultSeriesMeta covers the default macro basket. Unknown series fall back to
// the raw id with no unit.
var defaultSeriesMeta = map[string]seriesMeta{
	"CPIAUCSL": {"U.S. Consumer Price Index (CPI)", " (index)"},
	"UNRATE":   {"U.S. Unemployment Rate", "%"},
	"FEDFUNDS": {"U.S. Federal Funds Effective Rate", "%"},
	"PAYEMS":   {"U.S. Nonfarm Payrolls", "K"},
	"GDPC1":    {"U.S. Real Gross Domestic Product", " (bil. chained $)"},
	"PCEPI":    {"U.S. PCE Price Index", " (index)"},
	"DGS10":    {"U.S. 10-Year Treasury Yield", "%"},
	"T10YIE":   {"U.S. 10-Year Breakeven Inflation Rate", "%"},
	"CPILFESL": {"U.S. Core CPI (ex food & energy)", " (index)"},
	"DGS2":     {"U.S. 2-Year Treasury Yield", "%"},
	"T10Y2Y":   {"U.S. 10Y-2Y Treasury Spread (recession signal)", "%"},
	"ICSA":     {"U.S. Initial Jobless Claims", ""},
	"VIXCLS":   {"CBOE Volatility Index (VIX)", ""},
	"UMCSENT":  {"U.S. Consumer Sentiment (Michigan)", " (index)"},
}

// releaseSchedule maps a FRED release-name substring to the well-known Eastern-time
// publish slot + Gordon blackout keywords. FRED's releases/dates endpoint gives us
// real, live release *dates*; the time-of-day is a stable BLS/Fed convention (CPI,
// PPI, jobs, GDP, PCE all print 08:30 ET; FOMC statements 14:00 ET). Matching by
// name (not numeric release_id) keeps this robust to id changes.
type releaseSlot struct {
	Hour, Min int
	Keywords  []string
}

var releaseSchedule = map[string]releaseSlot{
	"Consumer Price Index":        {8, 30, []string{"cpi", "inflation", "consumer price"}},
	"Producer Price Index":        {8, 30, []string{"ppi", "producer price", "inflation"}},
	"Employment Situation":        {8, 30, []string{"jobs", "payroll", "employment", "unemployment", "nonfarm"}},
	"Gross Domestic Product":      {8, 30, []string{"gdp", "gross domestic"}},
	"Personal Income and Outlays": {8, 30, []string{"pce", "personal income", "inflation"}},
	"Federal Open Market":         {14, 0, []string{"fomc", "fed", "interest rate", "federal reserve", "rate decision"}},
}

// FREDConfig configures the FRED numeric-data ingestion source.
type FREDConfig struct {
	Simulated      bool
	APIKey         string
	Series         []string      // series ids to watch, e.g. ["CPIAUCSL","UNRATE"]
	PollInterval   time.Duration // how often to re-check each series for a new print
	MaxCallsPerMin int           // pacing guard (FRED free ≈ 120/min)
	CalendarDays   int           // how many days ahead to include in the release calendar
}

// EconCalendarEvent mirrors the JSON shape Gordon's EconCalendar consumes
// (gordon/external.go EconEvent): {time, title, keywords}.
type EconCalendarEvent struct {
	Time     time.Time `json:"time"`
	Title    string    `json:"title"`
	Keywords []string  `json:"keywords"`
}

// fredObservationsResp / fredReleaseDatesResp map the fields we use.
type fredObservationsResp struct {
	Observations []struct {
		Date  string `json:"date"`
		Value string `json:"value"` // "." for missing
	} `json:"observations"`
}

type fredReleaseDatesResp struct {
	ReleaseDates []struct {
		ReleaseID   int    `json:"release_id"`
		ReleaseName string `json:"release_name"`
		Date        string `json:"date"` // YYYY-MM-DD (Eastern calendar date)
	} `json:"release_dates"`
}

type FREDSource struct {
	simulated      bool
	apiKey         string
	series         []string
	pollInterval   time.Duration
	minCallSpacing time.Duration
	calendarDays   int
	client         *http.Client // observations (small, fast)
	calClient      *http.Client // releases/dates (heavier payload, needs more headroom)
	eastern        *time.Location

	lastCallAt time.Time       // poll goroutine only, for pacing
	seen       map[string]bool // "series|date" -> already emitted
	seenOrder  []string        // FIFO for bounded eviction

	// metrics + calendar snapshot: written by the poll goroutine, read by the
	// monitor HTTP goroutine — guarded by mu.
	mu            sync.Mutex
	releasesSeen  int64
	lastRelease   string
	lastReleaseAt time.Time
	lastPollAt    time.Time
	lastError     string
	calendarJSON  []byte
	calendarSize  int
	calendarError string
}

func (f *FREDSource) recordCalendarErr(msg string) {
	f.mu.Lock()
	f.calendarError = msg
	f.mu.Unlock()
}

// NewFREDSource builds the FRED source. Live when an API key is present; otherwise
// idle. Empty series/interval fall back to a macro basket / 30-minute cadence.
func NewFREDSource(cfg FREDConfig) *FREDSource {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Minute
	}
	if cfg.MaxCallsPerMin <= 0 {
		cfg.MaxCallsPerMin = 60 // half of FRED's ~120/min, ample headroom
	}
	if cfg.CalendarDays <= 0 {
		cfg.CalendarDays = 14
	}
	if len(cfg.Series) == 0 {
		cfg.Series = []string{"CPIAUCSL", "UNRATE", "FEDFUNDS", "PAYEMS", "GDPC1"}
	}
	// America/New_York gives correct EST/EDT offsets across DST automatically; fall
	// back to a fixed -05:00 if tzdata is unavailable in the runtime image.
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.FixedZone("ET", -5*3600)
	}
	return &FREDSource{
		simulated:      cfg.Simulated,
		apiKey:         cfg.APIKey,
		series:         cfg.Series,
		pollInterval:   cfg.PollInterval,
		minCallSpacing: time.Minute / time.Duration(cfg.MaxCallsPerMin),
		calendarDays:   cfg.CalendarDays,
		client:         &http.Client{Timeout: 15 * time.Second},
		calClient:      &http.Client{Timeout: 45 * time.Second},
		eastern:        loc,
		seen:           make(map[string]bool),
	}
}

// FREDMetrics is a point-in-time view for the status dashboard's External APIs card.
type FREDMetrics struct {
	Live           bool      `json:"live"`
	ReleasesSeen   int64     `json:"releases_seen"`
	LastRelease    string    `json:"last_release"`
	LastReleaseAt  time.Time `json:"last_release_at"`
	LastPollAt     time.Time `json:"last_poll_at"`
	LastError      string    `json:"last_error"`
	PollInterval   string    `json:"poll_interval"`
	Series         string    `json:"series"`
	CalendarEvents int       `json:"calendar_events"`
	CalendarError  string    `json:"calendar_error"`
	MaxCallsPerMin int       `json:"max_calls_per_min"`
}

// Metrics returns a snapshot of the source's live health.
func (f *FREDSource) Metrics() FREDMetrics {
	f.mu.Lock()
	defer f.mu.Unlock()
	return FREDMetrics{
		Live:           !f.simulated,
		ReleasesSeen:   f.releasesSeen,
		LastRelease:    f.lastRelease,
		LastReleaseAt:  f.lastReleaseAt,
		LastPollAt:     f.lastPollAt,
		LastError:      f.lastError,
		PollInterval:   f.pollInterval.String(),
		Series:         strings.Join(f.series, ","),
		CalendarEvents: f.calendarSize,
		CalendarError:  f.calendarError,
		MaxCallsPerMin: int(time.Minute / f.minCallSpacing),
	}
}

// CalendarJSON returns the latest release-calendar snapshot in Gordon's EconEvent
// shape (a JSON array). Empty until the first successful build. Safe for the
// monitor HTTP goroutine to call concurrently.
func (f *FREDSource) CalendarJSON() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calendarJSON) == 0 {
		return []byte("[]")
	}
	out := make([]byte, len(f.calendarJSON))
	copy(out, f.calendarJSON)
	return out
}

func (f *FREDSource) Start(ctx context.Context, output chan<- *GekkoIngestEvent) error {
	if f.simulated || f.apiKey == "" {
		log.Println("[INFO] FRED source is SIMULATED/idle (no API key); nothing to poll.")
		<-ctx.Done()
		return nil
	}

	log.Printf("[INFO] FRED Ingestion Source started in LIVE mode: series %v, poll every %v, ≤%d calls/min (free tier ≈120/min)",
		f.series, f.pollInterval, int(time.Minute/f.minCallSpacing))

	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()
	f.pollAll(ctx, output)
	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] FRED Ingestion Source shutting down...")
			return nil
		case <-ticker.C:
			f.pollAll(ctx, output)
		}
	}
}

// pollAll re-checks each series for a new print and rebuilds the release calendar,
// paced under the free-tier call limit.
func (f *FREDSource) pollAll(ctx context.Context, output chan<- *GekkoIngestEvent) {
	for _, s := range f.series {
		if ctx.Err() != nil {
			return
		}
		f.throttle(ctx)
		f.pollSeries(ctx, s, output)
	}
	if ctx.Err() != nil {
		return
	}
	f.throttle(ctx)
	f.buildCalendar(ctx)
}

// throttle enforces minimum spacing between HTTP calls.
func (f *FREDSource) throttle(ctx context.Context) {
	if wait := f.minCallSpacing - time.Since(f.lastCallAt); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	f.lastCallAt = time.Now()
}

func (f *FREDSource) recordPoll(errMsg string) {
	f.mu.Lock()
	f.lastError = errMsg
	f.lastPollAt = time.Now()
	f.mu.Unlock()
}

func (f *FREDSource) pollSeries(ctx context.Context, series string, output chan<- *GekkoIngestEvent) {
	q := url.Values{}
	q.Set("series_id", series)
	q.Set("api_key", f.apiKey)
	q.Set("file_type", "json")
	q.Set("sort_order", "desc")
	q.Set("limit", "2") // latest + prior, to compute the change
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fredObservationsURL+"?"+q.Encode(), nil)
	if err != nil {
		log.Printf("[ERROR] FRED build request: %v", err)
		return
	}
	resp, err := f.client.Do(req)
	if err != nil {
		msg := redactKey(err, f.apiKey)
		log.Printf("[ERROR] FRED fetch %s failed: %s", series, msg)
		f.recordPoll(msg)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Printf("[ERROR] FRED observations %s status %d: %s", series, resp.StatusCode, body)
		f.recordPoll(fmt.Sprintf("API %d", resp.StatusCode))
		return
	}
	var data fredObservationsResp
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("[ERROR] FRED decode %s: %v", series, err)
		f.recordPoll("decode error")
		return
	}
	f.recordPoll("")
	if len(data.Observations) == 0 {
		return
	}
	latest := data.Observations[0]
	val, ok := parseFredValue(latest.Value)
	if !ok {
		return // missing/"." value; nothing to report yet
	}
	key := series + "|" + latest.Date
	if f.seen[key] {
		return
	}
	f.markSeen(key)

	prev, hasPrev := math.NaN(), false
	if len(data.Observations) > 1 {
		if p, ok := parseFredValue(data.Observations[1].Value); ok {
			prev, hasPrev = p, true
		}
	}

	meta := f.metaFor(series)
	event := &GekkoIngestEvent{
		SourceID:             SourceFRED,
		RawContent:           fredSentence(meta, series, latest.Date, val, prev, hasPrev),
		SourceURL:            fmt.Sprintf("%s/graph/?id=%s", fredSourceRef, series),
		ExtractedTimestampNS: time.Now().UnixNano(),
	}
	event.PayloadHash = event.ComputeHash()

	select {
	case output <- event:
	case <-ctx.Done():
		return
	default:
		log.Println("[WARNING] ingest channel full, dropping FRED release")
		return
	}

	change := math.NaN()
	if hasPrev {
		change = val - prev
	}
	f.mu.Lock()
	f.releasesSeen++
	f.lastRelease = series
	f.lastReleaseAt = time.Now()
	f.mu.Unlock()
	log.Printf("[INFO] FRED %s new print for %s: %.4g (change %.4g)", series, latest.Date, val, change)
}

// buildCalendar fetches upcoming release dates and rebuilds the Gordon-shaped
// calendar snapshot, keeping only releases we recognize within the look-ahead window.
func (f *FREDSource) buildCalendar(ctx context.Context) {
	now := time.Now()
	q := url.Values{}
	q.Set("api_key", f.apiKey)
	q.Set("file_type", "json")
	q.Set("sort_order", "asc")
	q.Set("include_release_dates_with_no_data", "true") // needed for future/scheduled dates
	q.Set("realtime_start", now.Format("2006-01-02"))
	q.Set("realtime_end", now.AddDate(0, 0, f.calendarDays).Format("2006-01-02"))
	q.Set("limit", "1000")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fredReleaseDatesURL+"?"+q.Encode(), nil)
	if err != nil {
		return
	}
	resp, err := f.calClient.Do(req)
	if err != nil {
		msg := redactKey(err, f.apiKey)
		log.Printf("[WARN] FRED releases/dates fetch: %s", msg)
		f.recordCalendarErr("fetch: " + msg)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("[WARN] FRED releases/dates status %d: %s", resp.StatusCode, body)
		f.recordCalendarErr(fmt.Sprintf("API %d", resp.StatusCode))
		return
	}
	var data fredReleaseDatesResp
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("[WARN] FRED releases/dates decode: %v", err)
		f.recordCalendarErr("decode error")
		return
	}

	events := f.calendarFrom(data, now)
	blob, err := json.Marshal(events)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.calendarJSON = blob
	f.calendarSize = len(events)
	f.calendarError = ""
	f.mu.Unlock()
	if len(events) > 0 {
		log.Printf("[INFO] FRED econ calendar rebuilt: %d upcoming release(s) in next %dd.", len(events), f.calendarDays)
	}
}

// calendarFrom converts FRED release dates into recognized, future EconCalendarEvents
// sorted by time. Pure/deterministic given `now` — unit-tested directly.
func (f *FREDSource) calendarFrom(data fredReleaseDatesResp, now time.Time) []EconCalendarEvent {
	horizon := now.AddDate(0, 0, f.calendarDays)
	var events []EconCalendarEvent
	for _, rd := range data.ReleaseDates {
		slot, keywords, ok := matchRelease(rd.ReleaseName)
		if !ok {
			continue
		}
		day, err := time.ParseInLocation("2006-01-02", rd.Date, f.eastern)
		if err != nil {
			continue
		}
		when := time.Date(day.Year(), day.Month(), day.Day(), slot.Hour, slot.Min, 0, 0, f.eastern)
		if when.Before(now) || when.After(horizon) {
			continue
		}
		events = append(events, EconCalendarEvent{Time: when, Title: rd.ReleaseName, Keywords: keywords})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })
	return events
}

// matchRelease returns the publish slot + keywords for a recognized release name.
func matchRelease(name string) (releaseSlot, []string, bool) {
	for sub, slot := range releaseSchedule {
		if strings.Contains(name, sub) {
			return slot, slot.Keywords, true
		}
	}
	return releaseSlot{}, nil, false
}

func (f *FREDSource) metaFor(series string) seriesMeta {
	if m, ok := defaultSeriesMeta[series]; ok {
		return m
	}
	return seriesMeta{Title: series}
}

// fredSentence renders a readable, embeddable/keyword-matchable event line.
func fredSentence(meta seriesMeta, series, date string, val, prev float64, hasPrev bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) updated to %s%s for %s", meta.Title, series, formatNum(val), meta.Unit, date)
	if hasPrev {
		delta := val - prev
		dir := "unchanged from"
		if delta > 0 {
			dir = "up from"
		} else if delta < 0 {
			dir = "down from"
		}
		fmt.Fprintf(&b, ", %s prior %s%s (change %s)", dir, formatNum(prev), meta.Unit, formatSigned(delta))
	}
	b.WriteString(". Source: FRED, Federal Reserve Bank of St. Louis.")
	return b.String()
}

func (f *FREDSource) markSeen(key string) {
	f.seen[key] = true
	f.seenOrder = append(f.seenOrder, key)
	if len(f.seenOrder) > maxSeenFRED {
		old := f.seenOrder[0]
		f.seenOrder = f.seenOrder[1:]
		delete(f.seen, old)
	}
}

// parseFredValue parses a FRED observation value; "." (missing) returns ok=false.
func parseFredValue(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "." {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func formatNum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatSigned(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if v > 0 {
		return "+" + s
	}
	return s
}
