package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const lessonsFixture = `[
  {"id":"entry-band/0.50-0.65","pattern":"Entries priced 0.50-0.65 lose","confidence":0.95,"status":"validated","evidence":{"trades":1187,"win_rate_pct":11}},
  {"id":"family/KXINXY-26DEC31H1600","pattern":"Family KXINXY-26DEC31H1600 is a repeat loser","confidence":0.87,"status":"proposed","evidence":{"trades":167,"win_rate_pct":17}},
  {"id":"stops/noise-stopouts","pattern":"Stop-losses fire on noise","confidence":0.57,"status":"proposed","evidence":{"trades":33,"win_rate_pct":3}},
  {"id":"entry-band/0.00-0.10","pattern":"Old retired claim","confidence":0.4,"status":"retired","evidence":{"trades":20,"win_rate_pct":50}}
]`

func newTestBook(t *testing.T, content string) *LessonBook {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lessons.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return &LessonBook{path: path}
}

func TestLessonBook_SelectionAndOrdering(t *testing.T) {
	lb := newTestBook(t, lessonsFixture)

	// Unrelated ticker: general lessons only, validated first, no family noise,
	// no retired.
	lines := lb.PromptLines("KXBTCD-26JUL0717-T63749.99", 4)
	if len(lines) != 2 {
		t.Fatalf("expected 2 general lessons, got %v", lines)
	}
	if !strings.Contains(lines[0], "0.50-0.65") || !strings.Contains(lines[0], "validated") {
		t.Errorf("validated lesson should rank first: %v", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "KXINXY") || strings.Contains(l, "retired") {
			t.Errorf("unexpected family/retired lesson: %s", l)
		}
	}

	// Matching family ticker: its family lesson leads.
	lines = lb.PromptLines("KXINXY-26DEC31H1600-B5900", 4)
	if len(lines) != 3 || !strings.Contains(lines[0], "KXINXY") {
		t.Fatalf("family lesson should lead for its own ticker, got %v", lines)
	}

	// Cap respected.
	if got := lb.PromptLines("KXINXY-26DEC31H1600-B5900", 1); len(got) != 1 {
		t.Errorf("k=1 must cap output, got %v", got)
	}
}

func TestLessonBook_MissingFileIsSilent(t *testing.T) {
	lb := &LessonBook{path: filepath.Join(t.TempDir(), "nope.json")}
	if got := lb.PromptLines("KXFOO-26DEC31-B1", 4); got != nil {
		t.Errorf("missing book must yield no lines, got %v", got)
	}
}

func TestLessonBook_ReloadsOnMtimeChange(t *testing.T) {
	lb := newTestBook(t, lessonsFixture)
	if got := lb.PromptLines("X", 4); len(got) != 2 {
		t.Fatalf("initial load failed: %v", got)
	}

	// Rewrite the book; force the staleness window and mtime to move.
	updated := `[{"id":"venue/polymarket","pattern":"Venue polymarket underperforms","confidence":0.9,"status":"validated","evidence":{"trades":290,"win_rate_pct":21}}]`
	if err := os.WriteFile(lb.path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(lb.path, future, future); err != nil {
		t.Fatal(err)
	}
	lb.mu.Lock()
	lb.lastCheck = time.Time{}
	lb.mu.Unlock()

	got := lb.PromptLines("X", 4)
	if len(got) != 1 || !strings.Contains(got[0], "polymarket") {
		t.Errorf("book should reload after mtime change, got %v", got)
	}
}
