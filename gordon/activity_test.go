package main

import (
	"strings"
	"testing"
	"time"
)

func TestActivityGate_FamilyCooldown(t *testing.T) {
	g := newActivityGate(10 * time.Minute)
	now := time.Now()
	ev := activityEvent{Ticker: "KXBTCD-26OCT1517-T81749.99", Bid: 0.40, Ask: 0.44}
	if ok, why := g.admit(ev, now); !ok {
		t.Fatalf("first event must pass: %s", why)
	}
	sibling := ev
	sibling.Ticker = "KXBTCD-26OCT1517-T82499.99" // same family, different strike
	if ok, _ := g.admit(sibling, now.Add(2*time.Minute)); ok {
		t.Error("a sibling strike inside the cooldown must be skipped")
	}
	other := ev
	other.Ticker = "KXETHD-26OCT1517-T2459.99"
	if ok, _ := g.admit(other, now.Add(2*time.Minute)); !ok {
		t.Error("a different family is independent")
	}
	if ok, _ := g.admit(sibling, now.Add(11*time.Minute)); !ok {
		t.Error("after the cooldown the family may be evaluated again")
	}
}

func TestActivityGate_RejectsMalformed(t *testing.T) {
	g := newActivityGate(time.Minute)
	for name, ev := range map[string]activityEvent{
		"no ticker": {Bid: 0.4, Ask: 0.5},
		"one-sided": {Ticker: "KXA-26OCT-T1", Bid: 0, Ask: 0.5},
		"crossed":   {Ticker: "KXA-26OCT-T1", Bid: 0.6, Ask: 0.5},
	} {
		if ok, _ := g.admit(ev, time.Now()); ok {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestActivityContent(t *testing.T) {
	trend := activityContent(" Will BTC close above $81,749? ", activityEvent{Kind: "trend", TrendPct: -12.3, Mid: 0.31, Bid: 0.30, Ask: 0.32})
	for _, want := range []string{"Will BTC close above $81,749?", "-12.3%", "0.31", "bid 0.30 / ask 0.32"} {
		if !strings.Contains(trend, want) {
			t.Errorf("trend content missing %q: %s", want, trend)
		}
	}
	surge := activityContent("X", activityEvent{Kind: "vol_surge", VolSurge: 4.2, Mid: 0.5, Bid: 0.49, Ask: 0.51})
	if !strings.Contains(surge, "surged 4.2x") {
		t.Errorf("surge content wrong: %s", surge)
	}
}

func TestMatcherLookup(t *testing.T) {
	var nilMatcher *MarketMatcher
	if _, ok := nilMatcher.Lookup("X"); ok {
		t.Error("nil matcher must report not found")
	}
	m := &MarketMatcher{titles: map[string]string{"KXA-26OCT-T1": "A title"}}
	if title, ok := m.Lookup("KXA-26OCT-T1"); !ok || title != "A title" {
		t.Errorf("got %q %v", title, ok)
	}
	if _, ok := m.Lookup("nope"); ok {
		t.Error("unknown ticker must not be found")
	}
}

func TestActivityHashStable(t *testing.T) {
	ev := activityEvent{Ticker: "KXA-26OCT-T1", Kind: "trend", TsMs: 123}
	if activityHash(ev) != activityHash(ev) || activityHash(ev) == activityHash(activityEvent{Ticker: "KXB-26OCT-T1", Kind: "trend", TsMs: 123}) {
		t.Error("hash must be deterministic and ticker-specific")
	}
}
