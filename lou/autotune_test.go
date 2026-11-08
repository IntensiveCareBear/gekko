package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validatedLesson(id string, pnl float64) *Lesson {
	return &Lesson{ID: id, Pattern: "test pattern " + id, Status: "validated",
		Evidence: Evidence{Trades: 100, PnL: pnl, Runs: []string{"r1", "r2"}}}
}

func TestTuneTargets_OnlyValidatedAndConservativeWins(t *testing.T) {
	lessons := map[string]*Lesson{
		// Two lessons targeting MAX_ENTRY_PRICE: 0.50 (tighter) must win.
		"a": validatedLesson("entry-band/0.65-1.00", -3000),
		"b": validatedLesson("entry-band/0.50-0.65", -50000),
		// Proposed only: must not tune.
		"c": {ID: "stops/noise-stopouts", Status: "proposed", Evidence: Evidence{Trades: 30}},
		// Winning band: not a tightening driver.
		"d": func() *Lesson { l := validatedLesson("entry-band/0.00-0.10", 3000); return l }(),
	}
	targets := tuneTargets(lessons)
	if len(targets) != 1 || targets[0].knob != "MAX_ENTRY_PRICE" || targets[0].target != 0.50 {
		t.Fatalf("expected single MAX_ENTRY_PRICE target 0.50, got %+v", targets)
	}
}

func TestAutotune_StepBoundsAndDailyCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 7, 22, 0, 0, 0, time.UTC)
	lessons := map[string]*Lesson{
		"a": validatedLesson("entry-band/0.50-0.65", -50000), // wants MAX_ENTRY_PRICE 0.50
		"b": validatedLesson("stops/noise-stopouts", -6000),  // wants STOP_LOSS_MIN_ABS up
	}

	// Pass 1: each knob moves at most one MaxStep from default.
	changes, err := autotune(dir, lessons, now, AutonomyTune)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %+v", changes)
	}
	vals, _, err := readOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vals["MAX_ENTRY_PRICE"] != 0.65 { // 0.70 − 0.05 step, not the 0.50 target
		t.Errorf("MAX_ENTRY_PRICE should step to 0.65, got %v", vals["MAX_ENTRY_PRICE"])
	}
	if vals["STOP_LOSS_MIN_ABS"] != 0.03 { // 0.02 + 0.01 step
		t.Errorf("STOP_LOSS_MIN_ABS should step to 0.03, got %v", vals["STOP_LOSS_MIN_ABS"])
	}

	// Same day again: daily cap blocks further movement.
	changes, err = autotune(dir, lessons, now.Add(time.Hour), AutonomyTune)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("daily cap should block a second step, got %+v", changes)
	}

	// Next day, same lessons: the one-step-per-lesson cap blocks a repeat —
	// re-validating on the same evidence is not new information.
	changes, err = autotune(dir, lessons, now.Add(24*time.Hour), AutonomyTune)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("a lesson that already tuned its knob must not step again, got %+v", changes)
	}
	vals, _, _ = readOverrides(dir)
	if vals["MAX_ENTRY_PRICE"] != 0.65 {
		t.Errorf("day 2 MAX_ENTRY_PRICE should hold at 0.65, got %v", vals["MAX_ENTRY_PRICE"])
	}
}

func TestAutotune_RatchetNeverRelaxes(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 7, 22, 0, 0, 0, time.UTC)
	// Overrides already tighter than the lesson's target: no move.
	if err := os.WriteFile(filepath.Join(dir, "overrides.env"),
		[]byte("MAX_ENTRY_PRICE=0.45\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lessons := map[string]*Lesson{"a": validatedLesson("entry-band/0.50-0.65", -50000)} // target 0.50 > current 0.45
	changes, err := autotune(dir, lessons, now, AutonomyTune)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("ratchet must not relax 0.45 back toward 0.50, got %+v", changes)
	}
}

func TestAutotune_HardBounds(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// Current value one step below the bound: the lesson's target (Max) must
	// clamp exactly at the bound, never past it.
	if err := os.WriteFile(filepath.Join(dir, "overrides.env"),
		[]byte("STOP_LOSS_MIN_ABS=0.055\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lessons := map[string]*Lesson{"b": validatedLesson("stops/noise-stopouts", -6000)}
	if _, err := autotune(dir, lessons, now, AutonomyTune); err != nil {
		t.Fatal(err)
	}
	vals, _, _ := readOverrides(dir)
	if vals["STOP_LOSS_MIN_ABS"] != 0.06 {
		t.Errorf("STOP_LOSS_MIN_ABS must clamp at 0.06, got %v", vals["STOP_LOSS_MIN_ABS"])
	}
	// Re-running never pushes past the bound (the lesson cap and the at-bound
	// ratchet both stop it).
	if _, err := autotune(dir, lessons, now.Add(24*time.Hour), AutonomyTune); err != nil {
		t.Fatal(err)
	}
	vals, _, _ = readOverrides(dir)
	if vals["STOP_LOSS_MIN_ABS"] != 0.06 {
		t.Errorf("STOP_LOSS_MIN_ABS must stay at 0.06, got %v", vals["STOP_LOSS_MIN_ABS"])
	}
}

func TestRollbackRemovesOverrides(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	lessons := map[string]*Lesson{"a": validatedLesson("entry-band/0.50-0.65", -50000)}
	if _, err := autotune(dir, lessons, now, AutonomyTune); err != nil {
		t.Fatal(err)
	}
	if err := rollback(dir, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "overrides.env")); !os.IsNotExist(err) {
		t.Error("overrides.env should be gone after rollback")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tuning_history.jsonl"))
	if err != nil || !strings.Contains(string(raw), "rollback") {
		t.Errorf("tuning history should record the rollback: %v %s", err, raw)
	}
}

func TestOverrides_PreserveUnknownLines(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "overrides.env"),
		[]byte("SOME_HAND_ADDED=1\nMAX_ENTRY_PRICE=0.65\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vals, extra, err := readOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vals["MAX_ENTRY_PRICE"] != 0.65 || len(extra) != 1 || extra[0] != "SOME_HAND_ADDED=1" {
		t.Fatalf("parse mismatch: vals=%v extra=%v", vals, extra)
	}
	if err := writeOverrides(dir, vals, extra, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "overrides.env"))
	if !strings.Contains(string(raw), "SOME_HAND_ADDED=1") {
		t.Errorf("hand-added line lost on rewrite:\n%s", raw)
	}
}
