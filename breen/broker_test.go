package main

import (
	"os"
	"testing"
	"time"
)

func TestExecutiveDryModeBypassesBroker(t *testing.T) {
	// Setup temporary files for testing ledger
	tmpLedgerFile := "./test_ledger.db"
	defer os.Remove(tmpLedgerFile)

	// Initialize ledger
	ledger := NewImmutableLedger(tmpLedgerFile)

	// Create position tracker in DRY mode = true.
	// We intentionally pass a non-existent socket path to the broker client.
	// If the execution path tries to connect to the broker-worker, it will fail/panic/error.
	nonExistentSocket := "./non_existent_broker.sock"
	brokerClient := NewBrokerClient(nonExistentSocket, "secret-token")
	
	// Create position tracker
	tracker := NewPositionTracker("", ledger, brokerClient, true, 100000, 2*time.Hour, 48*time.Hour) // dryMode = true

	// Verify initial state
	if len(tracker.positions) != 0 {
		t.Fatalf("Expected 0 initial positions, got %d", len(tracker.positions))
	}

	proposal := &StrategyProposal{
		ContractID:   "AAPL-UP-200",
		Action:       "BUY",
		AveragePrice: 0.76,
		TotalSize:    500,
		Timestamp:    time.Now(),
	}

	// Trigger order execution
	tracker.ExecuteOrder(proposal)

	// If dry mode worked correctly:
	// 1. It bypassed Unix socket dialing completely (no errors thrown).
	// 2. It successfully wrote the simulated transaction to the local ledger.
	// 3. It created the position.
	tracker.mu.RLock()
	pos, ok := tracker.positions["AAPL-UP-200"]
	tracker.mu.RUnlock()

	if !ok {
		t.Fatal("Expected AAPL-UP-200 position to be opened, but it wasn't")
	}
	if pos.Size != 500 {
		t.Errorf("Expected position size 500, got %d", pos.Size)
	}

	// Read ledger blocks and verify Dry Mode records
	blocks, err := ledger.GetBlocks()
	if err != nil {
		t.Fatalf("Failed to retrieve ledger blocks: %v", err)
	}

	if len(blocks) < 2 {
		t.Fatalf("Expected at least 2 ledger blocks (DRY_OPEN and DRY_FILLED), got %d", len(blocks))
	}

	if blocks[0].Status != "DRY_OPEN" {
		t.Errorf("Expected first block status to be DRY_OPEN, got %s", blocks[0].Status)
	}
	if blocks[1].Status != "DRY_FILLED" {
		t.Errorf("Expected second block status to be DRY_FILLED, got %s", blocks[1].Status)
	}

	// Verify cryptographic back-linking integrity
	valid, err := ledger.VerifyLedger()
	if err != nil {
		t.Fatalf("Ledger cryptographic verification error: %v", err)
	}
	if !valid {
		t.Error("Ledger cryptographic check failed! Chain is broken.")
	}
}
