package main

import (
	"testing"
	"time"
)

func testDet() *activityDetector {
	return newActivityDetector(activityConfig{TrendPct: 8, MinMove: 0.03, VolSurge: 3, MinSamples: 6, Debounce: 10 * time.Minute, MinMid: 0.03})
}

func snap(mid, trend, surge float64, samples int) Momentum {
	return Momentum{Ticker: "KXBTCD-26OCT1517-T81749.99", Mid: mid, TrendPct: trend, VolSurge: surge, Samples: samples}
}

func TestActivity_FiresOnARealMove(t *testing.T) {
	d := testDet()
	ev, ok := d.check(snap(0.44, 10, 1, 12), 0.43, 0.45, time.Now())
	if !ok || ev.Kind != "trend" || ev.Magnitude != 10 || ev.Bid != 0.43 {
		t.Fatalf("want a trend event, got %+v ok=%v", ev, ok)
	}
}

func TestActivity_IgnoresNoise(t *testing.T) {
	now := time.Now()
	cases := map[string]Momentum{
		"penny book: +50% is one cent":     snap(0.04, 50, 1, 12), // move 0.0133 < 0.03
		"small trend":                      snap(0.44, 3, 1, 12),
		"too few samples":                  snap(0.44, 20, 1, 3),
		"settled-looking book":             snap(0.99, 20, 1, 12),
		"quiet book":                       snap(0.50, 0.5, 1.1, 12),
		"trend but price barely moved":     snap(0.10, 9, 1, 12), // 0.10 - 0.0917 = 0.0083
	}
	for name, m := range cases {
		if ev, ok := testDet().check(m, 0.4, 0.5, now); ok {
			t.Errorf("%s: should not fire, got %+v", name, ev)
		}
	}
}

func TestActivity_VolSurgeAndDebounce(t *testing.T) {
	d := testDet()
	now := time.Now()
	ev, ok := d.check(snap(0.50, 0.5, 4, 12), 0.49, 0.51, now)
	if !ok || ev.Kind != "vol_surge" {
		t.Fatalf("want vol_surge, got %+v ok=%v", ev, ok)
	}
	if _, ok := d.check(snap(0.50, 20, 4, 12), 0.49, 0.51, now.Add(5*time.Minute)); ok {
		t.Error("a second event inside the debounce window must be suppressed")
	}
	if _, ok := d.check(snap(0.50, 20, 4, 12), 0.49, 0.51, now.Add(11*time.Minute)); !ok {
		t.Error("after the debounce window the ticker may fire again")
	}
	if _, ok := d.check(snap(0.50, 20, 4, 12), 0, 0.51, now.Add(30*time.Minute)); ok {
		t.Error("a one-sided book must not fire")
	}
}
