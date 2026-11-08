package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFREDSource_ParsesAndDedupes drives the observations poll against a mock FRED
// API and asserts it emits a readable, embeddable event with the change vs prior,
// dedupes by observation date, and attributes provenance to api.stlouisfed.org.
func TestFREDSource_ParsesAndDedupes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") == "" {
			t.Error("expected api_key query param")
		}
		_ = json.NewEncoder(w).Encode(fredObservationsResp{
			Observations: []struct {
				Date  string `json:"date"`
				Value string `json:"value"`
			}{
				{Date: "2026-06-01", Value: "4.1"},
				{Date: "2026-05-01", Value: "4.2"},
			},
		})
	}))
	defer srv.Close()

	oldURL := fredObservationsURL
	fredObservationsURL = srv.URL
	defer func() { fredObservationsURL = oldURL }()

	f := NewFREDSource(FREDConfig{APIKey: "k", Series: []string{"UNRATE"}, MaxCallsPerMin: 600})
	out := make(chan *GekkoIngestEvent, 4)
	ctx := context.Background()

	f.pollSeries(ctx, "UNRATE", out)
	if got := len(out); got != 1 {
		t.Fatalf("first poll: expected 1 event, got %d", got)
	}
	ev := <-out
	if ev.SourceID != SourceFRED {
		t.Errorf("SourceID = %q, want FRED", ev.SourceID)
	}
	if !strings.HasPrefix(ev.SourceURL, "https://api.stlouisfed.org") {
		t.Errorf("SourceURL should be attributed to api.stlouisfed.org, got %q", ev.SourceURL)
	}
	if ev.PayloadHash != ev.ComputeHash() {
		t.Error("payload hash mismatch (would be quarantined)")
	}
	// Readable sentence carries the value, direction, and keyword-matchable terms.
	for _, want := range []string{"Unemployment Rate", "4.1", "down from", "4.2", "-0.1", "2026-06-01"} {
		if !strings.Contains(ev.RawContent, want) {
			t.Errorf("RawContent %q missing %q", ev.RawContent, want)
		}
	}

	// Re-poll the same latest observation: no new event (dedupe by series|date).
	f.pollSeries(ctx, "UNRATE", out)
	if got := len(out); got != 0 {
		t.Errorf("second poll: expected 0 new events (deduped), got %d", got)
	}
}

// TestFREDSource_SkipsMissingValue ensures a "." (missing) latest value emits nothing.
func TestFREDSource_SkipsMissingValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fredObservationsResp{
			Observations: []struct {
				Date  string `json:"date"`
				Value string `json:"value"`
			}{{Date: "2026-06-01", Value: "."}},
		})
	}))
	defer srv.Close()
	oldURL := fredObservationsURL
	fredObservationsURL = srv.URL
	defer func() { fredObservationsURL = oldURL }()

	f := NewFREDSource(FREDConfig{APIKey: "k", Series: []string{"CPIAUCSL"}, MaxCallsPerMin: 600})
	out := make(chan *GekkoIngestEvent, 2)
	f.pollSeries(context.Background(), "CPIAUCSL", out)
	if got := len(out); got != 0 {
		t.Errorf("expected 0 events for missing value, got %d", got)
	}
}

// TestFREDCalendarFrom_MatchesFutureRecognizedReleases checks the pure calendar
// builder: it keeps only recognized releases in the future window, attaches the
// known ET publish time, and returns them sorted by time.
func TestFREDCalendarFrom_MatchesFutureRecognizedReleases(t *testing.T) {
	f := NewFREDSource(FREDConfig{APIKey: "k", CalendarDays: 30})
	// Anchor "now" so date math is deterministic regardless of when the test runs.
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, f.eastern)

	data := fredReleaseDatesResp{ReleaseDates: []struct {
		ReleaseID   int    `json:"release_id"`
		ReleaseName string `json:"release_name"`
		Date        string `json:"date"`
	}{
		{ReleaseID: 10, ReleaseName: "Consumer Price Index", Date: "2026-07-15"}, // future, recognized
		{ReleaseID: 50, ReleaseName: "Employment Situation", Date: "2026-07-03"}, // future, recognized (earlier)
		{ReleaseID: 99, ReleaseName: "Weekly Widget Survey", Date: "2026-07-10"}, // unrecognized → dropped
		{ReleaseID: 10, ReleaseName: "Consumer Price Index", Date: "2026-06-15"}, // past → dropped
		{ReleaseID: 53, ReleaseName: "Gross Domestic Product", Date: "2026-09-01"}, // beyond horizon → dropped
	}}

	events := f.calendarFrom(data, now)
	if len(events) != 2 {
		t.Fatalf("expected 2 recognized future events, got %d: %+v", len(events), events)
	}
	// Sorted by time: Employment Situation (Jul 3) before CPI (Jul 15).
	if events[0].Title != "Employment Situation" || events[1].Title != "Consumer Price Index" {
		t.Errorf("events not sorted by time: %q then %q", events[0].Title, events[1].Title)
	}
	// CPI publishes 08:30 ET; verify the wall-clock time-of-day in Eastern.
	cpi := events[1].Time.In(f.eastern)
	if cpi.Hour() != 8 || cpi.Minute() != 30 {
		t.Errorf("CPI publish time = %02d:%02d ET, want 08:30", cpi.Hour(), cpi.Minute())
	}
	if len(events[1].Keywords) == 0 {
		t.Error("recognized release should carry blackout keywords")
	}
}

// TestFREDCalendarJSON_DefaultEmpty verifies CalendarJSON is a valid empty array
// before any build (the monitor serves it verbatim to Gordon).
func TestFREDCalendarJSON_DefaultEmpty(t *testing.T) {
	f := NewFREDSource(FREDConfig{APIKey: "k"})
	if got := string(f.CalendarJSON()); got != "[]" {
		t.Errorf("CalendarJSON default = %q, want []", got)
	}
	var arr []EconCalendarEvent
	if err := json.Unmarshal(f.CalendarJSON(), &arr); err != nil {
		t.Errorf("default CalendarJSON not valid JSON array: %v", err)
	}
}

// TestFREDSource_IdleWithoutKey verifies live start with no key exits cleanly on cancel.
func TestFREDSource_IdleWithoutKey(t *testing.T) {
	f := NewFREDSource(FREDConfig{APIKey: ""})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Start(ctx, make(chan *GekkoIngestEvent, 1)) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("idle Start returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle Start did not return after cancel")
	}
}
