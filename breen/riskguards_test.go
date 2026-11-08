package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// newGuardTracker builds a tracker with the post-mortem guards configured. It mirrors
// newGateTracker but lives here so the riskgate suite stays focused on the base caps.
func newGuardTracker(t *testing.T, caps RiskCaps) *PositionTracker {
	t.Helper()
	tmp := "./test_riskguard_ledger.db"
	t.Cleanup(func() { os.Remove(tmp) })
	ledger := NewImmutableLedger(tmp)
	tr := NewPositionTracker("", ledger, NewBrokerClient("./none.sock", "t"), true, 100000, time.Hour, 2*time.Hour)
	tr.SetRiskCaps(caps)
	return tr
}

func TestRiskGate_ContractExposurePctCap(t *testing.T) {
	// 2% of $100k NAV = $2,000; absolute cap is generous so the pct cap binds.
	tr := newGuardTracker(t, RiskCaps{MaxContractExposure: 15000, MaxContractExposurePct: 0.02})
	tr.lastNAV = 100000

	ok := &StrategyProposal{ContractID: "KXRATECUT", Action: "BUY", AveragePrice: 0.20, TotalSize: 9000} // $1,800 < $2k
	if allowed, reason := tr.riskGate(ok); !allowed {
		t.Errorf("expected $1,800 order allowed under 2%% NAV cap, rejected: %s", reason)
	}
	big := &StrategyProposal{ContractID: "KXRATECUT", Action: "BUY", AveragePrice: 0.30, TotalSize: 10143} // $3,043 > $2k
	if allowed, _ := tr.riskGate(big); allowed {
		t.Error("expected rejection: order exceeds 2% NAV per-contract cap")
	}
}

func TestRiskGate_OneStrikePerFamily(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{OneStrikePerFamily: true})

	// Holding T2.0 of the GDP event → a BUY on sibling strike T2.5 is rejected.
	tr.positions["KXGDP-26JUL30-T2.0"] = &Position{ContractID: "KXGDP-26JUL30-T2.0", EntryPrice: 0.58, Size: 1000, Timestamp: time.Now()}
	sibling := &StrategyProposal{ContractID: "KXGDP-26JUL30-T2.5", Action: "BUY", AveragePrice: 0.23, TotalSize: 100}
	if ok, _ := tr.riskGate(sibling); ok {
		t.Error("expected rejection: a different strike of the family is already held")
	}

	// Adding to the SAME strike is not a sibling — must still pass.
	add := &StrategyProposal{ContractID: "KXGDP-26JUL30-T2.0", Action: "BUY", AveragePrice: 0.58, TotalSize: 100}
	if ok, reason := tr.riskGate(add); !ok {
		t.Errorf("adding to the held strike should pass, rejected: %s", reason)
	}

	// An unrelated family is unaffected.
	other := &StrategyProposal{ContractID: "KXBRENTW-26JUL1717-T81.99", Action: "BUY", AveragePrice: 0.30, TotalSize: 100}
	if ok, reason := tr.riskGate(other); !ok {
		t.Errorf("unrelated family should pass, rejected: %s", reason)
	}

	// Flag off → sibling stacking is allowed again.
	tr.SetRiskCaps(RiskCaps{OneStrikePerFamily: false})
	if ok, reason := tr.riskGate(sibling); !ok {
		t.Errorf("with the guard disabled the sibling should pass, rejected: %s", reason)
	}
}

func TestRiskGate_PerContractLossLimit(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{PerContractLossLimit: 300})
	tr.contractRealizedPnL["KXFDVOPENSEA"] = -350 // already past the limit this run

	blocked := &StrategyProposal{ContractID: "KXFDVOPENSEA", Action: "BUY", AveragePrice: 0.5, TotalSize: 100}
	if ok, _ := tr.riskGate(blocked); ok {
		t.Error("expected rejection: contract has bled past the per-contract loss limit")
	}
	// A different, healthy contract is unaffected.
	other := &StrategyProposal{ContractID: "FRESH", Action: "BUY", AveragePrice: 0.5, TotalSize: 100}
	if ok, reason := tr.riskGate(other); !ok {
		t.Errorf("unrelated contract should pass, rejected: %s", reason)
	}
	// SELL to exit the bleeding contract must still pass (position held exactly).
	tr.positions["KXFDVOPENSEA"] = &Position{ContractID: "KXFDVOPENSEA", EntryPrice: 0.5, Size: 100, Timestamp: time.Now()}
	exit := &StrategyProposal{ContractID: "KXFDVOPENSEA", Action: "SELL", AveragePrice: 0.4, TotalSize: 100}
	if ok, reason := tr.riskGate(exit); !ok {
		t.Errorf("exit of a loss-limited contract should pass, rejected: %s", reason)
	}
}

func TestRiskGate_ExitCooldown(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{ExitCooldown: 30 * time.Minute})

	// Just closed → re-entry blocked.
	tr.lastExit["TESLACEOCHANGE"] = time.Now().Add(-5 * time.Minute)
	soon := &StrategyProposal{ContractID: "TESLACEOCHANGE", Action: "BUY", AveragePrice: 0.5, TotalSize: 100}
	if ok, _ := tr.riskGate(soon); ok {
		t.Error("expected rejection: re-entry within the cooldown window")
	}

	// Closed long ago → re-entry allowed.
	tr.lastExit["TESLACEOCHANGE"] = time.Now().Add(-31 * time.Minute)
	if ok, reason := tr.riskGate(soon); !ok {
		t.Errorf("re-entry after cooldown should pass, rejected: %s", reason)
	}

	// A partial exit arms the cooldown even while the family is still held: the
	// old still-held exemption let a sell-partial/buy-back churn loop run inside
	// one position (2026-07-14 POLY-25312457… spread bleed).
	tr.positions["HELD"] = &Position{ContractID: "HELD", EntryPrice: 0.5, Size: 100, Timestamp: time.Now()}
	tr.lastExit["HELD"] = time.Now() // partial close stamped just now
	add := &StrategyProposal{ContractID: "HELD", Action: "BUY", AveragePrice: 0.5, TotalSize: 100}
	if ok, _ := tr.riskGate(add); ok {
		t.Error("expected rejection: buy-back within cooldown of a partial exit")
	}

	// Adding to a held position with no recent exit is unaffected.
	tr.lastExit["HELD"] = time.Now().Add(-31 * time.Minute)
	if ok, reason := tr.riskGate(add); !ok {
		t.Errorf("adding to an open position outside the cooldown should pass, rejected: %s", reason)
	}
}

func TestRiskGate_DrawdownHalt(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{MaxDrawdownHaltPct: 10})

	// NAV down 12% from the $100k start → halt new entries.
	tr.lastNAV = 88000
	p := &StrategyProposal{ContractID: "ANY", Action: "BUY", AveragePrice: 0.5, TotalSize: 100}
	if ok, _ := tr.riskGate(p); ok {
		t.Error("expected rejection: drawdown past the halt threshold")
	}
	// Exits remain allowed so the book can de-risk (position held exactly).
	tr.positions["ANY"] = &Position{ContractID: "ANY", EntryPrice: 0.5, Size: 100, Timestamp: time.Now()}
	exit := &StrategyProposal{ContractID: "ANY", Action: "SELL", AveragePrice: 0.5, TotalSize: 100}
	if ok, reason := tr.riskGate(exit); !ok {
		t.Errorf("exit during a drawdown halt should pass, rejected: %s", reason)
	}
	// Recovered above the threshold → entries resume.
	tr.lastNAV = 95000
	if ok, reason := tr.riskGate(p); !ok {
		t.Errorf("entries should resume once NAV recovers, rejected: %s", reason)
	}
}

func TestContractFamily(t *testing.T) {
	cases := map[string]string{
		"KXINXY-26DEC31H1600-B8300":                   "KXINXY-26DEC31H1600", // Kalshi SERIES-EVENT-STRIKE → drop strike
		"KXFDVOPENSEA-27JAN01-4999999999":             "KXFDVOPENSEA-27JAN01",
		"JPMCEOCHANGE-27":                             "JPMCEOCHANGE-27",                             // 2 segments → left whole
		"POLY-68238163315667435120694805573568977252": "POLY-68238163315667435120694805573568977252", // unique token, not a strike
		"NODASH": "NODASH",
	}
	for in, want := range cases {
		if got := contractFamily(in); got != want {
			t.Errorf("contractFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRiskGate_LossLimitByFamily(t *testing.T) {
	// The strike-ladder leak: separate strikes of the same event must share one budget.
	tr := newGuardTracker(t, RiskCaps{PerContractLossLimit: 300})
	tr.contractRealizedPnL["KXFDVOPENSEA-27JAN01"] = -350 // family budget already blown

	// A *different strike* of the same event must now be blocked too.
	other := &StrategyProposal{ContractID: "KXFDVOPENSEA-27JAN01-7999999999", Action: "BUY", AveragePrice: 0.5, TotalSize: 100}
	if ok, _ := tr.riskGate(other); ok {
		t.Error("expected rejection: a sibling strike shares the family loss budget")
	}
	// A strike of a *different event* is unaffected.
	fresh := &StrategyProposal{ContractID: "KXFDVOPENSEA-27FEB01-7999999999", Action: "BUY", AveragePrice: 0.5, TotalSize: 100}
	if ok, reason := tr.riskGate(fresh); !ok {
		t.Errorf("a different event should have its own budget, rejected: %s", reason)
	}
}

func TestRiskGate_LossLimitCountsUnrealized(t *testing.T) {
	// The overshoot: a big underwater OPEN position should block further entries before
	// it books its loss on close.
	tr := newGuardTracker(t, RiskCaps{PerContractLossLimit: 300})
	// Realized only -$100 (under the limit), but an open position marked 0.20 vs 0.52
	// entry on 2000 contracts is -$640 unrealized → family bleed -$740.
	tr.contractRealizedPnL["KXFDVOPENSEA-27JAN01"] = -100
	tr.positions["KXFDVOPENSEA-27JAN01-4999999999"] = &Position{
		ContractID: "KXFDVOPENSEA-27JAN01-4999999999", EntryPrice: 0.52, Size: 2000, Timestamp: time.Now(),
	}
	tr.markCache["KXFDVOPENSEA-27JAN01-4999999999"] = markEntry{Price: 0.20, At: time.Now()}

	add := &StrategyProposal{ContractID: "KXFDVOPENSEA-27JAN01-1999999999", Action: "BUY", AveragePrice: 0.5, TotalSize: 100}
	if ok, _ := tr.riskGate(add); ok {
		t.Error("expected rejection: realized + open unrealized loss is past the limit")
	}
}

func TestRiskGate_ExposureCapByFamily(t *testing.T) {
	// Laddering across strikes must not multiply exposure past the per-family cap.
	tr := newGuardTracker(t, RiskCaps{MaxContractExposure: 2000})
	tr.positions["KXINXY-26DEC31H1600-B8300"] = &Position{ContractID: "KXINXY-26DEC31H1600-B8300", EntryPrice: 0.50, Size: 3000, Timestamp: time.Now()} // $1,500
	// A different strike of the same event: $600 more → $2,100 family > $2,000 cap.
	p := &StrategyProposal{ContractID: "KXINXY-26DEC31H1600-B5900", Action: "BUY", AveragePrice: 0.60, TotalSize: 1000}
	if ok, _ := tr.riskGate(p); ok {
		t.Error("expected rejection: family exposure across strikes exceeds the cap")
	}
}

func TestContractCategory(t *testing.T) {
	cases := map[string]string{
		"KXRATECUT-26DEC31":         "macro",
		"KXEFFR-26JUL01-T3.25":      "macro",
		"KXFEDDECISION-26SEP":       "macro",
		"KXCPICOREHEAD-26JUL14-CVH": "macro",
		"KXINXY-26DEC31H1600-B8300": "equity-index",
		"KXNASDAQ100Y-26":           "equity-index",
		"KXHSI-26":                  "equity-index",
		"BTC-100K-DEC":              "crypto",
		// Crypto/exotic keywords extended after the 2026-07-03 run.
		"KXXRPMAXMON-XRP-26JUL31-180":     "crypto",
		"KXFDVOPENSEA-27JAN01-7999999999": "crypto",
		"KXDASH-26AUGORDERS-P1020000000":  "crypto",
		"KXNFTVOL-26DEC31":                "crypto",
		"JPMCEOCHANGE-27":                 "", // single-name, uncategorized
		"POLY-6823816331566743512":        "", // unique token
	}
	for in, want := range cases {
		if got := contractCategory(in); got != want {
			t.Errorf("contractCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRiskGate_CategoryExposureCap(t *testing.T) {
	// $6k combined cap across the macro cluster; different SERIES, one category.
	tr := newGuardTracker(t, RiskCaps{MaxCategoryExposure: 6000})
	tr.positions["KXRATECUT-26DEC31"] = &Position{ContractID: "KXRATECUT-26DEC31", EntryPrice: 0.50, Size: 8000, Timestamp: time.Now()} // $4,000
	tr.positions["KXEFFR-26JUL01-T3"] = &Position{ContractID: "KXEFFR-26JUL01-T3", EntryPrice: 0.40, Size: 4000, Timestamp: time.Now()} // $1,600 → $5,600 macro

	// A THIRD, different macro series that pushes the cluster over $6k → reject.
	over := &StrategyProposal{ContractID: "KXFEDDECISION-26SEP", Action: "BUY", AveragePrice: 0.50, TotalSize: 2000} // +$1,000 → $6,600
	if ok, _ := tr.riskGate(over); ok {
		t.Error("expected rejection: combined macro-category exposure exceeds the cap")
	}
	// An uncorrelated single-name market is unaffected by the macro cluster.
	other := &StrategyProposal{ContractID: "TESLACEOCHANGE-27", Action: "BUY", AveragePrice: 0.50, TotalSize: 2000}
	if ok, reason := tr.riskGate(other); !ok {
		t.Errorf("uncategorized market should not share the macro budget, rejected: %s", reason)
	}
	// Exits from the maxed category still pass.
	exit := &StrategyProposal{ContractID: "KXRATECUT-26DEC31", Action: "SELL", AveragePrice: 0.45, TotalSize: 8000}
	if ok, reason := tr.riskGate(exit); !ok {
		t.Errorf("exit from a capped category should pass, rejected: %s", reason)
	}
}

func TestForceExitNearExpiry(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.allowMockBook = true // no Redis in tests; mark off a synthetic book
	tr.forceExitWindow = time.Hour

	// Imminent resolution (within the window) → force-exited.
	tr.positions["SOON"] = &Position{ContractID: "SOON", EntryPrice: 0.40, Size: 1000, Timestamp: time.Now(), Expiry: time.Now().Add(20 * time.Minute)}
	// Far-off resolution → left open.
	tr.positions["LATER"] = &Position{ContractID: "LATER", EntryPrice: 0.40, Size: 1000, Timestamp: time.Now(), Expiry: time.Now().Add(10 * time.Hour)}

	tr.forceExitNearExpiry(context.Background())

	tr.mu.RLock()
	_, soonOpen := tr.positions["SOON"]
	_, laterOpen := tr.positions["LATER"]
	closed := len(tr.closedTrades)
	tr.mu.RUnlock()

	if soonOpen {
		t.Error("expected the near-expiry position to be force-exited")
	}
	if !laterOpen {
		t.Error("expected the far-from-expiry position to stay open")
	}
	if closed != 1 {
		t.Fatalf("expected exactly 1 recorded close, got %d", closed)
	}
	if got := tr.closedTrades[0].Status; got != "FORCE_EXIT" {
		t.Errorf("expected force-exit status FORCE_EXIT, got %q", got)
	}
}

func TestForceExitDisabledWhenWindowZero(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.allowMockBook = true
	tr.forceExitWindow = 0 // disabled

	tr.positions["SOON"] = &Position{ContractID: "SOON", EntryPrice: 0.40, Size: 1000, Timestamp: time.Now(), Expiry: time.Now().Add(10 * time.Minute)}
	tr.forceExitNearExpiry(context.Background())

	tr.mu.RLock()
	_, open := tr.positions["SOON"]
	tr.mu.RUnlock()
	if !open {
		t.Error("force-exit should be a no-op when the window is disabled")
	}
}

func TestRiskGate_MidBandConvictionGate(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{MidBandLow: 0.35, MidBandHigh: 0.65, MidBandMinConfidence: 0.75})

	// Coin-flip price with weak conviction → rejected.
	weak := &StrategyProposal{ContractID: "KXFEAR-26JUL02", Action: "BUY", AveragePrice: 0.50, Confidence: 0.5, TotalSize: 100}
	if ok, _ := tr.riskGate(weak); ok {
		t.Error("expected rejection: mid-band price with confidence below the floor")
	}
	// Same price with strong multi-persona conviction → allowed.
	strong := &StrategyProposal{ContractID: "KXFEAR-26JUL02", Action: "BUY", AveragePrice: 0.50, Confidence: 0.9, TotalSize: 100}
	if ok, reason := tr.riskGate(strong); !ok {
		t.Errorf("high-conviction mid-band entry should pass, rejected: %s", reason)
	}
	// Prices outside the band pass regardless of conviction.
	cheap := &StrategyProposal{ContractID: "KXFEAR-26JUL02", Action: "BUY", AveragePrice: 0.30, Confidence: 0.1, TotalSize: 100}
	rich := &StrategyProposal{ContractID: "KXFEAR-26JUL02", Action: "BUY", AveragePrice: 0.70, Confidence: 0.1, TotalSize: 100}
	for _, p := range []*StrategyProposal{cheap, rich} {
		if ok, reason := tr.riskGate(p); !ok {
			t.Errorf("price %.2f outside the band should pass, rejected: %s", p.AveragePrice, reason)
		}
	}
	// SELLs of held contracts pass — exits reduce exposure.
	tr.positions["KXFEAR-26JUL02"] = &Position{ContractID: "KXFEAR-26JUL02", EntryPrice: 0.50, Size: 100, Timestamp: time.Now()}
	exit := &StrategyProposal{ContractID: "KXFEAR-26JUL02", Action: "SELL", AveragePrice: 0.50, Confidence: 0.1, TotalSize: 100}
	if ok, reason := tr.riskGate(exit); !ok {
		t.Errorf("SELL must bypass the mid-band gate, rejected: %s", reason)
	}
	// A consensus order whose post-weight Confidence dipped below the floor still
	// passes when a contributor's RAW conviction clears it (the 2026-07-08 flatline:
	// weight 0.97 × conf 0.80 = 0.776 read as the gate confidence, shaving every
	// single-contributor proposal below its raw number).
	shaved := &StrategyProposal{
		ContractID: "KXFEAR-26JUL02", Action: "BUY", AveragePrice: 0.50,
		Confidence:           0.72,
		ContributorProposals: []ContributorProposal{{Persona: "cautious-bull", Confidence: 0.80}},
		TotalSize:            100,
	}
	if ok, reason := tr.riskGate(shaved); !ok {
		t.Errorf("raw contributor conviction above the floor should pass, rejected: %s", reason)
	}
	// But a weak raw contributor doesn't rescue a weak consensus.
	weakBoth := &StrategyProposal{
		ContractID: "KXFEAR-26JUL02", Action: "BUY", AveragePrice: 0.50,
		Confidence:           0.55,
		ContributorProposals: []ContributorProposal{{Persona: "bull", Confidence: 0.60}},
		TotalSize:            100,
	}
	if ok, _ := tr.riskGate(weakBoth); ok {
		t.Error("expected rejection when both consensus and raw contributor conviction are below the floor")
	}
	// MinConfidence 0 disables the gate.
	tr.SetRiskCaps(RiskCaps{MidBandLow: 0.35, MidBandHigh: 0.65, MidBandMinConfidence: 0})
	if ok, reason := tr.riskGate(weak); !ok {
		t.Errorf("gate disabled at MinConfidence 0, rejected: %s", reason)
	}
}

func TestRiskGate_CategoryLossLimit(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{CategoryLossLimit: 500})
	// Crypto cluster has bled -$520 realized this run across different families.
	tr.categoryRealizedPnL["crypto"] = -520

	blocked := &StrategyProposal{ContractID: "BTC-100K-DEC", Action: "BUY", AveragePrice: 0.30, TotalSize: 100}
	if ok, _ := tr.riskGate(blocked); ok {
		t.Error("expected rejection: crypto category past its loss limit")
	}
	// An uncategorized single-name market is unaffected.
	other := &StrategyProposal{ContractID: "TESLACEOCHANGE-27", Action: "BUY", AveragePrice: 0.30, TotalSize: 100}
	if ok, reason := tr.riskGate(other); !ok {
		t.Errorf("uncategorized market should not share the crypto budget, rejected: %s", reason)
	}
	// Exits from the bleeding category must still pass (position held exactly).
	tr.positions["BTC-100K-DEC"] = &Position{
		ContractID: "BTC-100K-DEC", EntryPrice: 0.30, Size: 100, Timestamp: time.Now(),
	}
	exit := &StrategyProposal{ContractID: "BTC-100K-DEC", Action: "SELL", AveragePrice: 0.20, TotalSize: 100}
	if ok, reason := tr.riskGate(exit); !ok {
		t.Errorf("exit of a loss-limited category should pass, rejected: %s", reason)
	}
}

func TestRiskGate_SellRequiresExactHolding(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	// Held: one strike of the GDP ladder.
	tr.positions["KXGDP-26JUL30-T2.0"] = &Position{
		ContractID: "KXGDP-26JUL30-T2.0", EntryPrice: 0.58, Size: 100, Timestamp: time.Now(),
	}
	// A sibling strike shares the family but is NOT an exit.
	sibling := &StrategyProposal{ContractID: "KXGDP-26JUL30-T0.0", Action: "SELL", AveragePrice: 0.98, TotalSize: 100}
	if ok, _ := tr.riskGate(sibling); ok {
		t.Error("expected rejection: SELL of a sibling strike we don't hold")
	}
	// The exact held contract exits fine.
	exact := &StrategyProposal{ContractID: "KXGDP-26JUL30-T2.0", Action: "SELL", AveragePrice: 0.60, TotalSize: 100}
	if ok, reason := tr.riskGate(exact); !ok {
		t.Errorf("exit of the exact held contract should pass, rejected: %s", reason)
	}
	// Bearish signal-only proposals never execute, even for held contracts.
	bearish := &StrategyProposal{ContractID: "KXGDP-26JUL30-T2.0", Action: "SELL", Bearish: true, AveragePrice: 0.60, TotalSize: 100}
	if ok, _ := tr.riskGate(bearish); ok {
		t.Error("expected rejection: bearish signal-only proposal must not execute")
	}
}

func TestRiskGate_CategoryLossCountsUnrealized(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{CategoryLossLimit: 500})
	// Realized -$300 plus an open ETH position marked -$250 → -$550 category bleed.
	tr.categoryRealizedPnL["crypto"] = -300
	tr.positions["KXETHMINY-27JAN01-1000"] = &Position{
		ContractID: "KXETHMINY-27JAN01-1000", EntryPrice: 0.50, Size: 1000, Timestamp: time.Now(),
	}
	tr.markCache["KXETHMINY-27JAN01-1000"] = markEntry{Price: 0.25, At: time.Now()}

	add := &StrategyProposal{ContractID: "BTC-100K-DEC", Action: "BUY", AveragePrice: 0.30, TotalSize: 100}
	if ok, _ := tr.riskGate(add); ok {
		t.Error("expected rejection: realized + open unrealized crypto loss is past the limit")
	}
}

func TestRecordClose_AccruesCategoryPnL(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	pos := &Position{ContractID: "KXXRPMAXMON-XRP-26JUL31-180", EntryPrice: 0.576, Size: 1000, Timestamp: time.Now()}
	tr.positions[pos.ContractID] = pos

	tr.mu.Lock()
	tr.recordClosedTradeLocked(pos, 0.015, pos.Size, "EXIT", "STOP_LOSS", 0)
	tr.mu.Unlock()

	got := tr.categoryRealizedPnL["crypto"]
	if got > -560 || got < -562 {
		t.Errorf("crypto category should accrue the -$561 realized loss, got $%.2f", got)
	}
	if _, tracked := tr.categoryRealizedPnL[""]; tracked {
		t.Error("uncategorized bucket must never be tracked")
	}
}
