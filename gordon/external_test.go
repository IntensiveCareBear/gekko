package main

import (
	"testing"
	"time"
)

func TestEconCalendar_NilSafe(t *testing.T) {
	var c *EconCalendar // disabled
	if blocked, _ := c.Blackout("anything KXCPI", time.Now()); blocked {
		t.Error("nil calendar must never block")
	}
}

func TestEconCalendar_Blackout(t *testing.T) {
	now := time.Now()
	c := &EconCalendar{window: 30 * time.Minute}
	c.events = []EconEvent{
		{Time: now.Add(15 * time.Minute), Title: "CPI release", Keywords: []string{"cpi", "inflation"}},
		{Time: now.Add(10 * time.Hour), Title: "FOMC", Keywords: []string{"fomc", "rate"}},
	}

	// Within the 30m pre-print window AND keyword matches → blocked.
	if blocked, title := c.Blackout("Traders eye KXCPI ahead of print", now); !blocked || title != "CPI release" {
		t.Errorf("expected CPI blackout, got blocked=%v title=%q", blocked, title)
	}
	// Keyword matches a far-off event (10h away) → not in window → allowed.
	if blocked, _ := c.Blackout("FOMC rate decision next week", now); blocked {
		t.Error("event outside window should not block")
	}
	// In window but no keyword match → allowed.
	if blocked, _ := c.Blackout("Bitcoin rallies on ETF news", now); blocked {
		t.Error("non-matching headline should not block")
	}
	// After the release time → allowed.
	if blocked, _ := c.Blackout("cpi", now.Add(20*time.Minute)); blocked {
		t.Error("after release should not block")
	}
}

func TestScoreSentiment_FallsBackToLexicon(t *testing.T) {
	sentimentClient = nil // ensure API disabled
	if s := scoreSentiment(nil, "stocks surge to record high"); s <= 0 {
		t.Errorf("expected positive lexicon fallback, got %.2f", s)
	}
}

func TestNewSentimentClient_DisabledWhenUnset(t *testing.T) {
	if NewSentimentClient("", time.Second) != nil {
		t.Error("empty URL should yield a disabled (nil) client")
	}
}

func TestNewEconCalendar_DisabledWhenUnset(t *testing.T) {
	if NewEconCalendar("", "", time.Minute) != nil {
		t.Error("no source should yield a disabled (nil) calendar")
	}
}
