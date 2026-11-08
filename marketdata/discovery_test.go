package main

import "testing"

func TestCapMarkets(t *testing.T) {
	mk := func(t string, vol float64) CatalogMarket { return CatalogMarket{Ticker: t, Volume: vol} }

	// Pinned (low-volume weather) markets survive; the fill is volume-ranked and cut
	// to the remaining budget.
	keep := []CatalogMarket{mk("KXHIGHNY-A", 0), mk("KXHIGHNY-B", 0)}
	fill := []CatalogMarket{mk("HI", 100), mk("MID", 50), mk("LO", 1)}
	got := capMarkets(keep, fill, 4)
	if len(got) != 4 {
		t.Fatalf("got %d markets, want 4", len(got))
	}
	if got[0].Ticker != "KXHIGHNY-A" || got[1].Ticker != "KXHIGHNY-B" {
		t.Errorf("pinned markets should lead: %+v", got[:2])
	}
	if got[2].Ticker != "HI" || got[3].Ticker != "MID" {
		t.Errorf("fill should be the two highest-volume: %v %v", got[2].Ticker, got[3].Ticker)
	}

	// When the pinned set alone meets/exceeds the budget, keep all pinned and drop fill.
	got = capMarkets(keep, fill, 1)
	if len(got) != 2 || got[0].Ticker != "KXHIGHNY-A" || got[1].Ticker != "KXHIGHNY-B" {
		t.Errorf("pinned set should be kept whole with no fill, got %+v", got)
	}
}

func TestNearMoney(t *testing.T) {
	mk := func(tk, ev string, bid, ask float64) CatalogMarket {
		return CatalogMarket{Ticker: tk, Event: ev, YesBid: bid, YesAsk: ask}
	}
	in := []CatalogMarket{
		mk("A-DEEP", "A", 0.98, 0.99), mk("A-ATM", "A", 0.48, 0.50), mk("A-DEAD", "A", 0, 0),
		mk("A-NEAR", "A", 0.30, 0.32), mk("B-ONLY", "B", 0, 0.01),
	}
	got := nearMoney(in, 2)
	if len(got) != 3 {
		t.Fatalf("got %d markets, want 3 (2 from A, 1 from B): %+v", len(got), got)
	}
	want := map[string]bool{"A-ATM": true, "A-NEAR": true, "B-ONLY": true}
	for _, m := range got {
		if !want[m.Ticker] {
			t.Errorf("unexpected survivor %s", m.Ticker)
		}
	}
	if len(nearMoney(in, 0)) != len(in) {
		t.Error("perEvent 0 must keep everything")
	}
}
