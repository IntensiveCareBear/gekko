package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Lou phase 2 (docs/lou-learning-loop-proposal.md): the lesson book Lou
// distills from past runs flows back into the proposal prompt, so the LLM sees
// "here's what burned us before" at decision time. Read-only and fail-safe: no
// file (Lou never ran, or LOU_LESSONS_FILE unset/wrong) means zero lessons and
// proposals work exactly as before.

// LouLesson is the subset of Lou's lesson schema the prompt needs
// (lou/lessons.go is the writer and source of truth).
type LouLesson struct {
	ID         string  `json:"id"`
	Pattern    string  `json:"pattern"`
	Confidence float64 `json:"confidence"`
	Status     string  `json:"status"`
	Evidence   struct {
		Trades     int     `json:"trades"`
		WinRatePct float64 `json:"win_rate_pct"`
	} `json:"evidence"`
}

// LessonBook caches lessons.json, reloading when the file's mtime changes
// (checked at most every 30s — Lou rewrites the file once a day, the hot path
// reads it per proposal).
type LessonBook struct {
	mu        sync.Mutex
	path      string
	lessons   []LouLesson
	mtime     time.Time
	lastCheck time.Time
}

var lessonBook = &LessonBook{path: getEnv("LOU_LESSONS_FILE", "runs/lou/lessons.json")}

// refreshLocked reloads the file if its mtime moved. Any error (missing file,
// bad JSON mid-rewrite) keeps the previous in-memory book.
func (lb *LessonBook) refreshLocked(now time.Time) {
	if now.Sub(lb.lastCheck) < 30*time.Second {
		return
	}
	lb.lastCheck = now
	st, err := os.Stat(lb.path)
	if err != nil {
		lb.lessons = nil // book removed (e.g. run reset) — stop citing it
		return
	}
	if st.ModTime().Equal(lb.mtime) {
		return
	}
	raw, err := os.ReadFile(lb.path)
	if err != nil {
		return
	}
	var all []LouLesson
	if err := json.Unmarshal(raw, &all); err != nil {
		return // likely caught mid-rewrite; next check picks it up
	}
	active := all[:0]
	for _, l := range all {
		if l.Status != "retired" {
			active = append(active, l)
		}
	}
	lb.lessons = active
	lb.mtime = st.ModTime()
	log.Printf("[LESSONS] Loaded %d active lessons from %s", len(active), lb.path)
}

// PromptLines picks the lessons worth showing for one proposal: every
// family-specific lesson whose family stem appears in the candidate ticker,
// then the general book (validated before proposed, higher confidence first),
// capped at k lines total. Family lessons for OTHER families are noise and are
// never included.
func (lb *LessonBook) PromptLines(ticker string, k int) []string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.refreshLocked(time.Now())
	if len(lb.lessons) == 0 || k <= 0 {
		return nil
	}

	var family, general []LouLesson
	for _, l := range lb.lessons {
		if stem, ok := strings.CutPrefix(l.ID, "family/"); ok {
			if ticker != "" && strings.Contains(ticker, stem) {
				family = append(family, l)
			}
			continue
		}
		general = append(general, l)
	}
	rank := func(ls []LouLesson) {
		sort.Slice(ls, func(i, j int) bool {
			if ls[i].Status != ls[j].Status {
				return ls[i].Status == "validated"
			}
			return ls[i].Confidence > ls[j].Confidence
		})
	}
	rank(family)
	rank(general)

	picked := append(family, general...)
	if len(picked) > k {
		picked = picked[:k]
	}
	lines := make([]string, 0, len(picked))
	for _, l := range picked {
		lines = append(lines, fmt.Sprintf("%s [%s, %d trades, %.0f%% win rate]",
			l.Pattern, l.Status, l.Evidence.Trades, l.Evidence.WinRatePct))
	}
	return lines
}
