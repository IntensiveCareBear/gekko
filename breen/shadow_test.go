package main

import "testing"

func TestIsShadowed(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})

	// Off by default: nothing is suppressed.
	if tr.isShadowed(&StrategyProposal{Persona: "gemma-bull"}) {
		t.Error("an empty shadow set must suppress nothing")
	}

	tr.shadowPersonas = parsePersonaSet("gemma-bull,gemma-bear,bull")

	cases := []struct {
		name string
		p    *StrategyProposal
		want bool
	}{
		{"shadowed persona", &StrategyProposal{Persona: "gemma-bull"}, true},
		{"live persona", &StrategyProposal{Persona: "weather"}, false},
		{"case insensitive", &StrategyProposal{Persona: "Gemma-Bull"}, true},
		// A bearish signal from a shadowed persona is suppressed too: its only effect
		// is the consensus veto, and a veto from a persona with no measured alpha is
		// noise rather than a brake.
		{"bearish signal is also suppressed", &StrategyProposal{Persona: "gemma-bear", Bearish: true}, true},
		// Mixed authorship is not purely the shadowed persona's idea.
		{"mixed contributors are not suppressed",
			&StrategyProposal{Persona: "consensus", Contributors: []string{"gemma-bull", "weather"}}, false},
		{"all-shadowed contributors are suppressed",
			&StrategyProposal{Persona: "consensus", Contributors: []string{"gemma-bull", "bull"}}, true},
		{"nil proposal", nil, false},
	}
	for _, c := range cases {
		if got := tr.isShadowed(c.p); got != c.want {
			t.Errorf("%s: isShadowed = %v, want %v", c.name, got, c.want)
		}
	}
}

// captureShadowProposal is used as a routing guard, so its return value is the
// contract that matters: true means the caller must not route the proposal onward.
func TestCaptureShadowProposal_GuardsRouting(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.shadowPersonas = parsePersonaSet("gemma-bull")

	shadowed := &StrategyProposal{ContractID: "KXINXY-26DEC31", Action: "BUY",
		AveragePrice: 0.40, TotalSize: 100, Persona: "gemma-bull", Confidence: 0.8}
	live := &StrategyProposal{ContractID: "KXHIGHNY-26AUG25-T77", Action: "BUY",
		AveragePrice: 0.23, TotalSize: 100, Persona: "weather", Confidence: 0.9}

	if !tr.captureShadowProposal(shadowed) {
		t.Error("a shadowed proposal must report true so the caller stops routing it")
	}
	if tr.captureShadowProposal(live) {
		t.Error("a live persona's proposal must report false and continue to routing")
	}
	if got := tr.shadowProposals.Load(); got != 1 {
		t.Errorf("shadowProposals counter = %d, want 1", got)
	}
}

// The suppressed proposal must never reach the book: no position, no cash movement,
// and — critically — it must not be counted as a risk-gate rejection, which is a
// different thing entirely.
func TestCaptureShadowProposal_NeverTouchesTheBook(t *testing.T) {
	tr := newGuardTracker(t, RiskCaps{})
	tr.shadowPersonas = parsePersonaSet("gemma-bull")
	cashBefore := tr.cash

	p := &StrategyProposal{ContractID: "KXINXY-26DEC31", Action: "BUY",
		AveragePrice: 0.40, TotalSize: 100, Persona: "gemma-bull"}
	tr.captureShadowProposal(p)

	if len(tr.positions) != 0 {
		t.Errorf("shadow proposal opened %d position(s)", len(tr.positions))
	}
	if tr.cash != cashBefore {
		t.Errorf("cash moved on a shadow proposal: %.2f → %.2f", cashBefore, tr.cash)
	}
	if got := tr.proposalsRejected.Load(); got != 0 {
		t.Errorf("proposalsRejected = %d — shadowing is not a risk rejection and must not "+
			"pollute that metric", got)
	}
	if len(tr.closedTrades) != 0 {
		t.Error("shadow proposal must not produce a closed trade")
	}
}
