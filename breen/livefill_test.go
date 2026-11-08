package main

import "testing"

func TestLiveFill(t *testing.T) {
	p := &StrategyProposal{TotalSize: 100, AveragePrice: 0.30}
	cases := []struct {
		name      string
		res       *TradeResult
		wantSize  int
		wantPrice float64
	}{
		{"no report keeps the proposal", &TradeResult{Success: true}, 100, 0.30},
		{"nil result keeps the proposal", nil, 100, 0.30},
		{"partial fill books only what filled", &TradeResult{Success: true, FilledSize: 40, FilledPrice: 0.29}, 40, 0.29},
		{"full fill at a better price", &TradeResult{Success: true, FilledSize: 100, FilledPrice: 0.27}, 100, 0.27},
		{"a fill larger than the ask is clamped", &TradeResult{Success: true, FilledSize: 500}, 100, 0.30},
	}
	for _, c := range cases {
		size, price := liveFill(p, c.res)
		if size != c.wantSize || price != c.wantPrice {
			t.Errorf("%s: got %d @ %.2f, want %d @ %.2f", c.name, size, price, c.wantSize, c.wantPrice)
		}
	}
}
