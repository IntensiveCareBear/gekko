package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Phase 3 (docs/lou-learning-loop-proposal.md): auto-tune, paper-first. A
// VALIDATED lesson may nudge a pre-approved, bounded config knob. The change is
// written to <lou>/overrides.env, which run.sh sources at the next launch — Lou
// never touches a running process, code, or anything off the whitelist.
//
// Guardrails, in order of enforcement:
//   - whitelist: no Knob entry, no change;
//   - one-way ratchet: v1 only TIGHTENS (each knob has a tighten direction) —
//     relaxing a guard back is a human decision;
//   - hard bounds + max step per pass, clamped;
//   - daily cap: at most one change per knob per calendar day (tuning history
//     is the record);
//   - one step per lesson, ever: a lesson that already moved a knob never moves
//     it again — a persistent lesson re-validating on largely the same evidence
//     is not new information, and repeat applications ratchet the knob to its
//     bound (MAX_ENTRY_PRICE walked 0.70→0.55 in three passes off one lesson,
//     starving entries);
//   - rollback: delete overrides.env (or run `lou -rollback`) and the next
//     launch is back on defaults.

// Knob is one whitelisted parameter Lou may adjust.
type Knob struct {
	Name     string  // env var name, sourced by run.sh at next launch
	Default  float64 // effective value when no override exists
	Min, Max float64 // hard bounds — Lou can never leave this range
	MaxStep  float64 // biggest move in one pass
	// TightenUp: true = safer is higher (floors), false = safer is lower
	// (ceilings). The ratchet only permits moves in the safer direction.
	TightenUp bool
	Decimals  int // formatting precision for overrides.env
}

// knobWhitelist is the v1 set: the knobs the improvement log already proved
// matter, nothing else.
var knobWhitelist = []Knob{
	// MAX_ENTRY_PRICE Min raised 0.40→0.55 (2026-07-12): below 0.55 the entry
	// window collapses against MIN_ENTRY_PRICE and the mid-band confidence gate,
	// and the fleet stops opening positions entirely.
	{Name: "MAX_ENTRY_PRICE", Default: 0.70, Min: 0.55, Max: 0.80, MaxStep: 0.05, TightenUp: false, Decimals: 2},
	{Name: "MIN_ENTRY_PRICE", Default: 0.10, Min: 0.05, Max: 0.25, MaxStep: 0.05, TightenUp: true, Decimals: 2},
	{Name: "STOP_LOSS_MIN_ABS", Default: 0.02, Min: 0.01, Max: 0.06, MaxStep: 0.01, TightenUp: true, Decimals: 2},
}

func knobByName(name string) *Knob {
	for i := range knobWhitelist {
		if knobWhitelist[i].Name == name {
			return &knobWhitelist[i]
		}
	}
	return nil
}

// tuneTarget is one lesson's opinion about where a knob should sit.
type tuneTarget struct {
	knob   string
	target float64
	lesson *Lesson
}

// tuneTargets maps VALIDATED lessons to knob targets. Conflicts (two lessons on
// one knob) are resolved most-conservative-wins: the tightest target in the
// knob's tighten direction.
func tuneTargets(lessons map[string]*Lesson) []tuneTarget {
	byKnob := map[string]tuneTarget{}
	consider := func(t tuneTarget) {
		k := knobByName(t.knob)
		if k == nil {
			return // not whitelisted — no knob, no change
		}
		prev, ok := byKnob[t.knob]
		if !ok || (k.TightenUp && t.target > prev.target) || (!k.TightenUp && t.target < prev.target) {
			byKnob[t.knob] = t
		}
	}

	for _, l := range lessons {
		if l.Status != "validated" {
			continue
		}
		switch {
		// Losing entry bands drive the price gates toward excluding the band.
		case strings.HasPrefix(l.ID, "entry-band/") && l.Evidence.PnL < 0:
			switch strings.TrimPrefix(l.ID, "entry-band/") {
			case "0.00-0.10":
				consider(tuneTarget{"MIN_ENTRY_PRICE", 0.10, l})
			case "0.10-0.25":
				consider(tuneTarget{"MIN_ENTRY_PRICE", 0.25, l})
			case "0.50-0.65":
				consider(tuneTarget{"MAX_ENTRY_PRICE", 0.50, l})
			case "0.65-1.00":
				consider(tuneTarget{"MAX_ENTRY_PRICE", 0.65, l})
			}
		// Noise stop-outs push the stop floor up one step at a time (the daily
		// cap and Max bound keep a persistent lesson from ratcheting runaway).
		case l.ID == "stops/noise-stopouts":
			consider(tuneTarget{"STOP_LOSS_MIN_ABS", knobByName("STOP_LOSS_MIN_ABS").Max, l})
		}
	}

	out := make([]tuneTarget, 0, len(byKnob))
	for _, t := range byKnob {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].knob < out[j].knob })
	return out
}

// TuneChange is one applied adjustment, recorded in tuning_history.jsonl and
// the digest — the full audit trail the guardrails require.
type TuneChange struct {
	At       time.Time `json:"at"`
	Knob     string    `json:"knob"`
	Old      float64   `json:"old"`
	New      float64   `json:"new"`
	LessonID string    `json:"lesson_id"`
	Pattern  string    `json:"pattern"`
	Action   string    `json:"action"` // "tune", "bench", "blacklist" or "rollback"
	// Target names WHAT was acted on when the action isn't a numeric knob move: the
	// persona benched, or the family blacklisted. Empty for "tune", where Knob/Old/New
	// already say everything.
	Target string `json:"target,omitempty"`
}

// readOverrides parses <dir>/overrides.env into knob values; knobs without an
// override sit at their defaults. Unknown keys are preserved verbatim so a
// hand-added line survives a rewrite.
func readOverrides(dir string) (vals map[string]float64, extra []string, err error) {
	vals = map[string]float64{}
	for _, k := range knobWhitelist {
		vals[k.Name] = k.Default
	}
	f, err := os.Open(filepath.Join(dir, "overrides.env"))
	if os.IsNotExist(err) {
		return vals, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if knobByName(key) == nil {
			extra = append(extra, line)
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
			vals[key] = v
		}
	}
	return vals, extra, sc.Err()
}

// writeOverrides persists the managed knobs that differ from their defaults
// (plus any preserved unknown lines). All defaults ⇒ the file is removed —
// absence of overrides.env IS the rolled-back state.
func writeOverrides(dir string, vals map[string]float64, extra []string, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Lou auto-tune overrides (paper) — generated %s\n", now.Format(time.RFC3339))
	b.WriteString("# Sourced by run.sh at launch. Delete this file (or `lou -rollback`) to restore defaults.\n")
	n := 0
	for _, k := range knobWhitelist {
		if v, ok := vals[k.Name]; ok && v != k.Default {
			fmt.Fprintf(&b, "%s=%.*f\n", k.Name, k.Decimals, v)
			n++
		}
	}
	for _, line := range extra {
		b.WriteString(line + "\n")
	}
	path := filepath.Join(dir, "overrides.env")
	if n == 0 && len(extra) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// knobsTunedToday reads tuning_history.jsonl and reports which knobs already
// changed on now's calendar day (the max-one-step-per-day cap), plus which
// knob|lesson pairs have ever been applied (the one-step-per-lesson cap).
func knobsTunedToday(dir string, now time.Time) (tuned, byLesson map[string]bool, err error) {
	tuned = map[string]bool{}
	byLesson = map[string]bool{}
	f, err := os.Open(filepath.Join(dir, "tuning_history.jsonl"))
	if os.IsNotExist(err) {
		return tuned, byLesson, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	day := now.Format("2006-01-02")
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c TuneChange
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		if c.Action != "tune" {
			continue
		}
		if c.At.Format("2006-01-02") == day {
			tuned[c.Knob] = true
		}
		if c.LessonID != "" {
			byLesson[c.Knob+"|"+c.LessonID] = true
		}
	}
	return tuned, byLesson, sc.Err()
}

func appendTuneHistory(dir string, changes []TuneChange) error {
	if len(changes) == 0 {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "tuning_history.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, c := range changes {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return nil
}

// autotune applies validated lessons to the whitelisted knobs and persists the
// result. Returns the changes actually applied (possibly none).
func autotune(dir string, lessons map[string]*Lesson, now time.Time, level Autonomy) ([]TuneChange, error) {
	if !level.mayTune() {
		return nil, nil // observe: analyze and report, change nothing
	}
	vals, extra, err := readOverrides(dir)
	if err != nil {
		return nil, err
	}
	tunedToday, tunedByLesson, err := knobsTunedToday(dir, now)
	if err != nil {
		return nil, err
	}

	var changes []TuneChange
	for _, t := range tuneTargets(lessons) {
		k := knobByName(t.knob)
		cur := vals[k.Name]
		if tunedToday[k.Name] {
			continue // one step per knob per day
		}
		if tunedByLesson[k.Name+"|"+t.lesson.ID] {
			continue // one step per lesson, ever
		}
		// Clamp the target to hard bounds, then to one step from current.
		want := clampF(t.target, k.Min, k.Max)
		next := clampF(want, cur-k.MaxStep, cur+k.MaxStep)
		// One-way ratchet: only move in the knob's safer direction.
		if k.TightenUp && next <= cur || !k.TightenUp && next >= cur {
			continue
		}
		vals[k.Name] = next
		changes = append(changes, TuneChange{
			At: now, Knob: k.Name, Old: cur, New: next,
			LessonID: t.lesson.ID, Pattern: t.lesson.Pattern, Action: "tune",
		})
	}
	// Intervene level only: the actions that stop something trading rather than nudging
	// a number. Both are expressed as env knobs breen already understands, so Lou is
	// writing configuration rather than reaching into the trading path.
	if level.mayIntervene() {
		var interventions []TuneChange
		extra, interventions = applyInterventions(extra, lessons, now)
		changes = append(changes, interventions...)
	}

	if len(changes) == 0 {
		return nil, nil
	}
	if err := writeOverrides(dir, vals, extra, now); err != nil {
		return nil, err
	}
	return changes, appendTuneHistory(dir, changes)
}

// interventionKnobs maps a lesson-ID prefix to the env knob that acts on it. Both knobs
// are comma-separated SETS that breen parses, so Lou's job is only ever to add a member
// — never to rewrite or remove one, which keeps a human's hand-added entry safe.
var interventionKnobs = []struct {
	prefix string // lesson ID prefix
	knob   string // env knob in overrides.env
	action string // TuneChange.Action
}{
	{"persona/", "SHADOW_PERSONAS", "bench"},
	{"family/", "FAMILY_BLACKLIST", "blacklist"},
}

// applyInterventions adds personas and families named by VALIDATED, money-losing
// lessons to the shadow and blacklist sets. Returns the updated `extra` lines and the
// changes recorded.
//
// Membership is a set union: an entry is added once and never removed by Lou. Undoing
// an intervention is a human decision (edit overrides.env, or `lou -rollback`), which is
// the deliberate asymmetry — Lou may stop the book doing something, but only a person
// may start it again.
func applyInterventions(extra []string, lessons map[string]*Lesson, now time.Time) ([]string, []TuneChange) {
	current := map[string]map[string]bool{}
	for _, ik := range interventionKnobs {
		current[ik.knob] = map[string]bool{}
	}
	// Seed from whatever overrides.env already carries.
	kept := extra[:0:0]
	for _, line := range extra {
		key, val, ok := strings.Cut(line, "=")
		if set, managed := current[strings.TrimSpace(key)]; ok && managed {
			for _, m := range strings.Split(val, ",") {
				if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
					set[m] = true
				}
			}
			continue // re-emitted below from the merged set
		}
		kept = append(kept, line)
	}

	var changes []TuneChange
	for _, ik := range interventionKnobs {
		for _, l := range lessons {
			if l.Status != "validated" || !strings.HasPrefix(l.ID, ik.prefix) {
				continue
			}
			if l.Evidence.PnL >= 0 {
				continue // a lesson about something that MAKES money is not a reason to stop it
			}
			name := strings.ToLower(strings.TrimPrefix(l.ID, ik.prefix))
			if name == "" || current[ik.knob][name] {
				continue
			}
			current[ik.knob][name] = true
			changes = append(changes, TuneChange{
				At: now, Knob: ik.knob, Target: name,
				LessonID: l.ID, Pattern: l.Pattern, Action: ik.action,
			})
		}
	}

	// Re-emit each managed set, sorted so the file is stable between passes.
	for _, ik := range interventionKnobs {
		members := make([]string, 0, len(current[ik.knob]))
		for m := range current[ik.knob] {
			members = append(members, m)
		}
		if len(members) == 0 {
			continue
		}
		sort.Strings(members)
		kept = append(kept, ik.knob+"="+strings.Join(members, ","))
	}
	return kept, changes
}

// rollback removes overrides.env and records the reversal.
func rollback(dir string, now time.Time) error {
	vals, _, err := readOverrides(dir)
	if err != nil {
		return err
	}
	var changes []TuneChange
	for _, k := range knobWhitelist {
		if vals[k.Name] != k.Default {
			changes = append(changes, TuneChange{
				At: now, Knob: k.Name, Old: vals[k.Name], New: k.Default, Action: "rollback",
			})
		}
	}
	path := filepath.Join(dir, "overrides.env")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return appendTuneHistory(dir, changes)
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
