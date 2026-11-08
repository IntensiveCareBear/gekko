package main

import "testing"

func TestComputePersonaStats(t *testing.T) {
	trades := []ClosedTrade{
		{Persona: "bull", RealizedPnL: 100, ReturnPct: 10},
		{Persona: "bull", RealizedPnL: -40, ReturnPct: -4},
		{Persona: "bear", RealizedPnL: 50, ReturnPct: 5},
		{Persona: "", RealizedPnL: 7, ReturnPct: 1}, // unattributed bucket
	}
	stats := computePersonaStats(trades, 100000, 20)

	byName := map[string]PersonaStat{}
	for _, s := range stats {
		byName[s.Persona] = s
	}

	bull := byName["bull"]
	if bull.Trades != 2 || bull.Wins != 1 || bull.Losses != 1 {
		t.Errorf("bull counts wrong: %+v", bull)
	}
	if bull.RealizedPnL != 60 {
		t.Errorf("bull pnl = %v, want 60", bull.RealizedPnL)
	}
	if bull.WinRate != 50 {
		t.Errorf("bull win rate = %v, want 50", bull.WinRate)
	}
	if _, ok := byName["unattributed"]; !ok {
		t.Error("empty persona should bucket under 'unattributed'")
	}

	// A consensus trade credits each contributor the full outcome.
	consensus := []ClosedTrade{
		{Persona: "consensus", Contributors: []string{"bull", "bear"}, RealizedPnL: 100, ReturnPct: 10},
	}
	cs := map[string]PersonaStat{}
	for _, s := range computePersonaStats(consensus, 100000, 20) {
		cs[s.Persona] = s
	}
	if cs["bull"].Trades != 1 || cs["bear"].Trades != 1 {
		t.Errorf("both contributors should be credited 1 trade: %+v", cs)
	}
	if cs["bull"].RealizedPnL != 100 || cs["bear"].RealizedPnL != 100 {
		t.Errorf("each contributor should get full $100: %+v", cs)
	}
	if _, ok := cs["consensus"]; ok {
		t.Error("consensus identity should not appear as its own scorecard row when contributors exist")
	}

	// Sorted by realized PnL desc → bull (60) first, then bear (50).
	if stats[0].Persona != "bull" {
		t.Errorf("expected bull first (highest pnl), got %q", stats[0].Persona)
	}
}
