package main

import (
	"math"
	"testing"
	"time"
)

func TestPersonaWeights(t *testing.T) {
	stats := []PersonaStat{
		{Persona: "bull", Trades: 50, WinRate: 80},    // strong record, full shrink
		{Persona: "bear", Trades: 50, WinRate: 20},    // poor record
		{Persona: "neutral", Trades: 1, WinRate: 100}, // great but tiny sample → near 1.0
	}
	w := personaWeights(stats)
	if w["bull"] <= 1.0 {
		t.Errorf("strong persona weight should exceed 1.0, got %.3f", w["bull"])
	}
	if w["bear"] >= 1.0 {
		t.Errorf("weak persona weight should be below 1.0, got %.3f", w["bear"])
	}
	if w["neutral"] <= 1.0 || w["neutral"] > 1.2 {
		t.Errorf("tiny-sample persona should stay near 1.0, got %.3f", w["neutral"])
	}
	for name, val := range w {
		if val < 0.5 || val > 1.5 {
			t.Errorf("%s weight %.3f out of clamp [0.5,1.5]", name, val)
		}
	}
}

func TestConsolidate_AgreementSizesUp(t *testing.T) {
	props := []StrategyProposal{
		{ContractID: "X", Action: "BUY", AveragePrice: 0.50, TotalSize: 1000, Persona: "bull", Confidence: 0.9},
		{ContractID: "X", Action: "BUY", AveragePrice: 0.52, TotalSize: 1000, Persona: "neutral", Confidence: 0.9},
		{ContractID: "X", Action: "BUY", AveragePrice: 0.51, TotalSize: 1000, Persona: "cautious-bull", Confidence: 0.9},
	}
	out, _, ok := consolidateProposals(props, map[string]float64{}, 0.4, nil)
	if !ok {
		t.Fatal("expected consolidation to succeed")
	}
	// Three confident personas → conviction ~2.7 → clamped 2.0× → ~2000.
	if out.TotalSize <= 1000 {
		t.Errorf("agreement should size up beyond base 1000, got %d", out.TotalSize)
	}
	if out.Persona != "consensus" {
		t.Errorf("consolidated persona = %q", out.Persona)
	}
	if out.AveragePrice < 0.50 || out.AveragePrice > 0.52 {
		t.Errorf("avg price out of contributors' range: %.3f", out.AveragePrice)
	}
}

func TestVetoFactor(t *testing.T) {
	cases := []struct {
		bull, bear, want float64
	}{
		{1.0, 0, 1.0},   // no bears → full size
		{1.0, 0.5, 0.5}, // bears half the bulls → half size
		{1.0, 1.0, 0},   // bears match bulls → full veto
		{1.0, 2.0, 0},   // bears outweigh → clamped to 0
		{0, 1.0, 1.0},   // no bulls (guard) → unchanged
	}
	for _, c := range cases {
		if got := vetoFactor(c.bull, c.bear); got != c.want {
			t.Errorf("vetoFactor(%.2f,%.2f)=%.3f want %.3f", c.bull, c.bear, got, c.want)
		}
	}
}

func TestSplitByStance(t *testing.T) {
	props := []StrategyProposal{
		{ContractID: "X", Action: "BUY"},
		{ContractID: "Y", Action: "SELL", Bearish: true},
		{ContractID: "Z", Action: "BUY"},
		{ContractID: "W", Action: "SELL", Bearish: true},
	}
	buys, bearish := splitByStance(props)
	if len(buys) != 2 || len(bearish) != 2 {
		t.Fatalf("split: buys=%d bearish=%d, want 2/2", len(buys), len(bearish))
	}
}

func TestConsolidate_LoneWeakSizesDownOrSkips(t *testing.T) {
	props := []StrategyProposal{
		{ContractID: "X", Action: "BUY", AveragePrice: 0.50, TotalSize: 1000, Persona: "bull", Confidence: 0.1},
	}
	// Weight default 1.0 × conf 0.1 = 0.1 conviction < 0.4 floor → skip.
	if _, _, ok := consolidateProposals(props, map[string]float64{}, 0.4, nil); ok {
		t.Error("lone low-conviction proposal should be skipped")
	}
}

func TestConsolidate_WeightedByPerformance(t *testing.T) {
	// Two personas disagree on size; the higher-weighted one pulls the result.
	props := []StrategyProposal{
		{ContractID: "X", Action: "BUY", AveragePrice: 0.50, TotalSize: 2000, Persona: "good", Confidence: 1.0},
		{ContractID: "X", Action: "BUY", AveragePrice: 0.50, TotalSize: 0, Persona: "bad", Confidence: 1.0},
	}
	weights := map[string]float64{"good": 1.5, "bad": 0.5}
	out, _, ok := consolidateProposals(props, weights, 0.4, nil)
	if !ok {
		t.Fatal("expected success")
	}
	// Weighted base size = (1.5*2000 + 0.5*0)/(1.5+0.5) = 1500, ×mult(clamped 2.0) = 3000.
	if out.TotalSize < 2000 {
		t.Errorf("high-weight persona should dominate sizing, got %d", out.TotalSize)
	}
}

func TestConsolidate_Empty(t *testing.T) {
	if _, _, ok := consolidateProposals(nil, map[string]float64{}, 0.4, nil); ok {
		t.Error("empty buffer should not consolidate")
	}
}

func TestEventKey(t *testing.T) {
	if k := eventKey(&StrategyProposal{EventHash: "abc", ContractID: "X"}); k != "evt:abc" {
		t.Errorf("eventKey with hash = %q, want evt:abc", k)
	}
	if k := eventKey(&StrategyProposal{ContractID: "X"}); k != "ct:X" {
		t.Errorf("eventKey without hash should fall back to contract, got %q", k)
	}
}

func TestPickWinningContract(t *testing.T) {
	// Same event, two contracts: A backed by two confident personas, B by one weak.
	props := []StrategyProposal{
		{ContractID: "A", Persona: "bull", Confidence: 0.9, TotalSize: 1000, EventHash: "e1"},
		{ContractID: "A", Persona: "neutral", Confidence: 0.8, TotalSize: 1000, EventHash: "e1"},
		{ContractID: "B", Persona: "bear", Confidence: 0.3, TotalSize: 1000, EventHash: "e1"},
	}
	winner, subset, groups := pickWinningContract(props, map[string]float64{}, nil)
	if winner != "A" {
		t.Errorf("winner = %q, want A (higher aggregate conviction)", winner)
	}
	if len(subset) != 2 {
		t.Errorf("winner subset size = %d, want 2", len(subset))
	}
	if groups != 2 {
		t.Errorf("group count = %d, want 2", groups)
	}
}

func TestConsolidate_CapturesContributorProposals(t *testing.T) {
	props := []StrategyProposal{
		{ContractID: "X", Action: "BUY", AveragePrice: 0.40, TotalSize: 1000, Persona: "bull", Confidence: 0.9},
		{ContractID: "X", Action: "BUY", AveragePrice: 0.60, TotalSize: 500, Persona: "bear", Confidence: 0.9},
	}
	out, _, ok := consolidateProposals(props, map[string]float64{}, 0.4, nil)
	if !ok {
		t.Fatal("expected consolidation to succeed")
	}
	if len(out.ContributorProposals) != 2 {
		t.Fatalf("want 2 contributor proposals, got %d", len(out.ContributorProposals))
	}
	// Each persona's OWN proposed price/size must be retained for close-time scoring.
	byPersona := map[string]ContributorProposal{}
	for _, cp := range out.ContributorProposals {
		byPersona[cp.Persona] = cp
	}
	if byPersona["bull"].EntryPrice != 0.40 || byPersona["bear"].EntryPrice != 0.60 {
		t.Errorf("contributor entry prices not preserved: %+v", byPersona)
	}
}

// The bug this fixes: a consensus order used to credit every contributor with the
// identical consolidated result, so personas showed identical scorecards. Now each
// persona is scored on its OWN proposal vs the actual outcome.
func TestPersonaScoring_Differentiates(t *testing.T) {
	pos := &Position{
		ContractID:   "X",
		EntryPrice:   0.50, // consolidated basis
		Size:         100,
		Persona:      "consensus",
		Contributors: []string{"bull", "bear"},
		ContributorProposals: []ContributorProposal{
			{Persona: "bull", EntryPrice: 0.40, Size: 100}, // cheaper entry
			{Persona: "bear", EntryPrice: 0.80, Size: 100}, // pricier entry
		},
	}
	scores := personaScoresForClose(pos, 1.0, 100) // settles YES ($1)
	got := map[string]PersonaScore{}
	for _, s := range scores {
		got[s.Persona] = s
	}
	if got["bull"].ReturnPct == got["bear"].ReturnPct {
		t.Fatalf("expected differentiated returns, both = %.2f", got["bull"].ReturnPct)
	}
	if got["bull"].RealizedPnL <= got["bear"].RealizedPnL {
		t.Errorf("bull (cheaper entry) should out-earn bear: bull=%.2f bear=%.2f",
			got["bull"].RealizedPnL, got["bear"].RealizedPnL)
	}

	stats := computePersonaStats([]ClosedTrade{{ContractID: "X", PersonaScores: scores}}, 100000, 20)
	if len(stats) != 2 {
		t.Fatalf("want 2 personas, got %d", len(stats))
	}
	if stats[0].RealizedPnL == stats[1].RealizedPnL && stats[0].ReturnOnCapitalPct == stats[1].ReturnOnCapitalPct {
		t.Errorf("persona stats should differ, got identical: %+v / %+v", stats[0], stats[1])
	}
}

// The 2026-07-14 forensics bug: proposals routinely exceed the executed size
// (consensus conviction multiplier, buying-power cap, partial closes leaving
// stored proposals at full size), and scoring on raw proposal sizes inflated
// attributed PnL ~9x. Persona scores must be normalized to the ACTUAL closed
// quantity so per-persona PnL sums to the trade's PnL.
func TestPersonaScoring_NormalizesToClosedSize(t *testing.T) {
	pos := &Position{
		ContractID: "X",
		EntryPrice: 0.50,
		Size:       100, // consensus/cap shrank the fill to half the proposed total
		ContributorProposals: []ContributorProposal{
			{Persona: "bull", EntryPrice: 0.50, Size: 150},
			{Persona: "bear", EntryPrice: 0.50, Size: 50},
		},
	}
	scores := personaScoresForClose(pos, 0.60, 100)
	var sum float64
	for _, s := range scores {
		sum += s.RealizedPnL
	}
	tradePnL := (0.60 - 0.50) * 100 // $10
	if math.Abs(sum-tradePnL) > 0.02 {
		t.Errorf("persona PnL must sum to trade PnL $%.2f, got $%.2f (%+v)", tradePnL, sum, scores)
	}
	// Shares stay proportional to each persona's proposed size (75/25).
	got := map[string]PersonaScore{}
	for _, s := range scores {
		got[s.Persona] = s
	}
	if math.Abs(got["bull"].RealizedPnL-7.5) > 0.02 || math.Abs(got["bear"].RealizedPnL-2.5) > 0.02 {
		t.Errorf("want bull $7.50 / bear $2.50, got bull $%.2f / bear $%.2f",
			got["bull"].RealizedPnL, got["bear"].RealizedPnL)
	}

	// Partial-close sequence: proposals keep their full size after the first exit,
	// but each close must still only attribute the quantity actually closed —
	// 40 then 60, totalling the trade's true $10.
	pos.Size = 100
	first := personaScoresForClose(pos, 0.60, 40)
	pos.Size = 60
	second := personaScoresForClose(pos, 0.60, 60)
	sum = 0
	for _, s := range append(first, second...) {
		sum += s.RealizedPnL
	}
	if math.Abs(sum-tradePnL) > 0.02 {
		t.Errorf("partial closes must attribute $%.2f total, got $%.2f", tradePnL, sum)
	}
}

func TestComputePersonaStats_LegacyFallback(t *testing.T) {
	// Trades without PersonaScores (legacy/replayed) still credit each contributor
	// with the trade-level result.
	stats := computePersonaStats([]ClosedTrade{{
		ContractID: "X", RealizedPnL: -25, ReturnPct: 10,
		Persona: "consensus", Contributors: []string{"bull", "bear"},
	}}, 100000, 20)
	if len(stats) != 2 {
		t.Fatalf("want 2 personas from fallback, got %d", len(stats))
	}
	for _, s := range stats {
		if s.RealizedPnL != -25 || s.Trades != 1 {
			t.Errorf("fallback should credit full outcome to each contributor: %+v", s)
		}
	}
}

func TestConsolidate_DropsBenchedButKeepsUnknownNeutral(t *testing.T) {
	props := []StrategyProposal{
		{ContractID: "C1", Persona: "bull", Confidence: 0.8, TotalSize: 100, AveragePrice: 0.50},
		{ContractID: "C1", Persona: "mystery", Confidence: 0.8, TotalSize: 100, AveragePrice: 0.50},
	}
	// bull present-with-zero (benched); "mystery" missing from the map (neutral).
	weights := map[string]float64{"bull": 0}

	consolidated, summary, ok := consolidateProposals(props, weights, 0.1, nil)
	if !ok {
		t.Fatalf("unknown persona should keep the order alive, got skip (%s)", summary)
	}
	for _, c := range consolidated.Contributors {
		if c == "bull" {
			t.Error("benched persona must not appear as a contributor")
		}
	}
	if len(consolidated.Contributors) != 1 || consolidated.Contributors[0] != "mystery" {
		t.Errorf("expected only the unknown persona to contribute, got %v", consolidated.Contributors)
	}
}

func TestConsolidate_AllBenchedSkips(t *testing.T) {
	props := []StrategyProposal{
		{ContractID: "C1", Persona: "bull", Confidence: 0.9, TotalSize: 100, AveragePrice: 0.50},
		{ContractID: "C1", Persona: "bear", Confidence: 0.9, TotalSize: 100, AveragePrice: 0.50},
	}
	weights := map[string]float64{"bull": 0, "bear": 0}

	// Even with minConviction 0 an all-benched window must skip (and not ÷0).
	if _, summary, ok := consolidateProposals(props, weights, 0, nil); ok {
		t.Errorf("all-benched flush should be skipped, got order (%s)", summary)
	}
}

func TestWeightedConviction_IgnoresBenched(t *testing.T) {
	props := []StrategyProposal{
		{ContractID: "C1", Persona: "bull", Confidence: 1.0},
		{ContractID: "C1", Persona: "bear", Confidence: 1.0},
	}
	all := weightedConviction(props, map[string]float64{}, nil)           // both neutral → 2.0
	half := weightedConviction(props, map[string]float64{"bull": 0}, nil) // bull benched → 1.0
	if all <= half {
		t.Errorf("benching a contributor must lower conviction: all=%.2f benched=%.2f", all, half)
	}
	if half != 1.0 {
		t.Errorf("benched persona should contribute 0, want conviction 1.0, got %.2f", half)
	}
}

func TestSelectCachedBears_MatchAndPrune(t *testing.T) {
	now := time.Now()
	cache := []cachedBear{
		{persona: "bear", contractID: "KXGDP-T0.0", eventHash: "E1", confidence: 0.6, at: now.Add(-1 * time.Minute)},
		{persona: "bear", contractID: "KXGDP-T0.0", eventHash: "E1", confidence: 0.9, at: now.Add(-2 * time.Minute)}, // stronger, same persona+contract
		{persona: "gemini-bear", contractID: "KXOTHER-T1", eventHash: "E2", confidence: 0.8, at: now.Add(-1 * time.Minute)},
		{persona: "bear", contractID: "KXGDP-T0.0", eventHash: "E1", confidence: 0.99, at: now.Add(-30 * time.Minute)}, // expired
	}
	cutoff := now.Add(-10 * time.Minute)

	// BUY on the exact contract KXGDP-T0.0 (event E9, unrelated) should match the two
	// live "bear" entries → one vote at the STRONGEST (0.9); the expired 0.99 is pruned;
	// gemini-bear's KXOTHER doesn't match the contract or event.
	matched, kept := selectCachedBears(cache, "KXGDP-T0.0", "E9", nil, cutoff)
	if len(matched) != 1 {
		t.Fatalf("want 1 matched bear, got %d", len(matched))
	}
	if matched[0].Persona != "bear" || matched[0].Confidence != 0.9 {
		t.Errorf("want strongest bear conf 0.9, got persona=%s conf=%.2f", matched[0].Persona, matched[0].Confidence)
	}
	if len(kept) != 3 {
		t.Errorf("expired entry should be pruned: want 3 kept, got %d", len(kept))
	}
}

func TestSelectCachedBears_EventMatchAndExclude(t *testing.T) {
	now := time.Now()
	cache := []cachedBear{
		{persona: "bear", contractID: "KXA", eventHash: "E1", confidence: 0.7, at: now},
		{persona: "gemini-bear", contractID: "KXB", eventHash: "E1", confidence: 0.5, at: now},
	}
	cutoff := now.Add(-10 * time.Minute)

	// Winner is a DIFFERENT contract but SAME event E1: both cached bears match by
	// event. But "bear" already voted in-window (excluded), so only gemini-bear counts.
	matched, _ := selectCachedBears(cache, "KXWINNER", "E1", map[string]bool{"bear": true}, cutoff)
	if len(matched) != 1 || matched[0].Persona != "gemini-bear" {
		t.Fatalf("want only gemini-bear via event match, got %+v", matched)
	}
}

func TestSelectCachedBears_NoFamilyLeak(t *testing.T) {
	now := time.Now()
	// A bear on one ladder strike must NOT veto a bull on a sibling strike (no shared
	// event): matching is exact-contract or exact-event only.
	cache := []cachedBear{
		{persona: "bear", contractID: "KXBRENTW-T55.99", eventHash: "E1", confidence: 0.9, at: now},
	}
	matched, _ := selectCachedBears(cache, "KXBRENTW-T81.99", "E2", nil, now.Add(-10*time.Minute))
	if len(matched) != 0 {
		t.Errorf("sibling-strike bear must not match a different strike/event, got %d", len(matched))
	}
}
