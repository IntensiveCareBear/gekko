package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFinnhubSource_ParsesAndDedupes drives the source against a mock Finnhub API and
// asserts it emits structured events, dedupes by article id, and attributes provenance
// to finnhub.io (so the compliance gatekeeper passes) while keeping the real URL in the
// payload.
func TestFinnhubSource_ParsesAndDedupes(t *testing.T) {
	items := []finnhubNewsItem{
		{ID: 1, Category: "general", Datetime: time.Now().Unix(), Headline: "Fed holds rates", Source: "Reuters", Summary: "The Fed left rates unchanged.", URL: "https://www.reuters.com/markets/fed"},
		{ID: 2, Category: "general", Datetime: time.Now().Unix(), Headline: "CPI cooler than expected", Source: "CNBC", Summary: "Inflation eased.", URL: "https://www.cnbc.com/cpi"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") == "" {
			t.Error("expected token query param")
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer srv.Close()

	f := NewFinnhubSource(FinnhubConfig{APIKey: "k", Categories: []string{"general"}, MaxCallsPerMin: 600})
	// Point the source at the mock server (drive pollCategory directly — Start loops).
	oldURL := finnhubNewsURL
	finnhubNewsURL = srv.URL
	defer func() { finnhubNewsURL = oldURL }()

	out := make(chan *GekkoIngestEvent, 8)
	ctx := context.Background()

	f.pollCategory(ctx, "general", out)
	if got := len(out); got != 2 {
		t.Fatalf("first poll: expected 2 events, got %d", got)
	}
	ev := <-out
	if ev.SourceID != SourceFinnhub {
		t.Errorf("SourceID = %q, want FINNHUB", ev.SourceID)
	}
	if !strings.HasPrefix(ev.SourceURL, "https://finnhub.io") {
		t.Errorf("SourceURL should be attributed to finnhub.io, got %q", ev.SourceURL)
	}
	var art FinnhubArticle
	if err := json.Unmarshal([]byte(ev.RawContent), &art); err != nil {
		t.Fatalf("payload not valid FinnhubArticle: %v", err)
	}
	if art.Headline == "" || art.URL == "" {
		t.Errorf("payload missing headline/url: %+v", art)
	}
	if ev.PayloadHash != ev.ComputeHash() {
		t.Error("payload hash mismatch (would be quarantined)")
	}

	// Second poll of the same items must emit nothing (dedupe by id).
	<-out // drain the second event from poll 1
	f.pollCategory(ctx, "general", out)
	if got := len(out); got != 0 {
		t.Errorf("second poll: expected 0 new events (deduped), got %d", got)
	}
}

// TestFinnhubSource_IdleWithoutKey verifies live start with no key doesn't panic and
// exits cleanly on context cancel.
func TestFinnhubSource_IdleWithoutKey(t *testing.T) {
	f := NewFinnhubSource(FinnhubConfig{APIKey: ""})
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

func TestRedactKey(t *testing.T) {
	err := fmt.Errorf(`Get "https://finnhub.io/api/v1/news?category=general&token=SECRET123": EOF`)
	if got := redactKey(err, "SECRET123"); strings.Contains(got, "SECRET123") || !strings.Contains(got, "token=REDACTED") {
		t.Errorf("key not masked: %s", got)
	}
	if got := redactKey(err, ""); got != err.Error() {
		t.Errorf("empty key must be a no-op, got %s", got)
	}
}
