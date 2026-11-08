package main

import (
	"encoding/json"
	"testing"
)

// sampleGammaMarkets mirrors the real Gamma /markets shape: the array fields
// (clobTokenIds, outcomePrices) are returned as JSON-ENCODED STRINGS.
const sampleGammaMarkets = `[
  {
    "id": "558943",
    "question": "Will USA win the 2026 FIFA World Cup?",
    "conditionId": "0xcdb1f04",
    "clobTokenIds": "[\"94603648636330087039501304492699481091005420017442244191603206509188088089447\", \"45270201343463663182019040935560267543606888663369415494551943549463253748361\"]",
    "outcomePrices": "[\"0.0325\", \"0.9675\"]",
    "volumeNum": 88268620.02,
    "liquidity": "3145963.67",
    "active": true,
    "closed": false,
    "enableOrderBook": true,
    "endDate": "2026-07-20T00:00:00Z"
  },
  {
    "id": "999",
    "question": "Closed market should be skipped",
    "clobTokenIds": "[\"111\",\"222\"]",
    "outcomePrices": "[\"0.5\",\"0.5\"]",
    "active": true,
    "closed": true,
    "enableOrderBook": true
  },
  {
    "id": "888",
    "question": "Order-book disabled should be skipped",
    "clobTokenIds": "[\"333\",\"444\"]",
    "outcomePrices": "[\"0.5\",\"0.5\"]",
    "active": true,
    "closed": false,
    "enableOrderBook": false
  }
]`

func TestParseYesToken(t *testing.T) {
	yes, no := parseYesToken(`["94603648636330087039501304492699481091005420017442244191603206509188088089447", "45270201343463663182019040935560267543606888663369415494551943549463253748361"]`)
	if yes != "94603648636330087039501304492699481091005420017442244191603206509188088089447" {
		t.Errorf("yes token = %q", yes)
	}
	if no != "45270201343463663182019040935560267543606888663369415494551943549463253748361" {
		t.Errorf("no token = %q", no)
	}
	if y, _ := parseYesToken("not json"); y != "" {
		t.Errorf("expected empty yes on bad input, got %q", y)
	}
}

func TestParseFirstFloat(t *testing.T) {
	if got := parseFirstFloat(`["0.0325", "0.9675"]`); got != 0.0325 {
		t.Errorf("parseFirstFloat = %v, want 0.0325", got)
	}
	if got := parseFirstFloat("garbage"); got != 0 {
		t.Errorf("parseFirstFloat(garbage) = %v, want 0", got)
	}
}

func TestPolymarketDecodeGamma(t *testing.T) {
	// Decode the same way ListOpenMarkets does, then apply the filter/map logic.
	var page []gammaMarket
	if err := json.Unmarshal([]byte(sampleGammaMarkets), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("expected 3 raw markets, got %d", len(page))
	}

	var kept []CatalogMarket
	for _, m := range page {
		if !m.Active || m.Closed || !m.EnableOrderBook {
			continue
		}
		yesTok, _ := parseYesToken(m.ClobTokenIDs)
		if yesTok == "" {
			continue
		}
		kept = append(kept, CatalogMarket{
			Ticker: polyPrefix + yesTok,
			Title:  m.Question,
			YesBid: parseFirstFloat(m.OutcomePrices),
			YesAsk: parseFirstFloat(m.OutcomePrices),
			Volume: m.VolumeNum,
			Status: "open",
			Event:  m.ConditionID,
		})
	}

	if len(kept) != 1 {
		t.Fatalf("expected 1 kept market (closed + book-disabled skipped), got %d", len(kept))
	}
	m := kept[0]
	wantTicker := "POLY-94603648636330087039501304492699481091005420017442244191603206509188088089447"
	if m.Ticker != wantTicker {
		t.Errorf("ticker = %q, want %q", m.Ticker, wantTicker)
	}
	if m.YesBid != 0.0325 {
		t.Errorf("YesBid = %v, want 0.0325", m.YesBid)
	}
	if m.Volume != 88268620.02 {
		t.Errorf("Volume = %v", m.Volume)
	}
}

func TestParsePolyBook(t *testing.T) {
	// Note unsorted input: bids ascending, asks descending — parsePolyBook must
	// resort to bids-desc / asks-asc.
	raw := `{
		"bids": [
			{"price": "0.001", "size": "2960445.78"},
			{"price": "0.03", "size": "100"}
		],
		"asks": [
			{"price": "0.999", "size": "20490824.67"},
			{"price": "0.04", "size": "200"}
		]
	}`
	ob, err := parsePolyBook([]byte(raw), "POLY-123")
	if err != nil {
		t.Fatalf("parsePolyBook: %v", err)
	}
	if ob.ContractID != "POLY-123" {
		t.Errorf("ContractID = %q", ob.ContractID)
	}
	if len(ob.Bids) != 2 || len(ob.Asks) != 2 {
		t.Fatalf("levels: bids=%d asks=%d", len(ob.Bids), len(ob.Asks))
	}
	if ob.Bids[0].Price != 0.03 {
		t.Errorf("best bid = %v, want 0.03 (highest)", ob.Bids[0].Price)
	}
	if ob.Asks[0].Price != 0.04 {
		t.Errorf("best ask = %v, want 0.04 (lowest)", ob.Asks[0].Price)
	}
	if ob.Bids[0].Volume != 100 {
		t.Errorf("best bid volume = %d, want 100", ob.Bids[0].Volume)
	}
}
