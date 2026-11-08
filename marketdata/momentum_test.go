package main

import (
	"math"
	"testing"
)

func lvls(pairs ...[2]int) []PriceLevel {
	out := make([]PriceLevel, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, PriceLevel{Price: float64(p[0]) / 100.0, Volume: p[1]})
	}
	return out
}

func TestOrderbookImbalance(t *testing.T) {
	// Bid-heavy book (bullish): more depth on the bid side → positive imbalance.
	ob := &OrderBook{
		Bids: lvls([2]int{49, 300}, [2]int{48, 200}),
		Asks: lvls([2]int{50, 50}, [2]int{51, 50}),
	}
	imb, rel := orderbookImbalance(ob)
	if imb <= 0 {
		t.Errorf("bid-heavy book imbalance = %.2f, want > 0", imb)
	}
	// (500-100)/600 ≈ 0.667.
	if math.Abs(imb-0.6667) > 1e-3 {
		t.Errorf("imbalance = %.4f, want ≈0.667", imb)
	}
	if rel <= 0 || rel > 1 {
		t.Errorf("reliability = %.3f, want (0,1]", rel)
	}

	// Ask-heavy book (bearish): negative imbalance.
	ob2 := &OrderBook{
		Bids: lvls([2]int{49, 40}),
		Asks: lvls([2]int{50, 400}),
	}
	if imb2, _ := orderbookImbalance(ob2); imb2 >= 0 {
		t.Errorf("ask-heavy book imbalance = %.2f, want < 0", imb2)
	}

	// One-sided or empty books yield no signal.
	if imb3, rel3 := orderbookImbalance(&OrderBook{Bids: lvls([2]int{49, 10})}); imb3 != 0 || rel3 != 0 {
		t.Errorf("one-sided book = (%.2f,%.2f), want (0,0)", imb3, rel3)
	}
	if imb4, rel4 := orderbookImbalance(nil); imb4 != 0 || rel4 != 0 {
		t.Errorf("nil book = (%.2f,%.2f), want (0,0)", imb4, rel4)
	}

	// Thin book is discounted vs a deep one at the same imbalance.
	thin := &OrderBook{Bids: lvls([2]int{49, 8}), Asks: lvls([2]int{50, 2})}
	deep := &OrderBook{Bids: lvls([2]int{49, 800}), Asks: lvls([2]int{50, 200})}
	_, relThin := orderbookImbalance(thin)
	_, relDeep := orderbookImbalance(deep)
	if !(relThin < relDeep) {
		t.Errorf("thin book reliability %.3f should be < deep %.3f", relThin, relDeep)
	}
}
