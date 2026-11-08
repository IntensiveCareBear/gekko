package main

// drawdownScalar ramps BUY size from 1.0 at easeStart (% below starting capital) down to
// floor at haltPct, replacing the cliff between full size and no entries at all (the
// MAX_DRAWDOWN_HALT_PCT halt stays as the backstop). Pure. A degenerate config
// (haltPct <= easeStart, or no halt) returns 1.0: sizing never invents a ramp.
func drawdownScalar(ddPct, easeStart, haltPct, floor float64) float64 {
	if haltPct <= 0 || haltPct <= easeStart || ddPct <= easeStart {
		return 1.0
	}
	if floor < 0 {
		floor = 0
	}
	if floor > 1 {
		floor = 1
	}
	if ddPct >= haltPct {
		return floor
	}
	return 1.0 - (1.0-floor)*(ddPct-easeStart)/(haltPct-easeStart)
}
