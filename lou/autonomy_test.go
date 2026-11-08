package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAutonomy(t *testing.T) {
	cases := []struct {
		in    string
		want  Autonomy
		known bool
	}{
		{"observe", AutonomyObserve, true},
		{"tune", AutonomyTune, true},
		{"intervene", AutonomyIntervene, true},
		{" INTERVENE ", AutonomyIntervene, true},
		{"", AutonomyObserve, false},
		{"nonsense", AutonomyObserve, false},
	}
	for _, c := range cases {
		got, known := parseAutonomy(c.in)
		if got != c.want || known != c.known {
			t.Errorf("parseAutonomy(%q) = (%v, %v), want (%v, %v)", c.in, got, known, c.want, c.known)
		}
	}
}

// Precedence matters: an operator at a shell must be able to override the dashboard,
// and an existing nightly.sh passing -autotune must keep behaving as it always has
// until someone actually sets a level.
func TestResolveAutonomy_Precedence(t *testing.T) {
	dir := t.TempDir()

	if got := resolveAutonomy(dir, false); got != AutonomyObserve {
		t.Errorf("nothing configured, no flag = %v, want observe", got)
	}
	if got := resolveAutonomy(dir, true); got != AutonomyTune {
		t.Errorf("legacy -autotune flag = %v, want tune (unchanged behaviour)", got)
	}

	// The dashboard's file outranks the legacy flag.
	if err := os.WriteFile(filepath.Join(dir, autonomyFile), []byte("intervene\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveAutonomy(dir, false); got != AutonomyIntervene {
		t.Errorf("file = %v, want intervene", got)
	}
	if got := resolveAutonomy(dir, true); got != AutonomyIntervene {
		t.Errorf("file must outrank the flag, got %v", got)
	}

	// ...and the environment outranks the file.
	t.Setenv("LOU_AUTONOMY", "observe")
	if got := resolveAutonomy(dir, true); got != AutonomyObserve {
		t.Errorf("env must outrank file and flag, got %v", got)
	}
}

// Observe is the whole point of the bottom rung: Lou still analyses, but overrides.env
// is never written.
func TestAutotune_ObserveChangesNothing(t *testing.T) {
	dir := t.TempDir()
	lessons := map[string]*Lesson{"a": validatedLesson("entry-band/0.50-0.65", -50000)}

	changes, err := autotune(dir, lessons, time.Now(), AutonomyObserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("observe must change nothing, got %+v", changes)
	}
	if _, err := os.Stat(filepath.Join(dir, "overrides.env")); !os.IsNotExist(err) {
		t.Error("observe must not write overrides.env at all")
	}
}

// Tune may move numbers but must not reach for the levers that stop something trading.
func TestAutotune_TuneDoesNotBenchOrBlacklist(t *testing.T) {
	dir := t.TempDir()
	lessons := map[string]*Lesson{
		"a": validatedLesson("entry-band/0.50-0.65", -50000),
		"b": validatedLesson("persona/gemma-bull", -8000),
		"c": validatedLesson("family/KXGDP", -2000),
	}
	changes, err := autotune(dir, lessons, time.Now(), AutonomyTune)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Action != "tune" {
			t.Errorf("tune level produced a %q action on %q — only numeric moves are allowed here",
				c.Action, c.Target)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "overrides.env"))
	if strings.Contains(string(raw), "SHADOW_PERSONAS") || strings.Contains(string(raw), "FAMILY_BLACKLIST") {
		t.Errorf("tune level wrote an intervention knob:\n%s", raw)
	}
}

func TestAutotune_IntervenesOnValidatedLosers(t *testing.T) {
	dir := t.TempDir()
	lessons := map[string]*Lesson{
		"a": validatedLesson("persona/gemma-bull", -8000),
		"b": validatedLesson("family/KXGDP", -2000),
		// A persona that MAKES money is not a reason to bench it.
		"c": validatedLesson("persona/weather", 4000),
		// Unvalidated: not yet actionable.
		"d": {ID: "persona/bull", Status: "proposed", Evidence: Evidence{Trades: 30, PnL: -9000}},
	}
	changes, err := autotune(dir, lessons, time.Now(), AutonomyIntervene)
	if err != nil {
		t.Fatal(err)
	}

	acted := map[string]string{}
	for _, c := range changes {
		if c.Target != "" {
			acted[c.Target] = c.Action
		}
	}
	if acted["gemma-bull"] != "bench" {
		t.Errorf("a validated losing persona should be benched, got %+v", acted)
	}
	if acted["kxgdp"] != "blacklist" {
		t.Errorf("a validated losing family should be blacklisted, got %+v", acted)
	}
	if _, benched := acted["weather"]; benched {
		t.Error("a profitable persona must never be benched")
	}
	if _, benched := acted["bull"]; benched {
		t.Error("an unvalidated lesson must not bench anyone")
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "overrides.env"))
	if !strings.Contains(string(raw), "SHADOW_PERSONAS=gemma-bull") {
		t.Errorf("overrides.env missing the bench:\n%s", raw)
	}
	if !strings.Contains(string(raw), "FAMILY_BLACKLIST=kxgdp") {
		t.Errorf("overrides.env missing the blacklist:\n%s", raw)
	}
}

// Lou unions into these sets and never removes from them: a human's hand-added entry
// has to survive, and undoing an intervention stays a human decision.
func TestAutotune_InterventionsAreAdditiveAndPreserveHandEdits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "overrides.env"),
		[]byte("SHADOW_PERSONAS=hand-added\nSOME_OTHER=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lessons := map[string]*Lesson{"a": validatedLesson("persona/gemma-bull", -8000)}
	if _, err := autotune(dir, lessons, time.Now(), AutonomyIntervene); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "overrides.env"))
	got := string(raw)
	if !strings.Contains(got, "SHADOW_PERSONAS=gemma-bull,hand-added") {
		t.Errorf("expected the union of hand-added and benched, got:\n%s", got)
	}
	if !strings.Contains(got, "SOME_OTHER=1") {
		t.Errorf("unrelated hand-added line lost:\n%s", got)
	}

	// A second pass must not duplicate the entry.
	if _, err := autotune(dir, lessons, time.Now().Add(48*time.Hour), AutonomyIntervene); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "overrides.env"))
	if strings.Count(string(raw), "gemma-bull") != 1 {
		t.Errorf("benching twice duplicated the entry:\n%s", raw)
	}
}
