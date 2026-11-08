package main

import (
	"os"
	"testing"
	"time"
)

func newGateTracker(t *testing.T, caps RiskCaps) *PositionTracker {
	t.Helper()
	tmp := "./test_riskgate_ledger.db"
	t.Cleanup(func() { os.Remove(tmp) })
	ledger := NewImmutableLedger(tmp)
	tracker := NewPositionTracker("", ledger, NewBrokerClient("./none.sock", "t"), true, 100000, time.Hour, 2*time.Hour)
	tracker.SetRiskCaps(caps)
	return tracker
}

func TestRiskGate_UnderCapAllowed(t *testing.T) {
	tr := newGateTracker(t, RiskCaps{MaxContractExposure: 15000, MaxTotalDeployed: 60000, MaxOpenPositions: 25})
	p := &StrategyProposal{ContractID: "FED-RATE", Action: "BUY", AveragePrice: 0.50, TotalSize: 1000} // $500
	if ok, reason := tr.riskGate(p); !ok {
		t.Errorf("expected allowed, rejected: %s", reason)
	}
}

func TestRiskGate_ContractExposureCap(t *testing.T) {
	tr := newGateTracker(t, RiskCaps{MaxContractExposure: 5000})
	// Existing position: 8000 @ 0.50 = $4000 notional.
	tr.positions["FED-RATE"] = &Position{ContractID: "FED-RATE", EntryPrice: 0.50, Size: 8000, Timestamp: time.Now()}
	// New order $1500 → total $5500 > $5000 cap.
	p := &StrategyProposal{ContractID: "FED-RATE", Action: "BUY", AveragePrice: 0.50, TotalSize: 3000}
	if ok, _ := tr.riskGate(p); ok {
		t.Error("expected rejection on contract exposure cap")
	}
}

func TestRiskGate_SellAlwaysAllowed(t *testing.T) {
	tr := newGateTracker(t, RiskCaps{MaxContractExposure: 1, MaxTotalDeployed: 1, MaxOpenPositions: 0})
	tr.positions["FED-RATE"] = &Position{ContractID: "FED-RATE", EntryPrice: 0.50, Size: 9999, Timestamp: time.Now()}
	p := &StrategyProposal{ContractID: "FED-RATE", Action: "SELL", AveragePrice: 0.99, TotalSize: 9999}
	if ok, reason := tr.riskGate(p); !ok {
		t.Errorf("SELL (exit) should always pass, rejected: %s", reason)
	}
}

func TestRiskGate_MaxOpenPositions(t *testing.T) {
	tr := newGateTracker(t, RiskCaps{MaxOpenPositions: 1})
	tr.positions["A"] = &Position{ContractID: "A", EntryPrice: 0.5, Size: 10, Timestamp: time.Now()}
	// New, distinct contract while already at 1 open → reject.
	pNew := &StrategyProposal{ContractID: "B", Action: "BUY", AveragePrice: 0.5, TotalSize: 10}
	if ok, _ := tr.riskGate(pNew); ok {
		t.Error("expected rejection: opening a new contract at max open positions")
	}
	// Adding to the EXISTING contract is still allowed (not a new position).
	pAdd := &StrategyProposal{ContractID: "A", Action: "BUY", AveragePrice: 0.5, TotalSize: 10}
	if ok, reason := tr.riskGate(pAdd); !ok {
		t.Errorf("adding to existing position should pass, rejected: %s", reason)
	}
}

func TestRiskGate_TotalDeployedCap(t *testing.T) {
	tr := newGateTracker(t, RiskCaps{MaxTotalDeployed: 10000})
	// Open notional: 16000 @ 0.50 = $8000 deployed.
	tr.positions["HELD"] = &Position{ContractID: "HELD", EntryPrice: 0.50, Size: 16000, Timestamp: time.Now()}
	p := &StrategyProposal{ContractID: "X", Action: "BUY", AveragePrice: 0.50, TotalSize: 6000} // $3000 → $11000 > cap
	if ok, _ := tr.riskGate(p); ok {
		t.Error("expected rejection on total deployed cap")
	}
}

func TestRiskGate_DeployedIgnoresRealizedLosses(t *testing.T) {
	// Regression: deployed used to be startingCapital − cash, so realized losses
	// counted as "deployed" forever — a losing run throttled entries even when flat.
	tr := newGateTracker(t, RiskCaps{MaxTotalDeployed: 10000})
	tr.cash = 80000 // −$20k realized, but NO open positions ⇒ deployed = $0
	p := &StrategyProposal{ContractID: "X", Action: "BUY", AveragePrice: 0.50, TotalSize: 6000} // $3000 < cap
	if ok, reason := tr.riskGate(p); !ok {
		t.Errorf("flat book must have $0 deployed regardless of realized losses, rejected: %s", reason)
	}
}

func TestRiskGate_RejectOnBreach(t *testing.T) {
	tr := newGateTracker(t, RiskCaps{RejectOnBreach: true})
	p := &StrategyProposal{ContractID: "X", Action: "BUY", AveragePrice: 0.5, TotalSize: 10, LimitBreached: true}
	if ok, _ := tr.riskGate(p); ok {
		t.Error("expected rejection when RejectOnBreach set and LimitBreached true")
	}
}

func TestRiskGate_DisabledByDefault(t *testing.T) {
	tr := newGateTracker(t, RiskCaps{}) // all zero = no limits
	tr.positions["X"] = &Position{ContractID: "X", EntryPrice: 0.99, Size: 1_000_000, Timestamp: time.Now()}
	p := &StrategyProposal{ContractID: "X", Action: "BUY", AveragePrice: 0.99, TotalSize: 1_000_000}
	if ok, reason := tr.riskGate(p); !ok {
		t.Errorf("zero caps should disable the gate, rejected: %s", reason)
	}
}
