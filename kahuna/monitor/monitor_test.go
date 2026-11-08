package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gekko/kahuna/ingest"
)

func TestMonitor_RecordEvent(t *testing.T) {
	m := NewMonitor()

	event1 := &ingest.GekkoIngestEvent{
		SourceID:    ingest.SourceKalshi,
		RawContent:  "test payload 1",
		SourceURL:   "https://api.kalshi.com/v2/market_lifecycle",
		PayloadHash: "hash123",
	}

	event2 := &ingest.GekkoIngestEvent{
		SourceID:    ingest.SourceReuters,
		RawContent:  "test payload 2",
		SourceURL:   "https://www.reuters.com/business",
		PayloadHash: "hash456",
	}

	m.RecordEvent(event1, "PROCESSED", "")
	m.RecordEvent(event2, "QUARANTINED", "Invalid payload domain")

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.totalReceived != 2 {
		t.Errorf("Expected total received to be 2, got %d", m.totalReceived)
	}
	if m.totalProcessed != 1 {
		t.Errorf("Expected total processed to be 1, got %d", m.totalProcessed)
	}
	if m.totalQuarantined != 1 {
		t.Errorf("Expected total quarantined to be 1, got %d", m.totalQuarantined)
	}

	km, ok := m.sourceMetrics[ingest.SourceKalshi]
	if !ok || km.ReceivedCount != 1 || km.ProcessedCount != 1 || km.QuarantinedCount != 0 {
		t.Errorf("Unexpected Kalshi metrics: %+v", km)
	}

	rm, ok := m.sourceMetrics[ingest.SourceReuters]
	if !ok || rm.ReceivedCount != 1 || rm.ProcessedCount != 0 || rm.QuarantinedCount != 1 {
		t.Errorf("Unexpected Reuters metrics: %+v", rm)
	}
}

func TestMonitor_RecentEventsCap(t *testing.T) {
	m := NewMonitor()
	m.maxRecentEvents = 5 // set a low cap for testing

	event := &ingest.GekkoIngestEvent{
		SourceID:    ingest.SourceTwitter,
		RawContent:  "tweet",
		SourceURL:   "https://twitter.com/X",
		PayloadHash: "hash",
	}

	// Record 10 events
	for i := 0; i < 10; i++ {
		m.RecordEvent(event, "PROCESSED", "")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.recentEvents) != 5 {
		t.Errorf("Expected recent events slice length to be capped at 5, got %d", len(m.recentEvents))
	}
}

func TestMonitor_APIStats(t *testing.T) {
	m := NewMonitor()
	event := &ingest.GekkoIngestEvent{
		SourceID:    ingest.SourceKalshi,
		RawContent:  "kalshi data",
		SourceURL:   "https://api.kalshi.com",
		PayloadHash: "hash123",
	}
	m.RecordEvent(event, "PROCESSED", "")

	req := httptest.NewRequest("GET", "/api/stats", nil)
	rr := httptest.NewRecorder()

	m.handleStats(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, rr.Code)
	}

	var resp StatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal API stats response: %v", err)
	}

	if resp.TotalReceived != 1 || resp.TotalProcessed != 1 || resp.TotalQuarantined != 0 {
		t.Errorf("Unexpected API stats totals inside JSON response: %+v", resp)
	}

	if len(resp.RecentEvents) != 1 {
		t.Errorf("Expected 1 recent event in JSON response, got %d", len(resp.RecentEvents))
	}
}

func TestMonitor_RoutingGUIandCLI(t *testing.T) {
	m := NewMonitor()

	// 1. Browser request (GUI)
	reqBrowser := httptest.NewRequest("GET", "/", nil)
	reqBrowser.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")
	rrBrowser := httptest.NewRecorder()

	m.handleIndex(rrBrowser, reqBrowser)

	if rrBrowser.Code != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, rrBrowser.Code)
	}
	if !strings.Contains(rrBrowser.Body.String(), "<!DOCTYPE html>") {
		t.Errorf("Expected HTML dashboard for browser request, got: %s", rrBrowser.Body.String()[:100])
	}

	// 2. curl request (CLI)
	reqCurl := httptest.NewRequest("GET", "/", nil)
	reqCurl.Header.Set("User-Agent", "curl/8.4.0")
	rrCurl := httptest.NewRecorder()

	m.handleIndex(rrCurl, reqCurl)

	if rrCurl.Code != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, rrCurl.Code)
	}
	if !strings.Contains(rrCurl.Body.String(), "KAHUNA INGESTION WORKER MONITOR (CLI)") {
		t.Errorf("Expected CLI ASCII dashboard for curl request, got: %s", rrCurl.Body.String())
	}
}

func TestMonitor_Shutdown(t *testing.T) {
	m := NewMonitor()
	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		errChan <- m.StartServer(ctx, "localhost:18081")
	}()

	// wait for server initialization
	time.Sleep(50 * time.Millisecond)

	// cancel context to initiate shutdown
	cancel()

	select {
	case err := <-errChan:
		if err != nil {
			t.Errorf("StartServer returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for server shutdown")
	}
}
