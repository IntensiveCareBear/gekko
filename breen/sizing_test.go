package main

import (
	"math"
	"testing"
)

func TestDrawdownScalar(t *testing.T) {
	cases := []struct{ dd, ease, halt, floor, want float64 }{
		{0, 4, 10, 0.25, 1},     // flat book: full size
		{4, 4, 10, 0.25, 1},     // at easeStart: still full
		{7, 4, 10, 0.25, 0.625}, // midpoint of the ramp
		{10, 4, 10, 0.25, 0.25}, // at the halt: floor
		{15, 4, 10, 0.25, 0.25}, // past the halt: floor (riskGate rejects anyway)
		{6, 10, 10, 0.25, 1},    // haltPct <= easeStart: degenerate, no ramp
		{6, 4, 0, 0.25, 1},      // halt disabled: no ramp
		{7, 4, 10, 2.0, 1},      // floor clamped to 1: no shrink
		{7, 4, 10, -1.0, 0.5},   // floor clamped to 0
	}
	for _, c := range cases {
		if got := drawdownScalar(c.dd, c.ease, c.halt, c.floor); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("drawdownScalar(%v,%v,%v,%v) = %v, want %v", c.dd, c.ease, c.halt, c.floor, got, c.want)
		}
	}
}
