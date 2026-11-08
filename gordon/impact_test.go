package main

import (
	"context"
	"testing"
)

func TestCalculateImpact_Basic(t *testing.T) {
	// Setup limits and mock book fetcher
	limits := NewLocalLimitsReader(2000, 10000.0, 0.05)
	obf := NewOrderBookFetcher("")
	obf.allowMock = true // no Redis in tests; evaluate against synthetic books
	engine := NewLiquidityEngine(limits, obf)

	ctx := context.Background()

	slip1, boundaries1, err := engine.CalculateImpact(ctx, "AAPL-TEST", "BUY", 100)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if slip1 > 0.015 {
		t.Errorf("Expected small slippage for small trade, got: %.4f", slip1)
	}

	if len(boundaries1) != 0 {
		t.Errorf("Expected empty boundaries for small trade size, got: %v", boundaries1)
	}

	// 2. Test large size: Should trigger slicing bounds (slippage exceeds 1.5%)
	// Mock book total depth is typically 5 asks with volumes like 250, 210, 170... total is ~1000.
	slip2, boundaries2, err := engine.CalculateImpact(ctx, "AAPL-TEST", "BUY", 1200)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if slip2 <= 0.015 {
		t.Errorf("Expected slippage to exceed 1.5%% for large trade size, got: %.4f", slip2)
	}

	if len(boundaries2) < 2 {
		t.Errorf("Expected at least [0, size] boundaries for large trade, got: %v", boundaries2)
	}

	if boundaries2[0] != 0 {
		t.Errorf("Expected first boundary to be 0, got %d", boundaries2[0])
	}

	if boundaries2[len(boundaries2)-1] != 1200 {
		t.Errorf("Expected last boundary to be 1200, got %d", boundaries2[len(boundaries2)-1])
	}
}
