package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Lou's autonomy level (2026-08-25 desk review, fix #6).
//
// Lou's whitelist held three bounded price knobs against nineteen validated lessons:
// it could see that a persona was losing money for two months and had no way to act on
// it. Widening that is a judgement call about how much rope an offline process gets, so
// rather than one more boolean this is an explicit ladder, set from the dashboard and
// read at the next nightly pass:
//
//	observe    — analyze, write lessons and the digest, change NOTHING.
//	tune       — may also adjust the bounded numeric knobs (entry prices, stop floor).
//	             Every move is clamped, one step per knob per day, and one-way toward
//	             the safer direction. This is what `-autotune` has always meant.
//	intervene  — may ALSO stop things trading: bench a persona into shadow mode, and
//	             blacklist a contract family. These are the actions with teeth, which
//	             is exactly why they sit behind their own rung.
//
// Nothing here is irreversible: every level writes only to runs/lou/overrides.env,
// which run.sh sources at launch, and `lou -rollback` deletes it.
type Autonomy int

const (
	AutonomyObserve Autonomy = iota
	AutonomyTune
	AutonomyIntervene
)

func (a Autonomy) String() string {
	switch a {
	case AutonomyIntervene:
		return "intervene"
	case AutonomyTune:
		return "tune"
	default:
		return "observe"
	}
}

// mayTune reports whether this level permits numeric knob changes.
func (a Autonomy) mayTune() bool { return a >= AutonomyTune }

// mayIntervene reports whether this level permits benching and blacklisting.
func (a Autonomy) mayIntervene() bool { return a >= AutonomyIntervene }

// parseAutonomy maps a name to a level, reporting whether it was recognised so callers
// can distinguish "not set" from "set to observe".
func parseAutonomy(s string) (Autonomy, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "observe":
		return AutonomyObserve, true
	case "tune":
		return AutonomyTune, true
	case "intervene":
		return AutonomyIntervene, true
	}
	return AutonomyObserve, false
}

// autonomyFile is where the dashboard writes the level. One line, one word.
const autonomyFile = "autonomy"

// resolveAutonomy decides this pass's level, most explicit source first:
//
//  1. LOU_AUTONOMY in the environment — the manual override, matching how every other
//     knob in this system can be forced from the shell.
//  2. runs/lou/autonomy — what the dashboard control writes.
//  3. the legacy -autotune flag: set means "tune", absent means "observe".
//
// The flag stays last so an existing nightly.sh that passes -autotune keeps behaving
// exactly as it did until someone actually sets a level.
func resolveAutonomy(dir string, autoTuneFlag bool) Autonomy {
	if lvl, ok := parseAutonomy(os.Getenv("LOU_AUTONOMY")); ok {
		return lvl
	}
	if raw, err := os.ReadFile(filepath.Join(dir, autonomyFile)); err == nil {
		if lvl, ok := parseAutonomy(string(raw)); ok {
			return lvl
		}
	}
	if autoTuneFlag {
		return AutonomyTune
	}
	return AutonomyObserve
}
