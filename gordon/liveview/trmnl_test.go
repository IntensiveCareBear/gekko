package main

import (
	"testing"
	"time"
)

// Anchor "now" to a known Wednesday 12:00 local so the today/week boundaries are
// deterministic: Monday 00:00 is 2.5 days back, today's midnight is 12h back.
func trmnlNow() time.Time {
	return time.Date(2026, time.July, 15, 12, 0, 0, 0, time.Local) // Wed
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func TestTRMNLMetricsBucketing(t *testing.T) {
	now := trmnlNow()
	trades := []trmnlTrade{
		{RealizedPnL: 100, ClosedAtMs: ms(now.Add(-2 * time.Hour))},  // today + week
		{RealizedPnL: -40, ClosedAtMs: ms(now.Add(-3 * time.Hour))},  // today + week
		{RealizedPnL: 30, ClosedAtMs: ms(now.Add(-48 * time.Hour))},  // week only (Mon)
		{RealizedPnL: 999, ClosedAtMs: ms(now.Add(-10 * 24 * time.Hour))}, // last week, excluded
		{RealizedPnL: 5, ClosedAtMs: 0},                              // no timestamp, ignored
	}

	today, week := trmnlMetrics(trades, now)

	if today.Trades != 2 {
		t.Errorf("today trades = %d, want 2", today.Trades)
	}
	if today.PnL != "+60.00" {
		t.Errorf("today pnl = %q, want +60.00", today.PnL)
	}
	// gross win 100 / gross loss 40 = 2.50
	if today.ProfitFactor != "2.50" {
		t.Errorf("today profit factor = %q, want 2.50", today.ProfitFactor)
	}
	// 1 win of 2 = 50%
	if today.WinRate != "50" {
		t.Errorf("today win rate = %q, want 50", today.WinRate)
	}

	if week.Trades != 3 {
		t.Errorf("week trades = %d, want 3", week.Trades)
	}
	if week.PnL != "+90.00" {
		t.Errorf("week pnl = %q, want +90.00", week.PnL)
	}
	if !week.PnLPositive {
		t.Errorf("week should be positive")
	}
}

func TestTRMNLWindowNoLosses(t *testing.T) {
	w := trmnlWindowFor([]trmnlTrade{{RealizedPnL: 10}, {RealizedPnL: 20}})
	if w.ProfitFactor != "∞" {
		t.Errorf("profit factor = %q, want ∞", w.ProfitFactor)
	}
	if w.WinRate != "100" {
		t.Errorf("win rate = %q, want 100", w.WinRate)
	}
}

func TestTRMNLWindowEmpty(t *testing.T) {
	w := trmnlWindowFor(nil)
	if w.Trades != 0 || w.ProfitFactor != "—" || w.WinRate != "—" || w.PnL != "0.00" {
		t.Errorf("empty window = %+v, want zeroed placeholders", w)
	}
}
