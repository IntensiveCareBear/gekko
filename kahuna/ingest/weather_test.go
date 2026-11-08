package ingest

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// TestWeatherSource_Live hits the real aviationweather.gov API. Opt-in (network),
// run with WEATHER_LIVE_TEST=1 — doubles as a manual smoke tool.
func TestWeatherSource_Live(t *testing.T) {
	if os.Getenv("WEATHER_LIVE_TEST") == "" {
		t.Skip("set WEATHER_LIVE_TEST=1 to run the live aviationweather.gov check")
	}
	w := NewWeatherSource(WeatherConfig{Stations: []WeatherStation{{"KNYC", "New York City", "America/New_York", 40.78, -73.97}}})
	out := make(chan *GekkoIngestEvent, 1)
	w.pollStation(context.Background(), w.stations[0], out)
	select {
	case ev := <-out:
		var p WeatherObs
		if err := json.Unmarshal([]byte(ev.RawContent), &p); err != nil {
			t.Fatalf("payload: %v", err)
		}
		t.Logf("emitted: %s | temp=%.1f high=%.1f date=%s url=%s", ev.RawContent, p.TempF, p.DayHighF, p.LocalDate, ev.SourceURL)
		if p.TempF < -60 || p.TempF > 140 || p.DayHighF < p.TempF-0.05 {
			t.Errorf("implausible reading: temp=%.1f high=%.1f", p.TempF, p.DayHighF)
		}
	default:
		t.Log("no event emitted (station may have no observation for today yet)")
	}
}

func fptr(f float64) *float64 { return &f }

func TestCToF(t *testing.T) {
	cases := []struct{ c, f float64 }{{0, 32}, {100, 212}, {37, 98.6}, {-40, -40}}
	for _, c := range cases {
		if got := cToF(c.c); math.Abs(got-c.f) > 0.001 {
			t.Errorf("cToF(%v)=%v, want %v", c.c, got, c.f)
		}
	}
}

// reduceObs must pick the latest today observation as the current reading, take the
// running high across today's obs (folding in the 6-hour maxT group), and ignore
// yesterday's obs and null temperatures.
func TestReduceObs(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// "now" = 2026-07-10 16:00 ET.
	now := time.Date(2026, 7, 10, 16, 0, 0, 0, loc)
	mk := func(hourET int, temp, maxT *float64) metarOb {
		ts := time.Date(2026, 7, 10, hourET, 51, 0, 0, loc)
		return metarOb{ICAOId: "KNYC", ObsTime: ts.Unix(), Temp: temp, MaxT: maxT}
	}
	yesterday := metarOb{ICAOId: "KNYC", ObsTime: time.Date(2026, 7, 9, 14, 51, 0, 0, loc).Unix(), Temp: fptr(40)} // 104°F, must be ignored

	obs := []metarOb{
		mk(9, fptr(24), nil),           // 75.2°F
		mk(14, fptr(28), fptr(29)),     // 82.4°F reading, 84.2°F 6h-max
		mk(15, fptr(27), nil),          // 80.6°F (latest so far)
		{ICAOId: "KNYC", ObsTime: mk(13, nil, nil).ObsTime, Temp: nil}, // null temp, skipped
		yesterday,
	}
	latest, dayHighF, localDate, ok := reduceObs(obs, "KNYC", loc, now)
	if !ok {
		t.Fatal("expected ok")
	}
	if localDate != "2026-07-10" {
		t.Errorf("localDate=%q, want 2026-07-10", localDate)
	}
	// Latest today reading is the 15:51 ob (27°C).
	if latest.Temp == nil || math.Abs(*latest.Temp-27) > 0.001 {
		t.Errorf("latest temp = %v, want 27", latest.Temp)
	}
	// Running high folds in the 14:51 6-hour max group (29°C -> 84.2°F), beating the
	// hourly readings and yesterday's ignored 40°C.
	if math.Abs(dayHighF-cToF(29)) > 0.05 {
		t.Errorf("dayHighF=%v, want %v (from maxT 29°C)", dayHighF, cToF(29))
	}
}

// KAUS 2026-10-01: the 00:53 CDT ob carried a 6-hour max of 32.8°C (91°F) — yesterday's
// 18:53 reading — and the day-high sat at 91°F all night against a real 82°F.
func TestReduceObs_MaxGroupStraddlingMidnight(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Date(2026, 10, 1, 3, 0, 0, 0, loc)
	obs := []metarOb{
		{ICAOId: "KAUS", ObsTime: time.Date(2026, 10, 1, 0, 53, 0, 0, loc).Unix(), Temp: fptr(27.8), MaxT: fptr(32.8)},
		{ICAOId: "KAUS", ObsTime: time.Date(2026, 10, 1, 2, 53, 0, 0, loc).Unix(), Temp: fptr(27.2)},
	}
	_, dayHighF, _, ok := reduceObs(obs, "KAUS", loc, now)
	if !ok || math.Abs(dayHighF-cToF(27.8)) > 0.05 {
		t.Errorf("dayHighF=%v ok=%v, want %v (yesterday's 6h max must not count)", dayHighF, ok, cToF(27.8))
	}
}

func TestReduceObs_NoTodayData(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 7, 10, 16, 0, 0, 0, loc)
	obs := []metarOb{{ICAOId: "KNYC", ObsTime: time.Date(2026, 7, 9, 14, 0, 0, 0, loc).Unix(), Temp: fptr(20)}}
	if _, _, _, ok := reduceObs(obs, "KNYC", loc, now); ok {
		t.Error("expected ok=false when no observation falls on today")
	}
}

func TestParseWeatherStations(t *testing.T) {
	if got := ParseWeatherStations(""); len(got) != len(defaultWeatherStations) {
		t.Errorf("empty spec: got %d stations, want default %d", len(got), len(defaultWeatherStations))
	}
	got := ParseWeatherStations("KNYC:New York City:America/New_York, KBAD, KDEN:Denver:America/Denver")
	if len(got) != 2 { // the malformed "KBAD" (no city/tz) is skipped
		t.Fatalf("got %d stations, want 2 (KBAD skipped)", len(got))
	}
	if got[0].ICAO != "KNYC" || got[0].City != "New York City" || got[0].TZ != "America/New_York" {
		t.Errorf("bad first station: %+v", got[0])
	}
	if got[1].ICAO != "KDEN" {
		t.Errorf("bad second station: %+v", got[1])
	}
	// An all-malformed spec falls back to defaults rather than emptying the source.
	if got := ParseWeatherStations("garbage,also:bad"); len(got) != len(defaultWeatherStations) {
		t.Errorf("all-malformed: got %d, want default", len(got))
	}
}
