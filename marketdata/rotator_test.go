package main

import (
	"fmt"
	"testing"
)

func TestRotatorCoversUniverseAndWraps(t *testing.T) {
	universe := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		universe = append(universe, fmt.Sprintf("T-%02d", i))
	}

	rot := &rotator{}
	seen := map[string]int{}
	wraps := 0
	// 25 tickers at budget 10 → a pass completes on every 3rd cycle (10+10+5…
	// the rotator wraps mid-batch, re-serving early tickers as padding).
	for cycle := 0; cycle < 15; cycle++ {
		batch, wrapped, _ := rot.next(universe, 10)
		if len(batch) != 10 {
			t.Fatalf("cycle %d: expected batch of 10, got %d", cycle, len(batch))
		}
		for _, tk := range batch {
			seen[tk]++
		}
		if wrapped {
			wraps++
		}
	}
	if len(seen) != 25 {
		t.Errorf("expected every universe ticker polled, got %d/25", len(seen))
	}
	if wraps < 4 {
		t.Errorf("expected at least 4 completed passes over 15 cycles, got %d", wraps)
	}
}

func TestRotatorHandlesShrinkingUniverse(t *testing.T) {
	rot := &rotator{}
	big := []string{"A", "B", "C", "D", "E"}
	rot.next(big, 4) // cursor advances to 4
	small := []string{"X", "Y"}
	batch, _, _ := rot.next(small, 4) // cursor 4 ≥ len 2 → reset, budget clamped
	if len(batch) != 2 || batch[0] != "X" {
		t.Errorf("expected clamped batch [X Y] after shrink, got %v", batch)
	}
	if batch, _, _ := rot.next(nil, 4); batch != nil {
		t.Errorf("expected nil batch for empty universe, got %v", batch)
	}
}

func TestSplitPriority(t *testing.T) {
	priority, universe := splitPriority(
		[]string{"HELD-1", " ", "WATCH-1", "HELD-1"},
		[]string{"HELD-1", "UNI-1", "UNI-2", "WATCH-1"},
	)
	if len(priority) != 2 || priority[0] != "HELD-1" || priority[1] != "WATCH-1" {
		t.Errorf("unexpected priority set: %v", priority)
	}
	if len(universe) != 2 || universe[0] != "UNI-1" || universe[1] != "UNI-2" {
		t.Errorf("expected universe minus priority, got %v", universe)
	}
}
