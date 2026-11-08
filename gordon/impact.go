package main

import (
	"context"
	"fmt"
	"math"
)

// CalculateImpact implements a square-root price impact calculation on the order book.
// If the calculated slippage exceeds 1.5% (0.015), it outputs an array of multi-chunk slice boundaries.
func (le *LiquidityEngine) CalculateImpact(ctx context.Context, contractID string, action string, size int) (float64, []int, error) {
	// 1. Fetch order book depth
	ob, err := le.orderBookFetcher.GetOrderBook(ctx, contractID)
	if err != nil {
		return 0.0, nil, fmt.Errorf("failed to fetch order book for impact calculation: %w", err)
	}

	// 2. Determine active side and calculate total depth
	var levels []PriceLevel
	if action == "BUY" {
		levels = ob.Asks
	} else {
		levels = ob.Bids
	}

	if len(levels) == 0 {
		return 0.0, nil, fmt.Errorf("empty order book side for contract %s", contractID)
	}

	totalDepth := 0
	for _, lvl := range levels {
		totalDepth += lvl.Volume
	}

	if totalDepth == 0 {
		return 0.0, nil, fmt.Errorf("zero depth on active side for contract %s", contractID)
	}

	// 3. Model square-root price impact
	// SlippagePct = beta * sqrt(Size / TotalDepth)
	// We default beta to 0.05
	beta := 0.05
	slippagePct := beta * math.Sqrt(float64(size)/float64(totalDepth))

	var sliceBoundaries []int
	const slippageThreshold = 0.015 // 1.5%

	// 4. Output slice boundaries if slippage exceeds 1.5%
	if slippagePct > slippageThreshold {
		// Calculate the max chunk size that keeps slippage under 1.5%
		// MaxChunkSize = TotalDepth * (0.015 / beta)^2
		maxChunkSize := int(float64(totalDepth) * math.Pow(slippageThreshold/beta, 2))
		if maxChunkSize < 10 {
			maxChunkSize = 10 // safe floor to prevent infinite loops or tiny slices
		}

		// Build slice boundaries (e.g. for size 1000 and maxChunk 300: [0, 300, 600, 900, 1000])
		for i := 0; i < size; i += maxChunkSize {
			sliceBoundaries = append(sliceBoundaries, i)
		}
		// Ensure the exact ending size is appended as the last boundary
		if sliceBoundaries[len(sliceBoundaries)-1] != size {
			sliceBoundaries = append(sliceBoundaries, size)
		}
	}

	return slippagePct, sliceBoundaries, nil
}
