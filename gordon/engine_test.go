package main

import (
	"context"
	"math"
	"testing"
)

func TestLiquidityEngine_Evaluate_ExposureCapping(t *testing.T) {
	// Limits: MaxPosition = 1000, MaxExposure = 5000.0, MaxSlippage = 5.0%
	limits := NewLocalLimitsReader(1000, 5000.0, 0.05)
	obf := NewOrderBookFetcher("")
	obf.allowMock = true // no Redis in tests; evaluate against synthetic books
	engine := NewLiquidityEngine(limits, obf)

	ctx := context.Background()

	// 1. Try size larger than MaxPositionSize (1000)
	res, err := engine.Evaluate(ctx, "TEST-AAPL", "BUY", 1500, 1.0)
	if err != nil {
		t.Fatalf("Unexpected evaluation error: %v", err)
	}

	if res.AdjustedSize > 1000 {
		t.Errorf("Expected size to be capped at 1000, got %d", res.AdjustedSize)
	}
	if !res.LimitBreached {
		t.Error("Expected limit breached flag to be set")
	}

	// 2. Try size exceeding exposure limits (exposure = size * maxPrice)
	// target size 800 at maxPrice 10.0 = $8000 (exceeds $5000)
	// Max allowed should be $5000 / $10 = 500 contracts
	res2, err := engine.Evaluate(ctx, "TEST-AAPL", "BUY", 800, 10.0)
	if err != nil {
		t.Fatalf("Unexpected evaluation error: %v", err)
	}

	if res2.AdjustedSize > 500 {
		t.Errorf("Expected size to be scaled down to 500, got %d", res2.AdjustedSize)
	}
}

func TestLiquidityEngine_Evaluate_SlippageOptimization(t *testing.T) {
	// Setup limits with low allowed slippage: MaxPosition = 2000, MaxExposure = 10000, MaxSlippage = 1.0%
	limits := NewLocalLimitsReader(2000, 10000.0, 0.01)
	obf := NewOrderBookFetcher("")
	obf.allowMock = true // no Redis in tests; evaluate against synthetic books
	engine := NewLiquidityEngine(limits, obf)

	ctx := context.Background()

	// Test heavy volume execution that triggers depth slippage.
	// Mock book asks are generated deterministically based on contract ID length.
	// For "TEST-CONTRACT-ID", base price is derived.
	res, err := engine.Evaluate(ctx, "TEST-CONTRACT-ID", "BUY", 1000, 1.0)
	if err != nil {
		t.Fatalf("Unexpected evaluation error: %v", err)
	}

	if res.SlippagePct > 0.01 {
		t.Errorf("Expected optimized slippage to be <= 1.0%%, got %.4f%%", res.SlippagePct*100)
	}

	if len(res.IcebergChunks) == 0 {
		t.Error("Expected chunks to be generated")
	}

	// Calculate and check chunk summing matches adjusted size
	sum := 0
	for _, chunk := range res.IcebergChunks {
		sum += chunk.Size
	}

	if sum != res.AdjustedSize {
		t.Errorf("Sum of chunks (%d) does not match adjusted size (%d)", sum, res.AdjustedSize)
	}
}

func TestLiquidityEngine_Evaluate_IcebergDelays(t *testing.T) {
	limits := NewLocalLimitsReader(1000, 5000.0, 0.05)
	obf := NewOrderBookFetcher("")
	obf.allowMock = true // no Redis in tests; evaluate against synthetic books
	engine := NewLiquidityEngine(limits, obf)

	ctx := context.Background()

	res, err := engine.Evaluate(ctx, "AAPL-TEST", "BUY", 600, 1.0)
	if err != nil {
		t.Fatalf("Unexpected evaluation error: %v", err)
	}

	if len(res.IcebergChunks) > 1 {
		// Verify that sequential chunks have increasing delayMS values
		prevDelay := -1
		for i, chunk := range res.IcebergChunks {
			if chunk.DelayMS <= prevDelay {
				t.Errorf("Chunk %d delay (%d ms) is not greater than previous (%d ms)", i, chunk.DelayMS, prevDelay)
			}
			prevDelay = chunk.DelayMS
		}
	}
}

func TestPriceLevelMaths(t *testing.T) {
	// Simple sanity test for rounding error safety
	val := 0.5364
	rounded := math.Round(val*1000) / 1000
	if rounded != 0.536 {
		t.Errorf("Expected 0.536, got %.3f", rounded)
	}
}

// With mock fallback disabled (production default), a missing book must surface
// as an error so the proposal is skipped — never a fabricated book.
func TestGetOrderBook_NoMockFallbackByDefault(t *testing.T) {
	obf := NewOrderBookFetcher("") // no Redis, allowMock stays false
	if ob, err := obf.GetOrderBook(context.Background(), "ANY-TICKER"); err == nil || ob != nil {
		t.Errorf("expected error with mock fallback disabled, got ob=%v err=%v", ob, err)
	}
}
