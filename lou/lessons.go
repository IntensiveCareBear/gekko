package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Lesson is a small, structured, evidence-backed claim about how Gekko trades —
// the unit of Lou's memory (docs/lou-learning-loop-proposal.md). Phase 1 is
// advisory: SuggestedAction is a recommendation for a human, never applied.
type Lesson struct {
	// ID is the stable pattern key (analyzer + subject, e.g. "entry-band/0.50-0.65").
	// A later pass with fresh data updates the same lesson instead of duplicating it.
	ID              string   `json:"id"`
	Pattern         string   `json:"pattern"`          // the human-readable claim
	Evidence        Evidence `json:"evidence"`         // what supports it, cumulative across runs
	Confidence      float64  `json:"confidence"`       // sample-size shrinkage, see confidence()
	SuggestedAction string   `json:"suggested_action"` // optional advisory knob nudge
	// Status lifecycle: proposed (first seen) → validated (still holds with
	// evidence from ≥2 runs) → retired (cumulative evidence stopped supporting it).
	Status      string    `json:"status"`
	FirstSeen   time.Time `json:"first_seen"`
	LastUpdated time.Time `json:"last_updated"`
	RetiredNote string    `json:"retired_note,omitempty"`
}

// Evidence is the quantitative support for a lesson, recomputed over the whole
// runs/ corpus each pass (cumulative, so a new run strengthens or weakens the
// same lesson rather than starting over).
type Evidence struct {
	Trades     int      `json:"trades"`
	Wins       int      `json:"wins"`
	Losses     int      `json:"losses"`
	PnL        float64  `json:"pnl"`
	WinRatePct float64  `json:"win_rate_pct"` // wins / (wins+losses); $0 scratches excluded
	Runs       []string `json:"runs"`         // run ids the evidence came from
	Detail     string   `json:"detail,omitempty"`
}

// confidence maps sample size to a weight in [0,0.95] via n/(n+25): ~0.5 at 25
// trades, ~0.8 at 100. Deliberately dumb and monotonic — phase 1 needs an
// ordering for the digest, not a calibrated probability.
func confidence(n int) float64 {
	c := float64(n) / (float64(n) + 25)
	if c > 0.95 {
		c = 0.95
	}
	return c
}

// Store is the persisted lesson set: lessons.json holds current state, keyed by
// lesson ID; lessons_history.jsonl appends one snapshot per change for audit.
type Store struct {
	dir     string
	Lessons map[string]*Lesson
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, Lessons: map[string]*Lesson{}}
	raw, err := os.ReadFile(filepath.Join(dir, "lessons.json"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Lesson
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("lessons.json corrupt: %w", err)
	}
	for _, l := range list {
		s.Lessons[l.ID] = l
	}
	return s, nil
}

// Changes summarizes one Merge pass for the digest.
type Changes struct {
	New       []*Lesson // first sighting → proposed
	Validated []*Lesson // promoted proposed → validated this pass
	Updated   []*Lesson // evidence refreshed, status unchanged
	Retired   []*Lesson // cumulative evidence no longer supports the claim
}

// Merge folds this pass's candidate lessons (recomputed over all runs) into the
// store. Candidates keep/refresh their lesson; an active lesson with no matching
// candidate is retired — the pattern stopped showing up in the cumulative data.
func (s *Store) Merge(candidates []*Lesson, now time.Time) Changes {
	var ch Changes
	seen := map[string]bool{}

	for _, c := range candidates {
		seen[c.ID] = true
		prev, ok := s.Lessons[c.ID]
		if !ok {
			c.Status = "proposed"
			c.FirstSeen = now
			c.LastUpdated = now
			c.Confidence = confidence(c.Evidence.Trades)
			s.Lessons[c.ID] = c
			ch.New = append(ch.New, c)
			continue
		}
		prev.Pattern = c.Pattern
		prev.Evidence = c.Evidence
		prev.SuggestedAction = c.SuggestedAction
		prev.Confidence = confidence(c.Evidence.Trades)
		prev.LastUpdated = now
		prev.RetiredNote = ""
		switch {
		case prev.Status == "proposed" && len(c.Evidence.Runs) >= 2:
			prev.Status = "validated"
			ch.Validated = append(ch.Validated, prev)
		case prev.Status == "retired":
			// The pattern re-emerged after retirement: back to proposed, it must
			// re-earn validation.
			prev.Status = "proposed"
			ch.New = append(ch.New, prev)
		default:
			ch.Updated = append(ch.Updated, prev)
		}
	}

	for _, l := range s.Lessons {
		if !seen[l.ID] && l.Status != "retired" {
			l.Status = "retired"
			l.LastUpdated = now
			l.RetiredNote = "cumulative evidence no longer meets the analyzer's threshold"
			ch.Retired = append(ch.Retired, l)
		}
	}
	return ch
}

// Save writes lessons.json (sorted by ID for stable diffs) and appends every
// changed lesson to lessons_history.jsonl with the pass timestamp.
func (s *Store) Save(ch Changes, now time.Time) error {
	list := make([]*Lesson, 0, len(s.Lessons))
	for _, l := range s.Lessons {
		list = append(list, l)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, "lessons.json"), raw, 0o644); err != nil {
		return err
	}

	hist, err := os.OpenFile(filepath.Join(s.dir, "lessons_history.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer hist.Close()
	enc := json.NewEncoder(hist)
	appendAll := func(action string, ls []*Lesson) error {
		for _, l := range ls {
			if err := enc.Encode(struct {
				At     time.Time `json:"at"`
				Action string    `json:"action"`
				Lesson *Lesson   `json:"lesson"`
			}{now, action, l}); err != nil {
				return err
			}
		}
		return nil
	}
	for _, group := range []struct {
		action string
		ls     []*Lesson
	}{{"proposed", ch.New}, {"validated", ch.Validated}, {"updated", ch.Updated}, {"retired", ch.Retired}} {
		if err := appendAll(group.action, group.ls); err != nil {
			return err
		}
	}
	return nil
}
