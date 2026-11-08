package ingest

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestWeatherForecastSource_Live drives the real NWS two-step resolve+fetch through the
// source and confirms a plausible forecast lands in the store and an event is emitted.
// Opt-in (network): NWS_LIVE_TEST=1.
func TestWeatherForecastSource_Live(t *testing.T) {
	if os.Getenv("NWS_LIVE_TEST") == "" {
		t.Skip("set NWS_LIVE_TEST=1 to run the live api.weather.gov check")
	}
	dir := t.TempDir()
	store := NewForecastStore(dir + "/forecasts.json")
	src := NewWeatherForecastSource(WeatherForecastConfig{
		Enabled:  true,
		Stations: []WeatherStation{{"KNYC", "New York City", "America/New_York", 40.78, -73.97}},
		Store:    store,
	})
	out := make(chan *GekkoIngestEvent, 4)
	src.pollStation(context.Background(), src.stations[0], out)

	today := time.Now().In(time.FixedZone("ET", -4*3600)).Format("2006-01-02")
	found := false
	for _, d := range []string{today} {
		if r, ok := store.Get("KNYC", d); ok {
			t.Logf("forecast KNYC %s = %.0f°F", d, r.HighF)
			if r.HighF > -60 && r.HighF < 140 {
				found = true
			}
		}
	}
	if !found {
		t.Error("no plausible forecast stored for today")
	}
	select {
	case ev := <-out:
		t.Logf("emitted forecast event: %s", ev.RawContent)
	default:
		t.Log("no event emitted (today's daytime period may have passed)")
	}
}

func TestForecastHighsByDate(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	var fr nwsForecastResp
	fr.Properties.Periods = []struct {
		IsDaytime       bool    `json:"isDaytime"`
		Temperature     float64 `json:"temperature"`
		TemperatureUnit string  `json:"temperatureUnit"`
		StartTime       string  `json:"startTime"`
	}{
		{true, 81, "F", "2026-07-11T12:00:00-04:00"},
		{false, 68, "F", "2026-07-11T18:00:00-04:00"}, // night ignored
		{true, 84, "F", "2026-07-12T06:00:00-04:00"},
		{true, 20, "C", "2026-07-13T06:00:00-04:00"}, // Celsius converted
	}
	got := forecastHighsByDate(fr, loc)
	if got["2026-07-11"] != 81 {
		t.Errorf("Jul11 = %v, want 81", got["2026-07-11"])
	}
	if got["2026-07-12"] != 84 {
		t.Errorf("Jul12 = %v, want 84", got["2026-07-12"])
	}
	if c := got["2026-07-13"]; c < 67.9 || c > 68.1 { // 20C = 68F
		t.Errorf("Jul13 (C→F) = %v, want ~68", c)
	}
	if _, ok := got["2026-07-11night"]; ok {
		t.Error("night period should not create an entry")
	}
}

func TestParseWeatherStations_CoordsInherited(t *testing.T) {
	// 3-field spec for a known ICAO inherits coordinates from stationCoords.
	st := ParseWeatherStations("KNYC:New York City:America/New_York")
	if len(st) != 1 || !st[0].HasCoords() {
		t.Fatalf("known 3-field station should inherit coords: %+v", st)
	}
	// 5-field spec sets explicit coords.
	st = ParseWeatherStations("KZZZ:Nowhere:America/New_York:12.5:-34.25")
	if len(st) != 1 || st[0].Lat != 12.5 || st[0].Lon != -34.25 {
		t.Fatalf("explicit coords not parsed: %+v", st)
	}
	// Unknown 3-field ICAO has no coords ⇒ forecast skips it.
	st = ParseWeatherStations("KZZZ:Nowhere:America/New_York")
	if len(st) != 1 || st[0].HasCoords() {
		t.Fatalf("unknown 3-field station should have no coords: %+v", st)
	}
}

func TestForecastStore_PersistAndGet(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/forecasts.json"
	s := NewForecastStore(path)
	s.Put(ForecastRecord{Station: "KNYC", Date: "2026-07-11", HighF: 81, CapturedAt: time.Now()})

	// Reload from disk — the record survives a restart.
	s2 := NewForecastStore(path)
	r, ok := s2.Get("KNYC", "2026-07-11")
	if !ok || r.HighF != 81 {
		t.Fatalf("persisted forecast not reloaded: %+v ok=%v", r, ok)
	}
	if _, ok := s2.Get("KNYC", "2026-07-12"); ok {
		t.Error("unexpected forecast for a date never stored")
	}
}
