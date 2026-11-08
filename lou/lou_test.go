package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkTrade builds a losing/winning trade with the fields the analyzers read.
func mkTrade(id string, entry, pnl float64, status, run string) ClosedTrade {
	return ClosedTrade{
		ContractID: id, Exchange: "kalshi", Side: "EXIT",
		EntryPrice: entry, RealizedPnL: pnl, Status: status, Run: run,
		OpenedAtMS: 1_000_000, ClosedAtMS: 1_030_000, // 30s hold
	}
}

func TestAnalyzeEntryBands_FlagsLosingBand(t *testing.T) {
	var trades []ClosedTrade
	// 30 sub-$0.10 longshots, 2 wins — the 24h-forensics pattern.
	for i := 0; i < 30; i++ {
		pnl := -10.0
		if i < 2 {
			pnl = 5
		}
		trades = append(trades, mkTrade("KXFOO-26DEC31-B1", 0.05, pnl, "DRY_CLOSED", "run1"))
	}
	lessons := analyzeEntryBands(trades, 25)
	if len(lessons) != 1 || lessons[0].ID != "entry-band/0.00-0.10" {
		t.Fatalf("expected one losing-band lesson, got %+v", lessons)
	}
	if !strings.Contains(lessons[0].SuggestedAction, "MIN_ENTRY_PRICE") {
		t.Errorf("cheap-band lesson should point at MIN_ENTRY_PRICE, got %q", lessons[0].SuggestedAction)
	}
	// Under the sample floor: no claim.
	if got := analyzeEntryBands(trades[:20], 25); len(got) != 0 {
		t.Errorf("under min-sample must stay silent, got %+v", got)
	}
}

func TestAnalyzeShortExpiry(t *testing.T) {
	// A 15-minute crypto series; expiry parses to a real instant.
	expMS, ok := contractExpiryUnixMS("KXETH15M-26JUL090845-45")
	if !ok {
		t.Fatal("expiry should parse for KXETH15M ticker")
	}
	var trades []ClosedTrade
	// 5 entries made 5 minutes before resolution — all short-life, mostly losers.
	for i := 0; i < 5; i++ {
		pnl := -20.0
		if i == 0 {
			pnl = 4 // one winner, still a losing bucket overall
		}
		tr := mkTrade("KXETH15M-26JUL090845-45", 0.95, pnl, "DRY_CLOSED", "run1")
		tr.OpenedAtMS = expMS - 5*60*1000 // 5 min of life left
		tr.ClosedAtMS = expMS
		trades = append(trades, tr)
	}
	lessons := analyzeShortExpiry(trades)
	if len(lessons) != 1 || lessons[0].ID != "expiry/short" {
		t.Fatalf("expected one short-expiry lesson, got %+v", lessons)
	}
	if !strings.Contains(lessons[0].SuggestedAction, "NEAR_EXPIRY_SKIP") {
		t.Errorf("short-expiry lesson should point at NEAR_EXPIRY_SKIP, got %q", lessons[0].SuggestedAction)
	}

	// Long-dated entries (months of life) are never short-expiry, even if losing.
	var longDated []ClosedTrade
	for i := 0; i < 10; i++ {
		tr := mkTrade("KXFOO-26DEC31-B1", 0.20, -20, "DRY_CLOSED", "run1")
		tr.OpenedAtMS = 1_000_000 // 1970 open vs 2026 expiry ⇒ huge life
		longDated = append(longDated, tr)
	}
	if got := analyzeShortExpiry(longDated); len(got) != 0 {
		t.Errorf("long-dated entries must not flag short-expiry, got %+v", got)
	}

	// Under the (low) sample floor: stay silent.
	if got := analyzeShortExpiry(trades[:3]); len(got) != 0 {
		t.Errorf("under sample floor must stay silent, got %+v", got)
	}
}

func TestAnalyzeFastStops(t *testing.T) {
	var trades []ClosedTrade
	for i := 0; i < 12; i++ {
		trades = append(trades, mkTrade("KXBAR-26DEC31-B2", 0.15, -8, "STOP_LOSS", "run1")) // 30s holds
	}
	lessons := analyzeFastStops(trades)
	if len(lessons) != 1 || lessons[0].ID != "stops/noise-stopouts" {
		t.Fatalf("expected the noise-stopout lesson, got %+v", lessons)
	}
	// Slow stops (2h holds) are legitimate: no lesson.
	for i := range trades {
		trades[i].ClosedAtMS = trades[i].OpenedAtMS + 2*3600*1000
	}
	if got := analyzeFastStops(trades); len(got) != 0 {
		t.Errorf("slow stops are not noise, got %+v", got)
	}
}

func TestAnalyzeFamilyBurn_GroupsStrikes(t *testing.T) {
	var trades []ClosedTrade
	// Losses spread across two strikes of one family (the ladder-evasion pattern).
	for i := 0; i < 5; i++ {
		trades = append(trades, mkTrade("KXINXY-26DEC31-B5900", 0.4, -30, "DRY_CLOSED", "run1"))
		trades = append(trades, mkTrade("KXINXY-26DEC31-B6000", 0.4, -30, "DRY_CLOSED", "run2"))
	}
	lessons := analyzeFamilyBurn(trades)
	if len(lessons) != 1 || lessons[0].ID != "family/KXINXY-26DEC31" {
		t.Fatalf("expected one family lesson aggregating both strikes, got %+v", lessons)
	}
	if lessons[0].Evidence.Trades != 10 || len(lessons[0].Evidence.Runs) != 2 {
		t.Errorf("family evidence should span 10 trades / 2 runs, got %+v", lessons[0].Evidence)
	}
}

func TestAnalyzePersonas_ReturnOnCapital(t *testing.T) {
	var trades []ClosedTrade
	for i := 0; i < 30; i++ {
		tr := mkTrade("KXBAZ-26DEC31-B3", 0.3, -5, "DRY_CLOSED", "run1")
		tr.PersonaScores = []PersonaScore{
			{Persona: "bear", RealizedPnL: -10, Cost: 100}, // -10% ROC
			{Persona: "bull", RealizedPnL: 10, Cost: 100},  // +10% ROC
			{Persona: "flat", RealizedPnL: 0.5, Cost: 100}, // +0.5%: no claim either way
		}
		trades = append(trades, tr)
	}
	lessons := analyzePersonas(trades, 25)
	if len(lessons) != 2 {
		t.Fatalf("expected bear (loser) + bull (winner) lessons, got %+v", lessons)
	}
	ids := lessons[0].ID + " " + lessons[1].ID
	if !strings.Contains(ids, "persona/bear") || !strings.Contains(ids, "persona/bull") {
		t.Errorf("expected persona/bear and persona/bull, got %s", ids)
	}
}

func TestAnalyzeVenues_OnlyFlagsDivergence(t *testing.T) {
	var trades []ClosedTrade
	for i := 0; i < 30; i++ {
		k := mkTrade("KXW-26DEC31-B1", 0.3, 10, "DRY_CLOSED", "run1") // kalshi wins
		trades = append(trades, k)
		p := mkTrade("POLY-123", 0.3, -10, "DRY_CLOSED", "run1") // polymarket loses
		p.Exchange = "polymarket"
		trades = append(trades, p)
	}
	lessons := analyzeVenues(trades, 25)
	if len(lessons) != 1 || lessons[0].ID != "venue/polymarket" {
		t.Fatalf("expected the diverging venue flagged, got %+v", lessons)
	}
	// Both venues bleeding: strategy problem, not a venue lesson.
	for i := range trades {
		trades[i].RealizedPnL = -10
	}
	if got := analyzeVenues(trades, 25); len(got) != 0 {
		t.Errorf("uniform bleeding must not produce a venue lesson, got %+v", got)
	}
}

func TestStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// Pass 1: a new pattern arrives from one run → proposed.
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cand := func(runs ...string) []*Lesson {
		return []*Lesson{{ID: "entry-band/0.00-0.10", Pattern: "cheap entries lose",
			Evidence: Evidence{Trades: 30, Runs: runs}}}
	}
	ch := s.Merge(cand("run1"), now)
	if len(ch.New) != 1 || ch.New[0].Status != "proposed" {
		t.Fatalf("first sighting should be proposed, got %+v", ch)
	}
	if err := s.Save(ch, now); err != nil {
		t.Fatal(err)
	}

	// Pass 2 (reloaded store): same pattern now backed by two runs → validated.
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ch2 := s2.Merge(cand("run1", "run2"), now.Add(time.Hour))
	if len(ch2.Validated) != 1 || s2.Lessons["entry-band/0.00-0.10"].Status != "validated" {
		t.Fatalf("two-run evidence should validate, got %+v", ch2)
	}

	// Pass 3: pattern vanishes from the candidates → retired.
	ch3 := s2.Merge(nil, now.Add(2*time.Hour))
	if len(ch3.Retired) != 1 || s2.Lessons["entry-band/0.00-0.10"].Status != "retired" {
		t.Fatalf("unsupported lesson should retire, got %+v", ch3)
	}

	// Pass 4: it re-emerges → back to proposed, must re-earn validation.
	ch4 := s2.Merge(cand("run3"), now.Add(3*time.Hour))
	if len(ch4.New) != 1 || s2.Lessons["entry-band/0.00-0.10"].Status != "proposed" {
		t.Fatalf("re-emerged lesson should be proposed again, got %+v", ch4)
	}
}

func TestPass_EndToEnd(t *testing.T) {
	root := t.TempDir()
	arch := filepath.Join(root, "archive-20260101-000000")
	if err := os.MkdirAll(arch, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 0; i < 30; i++ {
		raw, _ := json.Marshal(mkTrade("KXQUX-26DEC31-B4", 0.05, -10, "STOP_LOSS", ""))
		lines = append(lines, string(raw))
	}
	if err := os.WriteFile(filepath.Join(arch, "trades.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\ngarbage-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	digest, err := pass(root, 25, AutonomyObserve, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Lou digest", "New lessons", "0.00-0.10", "noise", "analysis only, no config was changed"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest missing %q:\n%s", want, digest)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "lou", "lessons.json")); err != nil {
		t.Errorf("lessons.json not persisted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "lou", "lessons_history.jsonl")); err != nil {
		t.Errorf("lessons_history.jsonl not persisted: %v", err)
	}
}
