package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func newAlloc() *AdaptiveAllocator {
	// rdb=nil → load/persist are no-ops; pure in-memory behavior.
	return NewAdaptiveAllocator(nil, 0.5, 0.5, 0.5, 0.5, 1.5, 3)
}

func TestWeightFor_ExploreGateBelowMinTrades(t *testing.T) {
	// Strong positive score but only 2 trades (< minTrades 3) → neutral 1.0.
	if w := weightFor(1.0, 2, 3, 0.5, 0.5, 0.5, 1.5); w != 1.0 {
		t.Errorf("under min trades should be neutral 1.0, got %.3f", w)
	}
}

func TestWeightFor_WinnerVsLoser(t *testing.T) {
	win := weightFor(1.0, 10, 3, 0.5, 0.5, 0.5, 1.5)   // 1 + 0.5*1 = 1.5
	lose := weightFor(-1.0, 10, 3, 0.5, 0.5, 0.5, 1.5) // 1 - 0.5 = 0.5 (== floor)
	if win <= 1.0 {
		t.Errorf("winner weight should exceed 1.0, got %.3f", win)
	}
	if lose >= 1.0 {
		t.Errorf("loser weight should be below 1.0, got %.3f", lose)
	}
	if win < 0.5 || win > 1.5 || lose < 0.5 || lose > 1.5 {
		t.Errorf("weights out of [0.5,1.5]: win=%.3f lose=%.3f", win, lose)
	}
}

func TestWeightFor_ExploreFloorNeverStarves(t *testing.T) {
	// Even a catastrophic score can't drop below the explore floor.
	if w := weightFor(-100, 50, 3, 0.5, 0.6, 0.5, 1.5); w < 0.6 {
		t.Errorf("weight %.3f dropped below explore floor 0.6", w)
	}
}

func TestObserve_EWMAUpdatesAndDecays(t *testing.T) {
	a := newAlloc() // decay 0.5
	// First observation seeds the score directly.
	a.Observe("bull", 100) // outcome capped to +1.0
	if got := a.score["bull"]; math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("first observe should seed score=1.0, got %.3f", got)
	}
	// A losing trade pulls it halfway (decay 0.5) toward -1.0 → 0.0.
	a.Observe("bull", -100)
	if got := a.score["bull"]; math.Abs(got-0.0) > 1e-9 {
		t.Errorf("EWMA after win then loss (decay .5) should be 0.0, got %.3f", got)
	}
	if a.count["bull"] != 2 {
		t.Errorf("count = %d, want 2", a.count["bull"])
	}
}

func TestWeights_WinnerOutranksLoserAfterEnoughTrades(t *testing.T) {
	a := newAlloc()
	for i := 0; i < 6; i++ {
		a.Observe("bull", 40)  // consistent winner
		a.Observe("bear", -40) // consistent loser
	}
	w := a.Weights()
	if w["bull"] <= w["bear"] {
		t.Errorf("winner should outweigh loser: bull=%.3f bear=%.3f", w["bull"], w["bear"])
	}
	if w["bull"] <= 1.0 || w["bear"] >= 1.0 {
		t.Errorf("expected bull>1>bear, got bull=%.3f bear=%.3f", w["bull"], w["bear"])
	}
}

func TestBench_TriggersAtMinTradesNotBefore(t *testing.T) {
	a := newAlloc()
	a.SetBenchPolicy(-0.30, 20, 12*time.Hour)

	// 19 straight losers: score deeply negative but under the trade floor → not benched.
	for i := 0; i < 19; i++ {
		a.Observe("bull", -60)
	}
	if a.IsBenched("bull") {
		t.Fatal("persona must not be benched before reaching the min-trades floor")
	}
	// The 20th loser crosses the floor with score < -0.30 → benched.
	a.Observe("bull", -60)
	if !a.IsBenched("bull") {
		t.Fatal("persona should be benched at 20 trades with a deeply negative score")
	}
	if w := a.Weights(); w["bull"] != 0 {
		t.Errorf("benched persona must get an EXPLICIT 0 weight, got %.3f", w["bull"])
	}
}

func TestBench_ExpiryResetsToNeutralProbation(t *testing.T) {
	a := newAlloc()
	a.SetBenchPolicy(-0.30, 5, 12*time.Hour)
	for i := 0; i < 5; i++ {
		a.Observe("bear", -80)
	}
	if !a.IsBenched("bear") {
		t.Fatal("expected bear benched after 5 heavy losers")
	}
	// Expire the bench manually.
	a.mu.Lock()
	a.benched["bear"] = time.Now().Add(-time.Minute)
	a.mu.Unlock()

	if a.IsBenched("bear") {
		t.Fatal("expired bench should be released on the next check")
	}
	// Score/count reset → the min-trades explore gate holds it at neutral 1.0.
	if w := a.Weights(); w["bear"] != 1.0 {
		t.Errorf("released persona should re-enter at neutral 1.0 (probation), got %.3f", w["bear"])
	}
}

func TestBench_PersistsAcrossRestart(t *testing.T) {
	a := newAlloc()
	a.SetBenchPolicy(-0.30, 5, 12*time.Hour)
	for i := 0; i < 5; i++ {
		a.Observe("bull", -80)
	}
	a.mu.Lock()
	p := allocPersisted{Score: a.score, Count: a.count, Benched: map[string]int64{}}
	for persona, until := range a.benched {
		p.Benched[persona] = until.UnixMilli()
	}
	a.mu.Unlock()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored allocPersisted
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if until, ok := restored.Benched["bull"]; !ok || time.UnixMilli(until).Before(time.Now()) {
		t.Errorf("bench should roundtrip persistence with a future expiry, got %+v", restored.Benched)
	}
}

func TestWeightFor_FloorAndMinWeightDropTogether(t *testing.T) {
	// With minW and floor both at 0.25 a chronic loser bottoms out at 0.25 —
	// the old hardcoded minW 0.5 made any lower explore floor a no-op.
	if w := weightFor(-1.0, 50, 3, 0.5, 0.25, 0.25, 1.5); math.Abs(w-0.5) > 1e-9 {
		// 1 + 0.5*(-1) = 0.5, still above the clamp — use a stronger gain case below.
		t.Errorf("gain 0.5 loser should sit at 0.5, got %.3f", w)
	}
	if w := weightFor(-1.0, 50, 3, 1.0, 0.25, 0.25, 1.5); math.Abs(w-0.25) > 1e-9 {
		t.Errorf("gain 1.0 loser should bottom at minW=floor=0.25, got %.3f", w)
	}
}
