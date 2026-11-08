package ingest

import (
	"strings"
	"testing"
)

func TestSalientTerms(t *testing.T) {
	cases := []struct {
		title string
		want  []string // subset that must appear (order-independent)
	}{
		{"Will the Fed cut interest rates in July 2026?", []string{"Fed"}},
		{"Will the Supreme Court rule on the case?", []string{"Supreme Court"}}, // consecutive caps → phrase
		{"Bitcoin above $100,000 by year end?", []string{"Bitcoin"}},
	}
	for _, c := range cases {
		got := salientTerms(c.title, 4)
		joined := strings.Join(got, "|")
		for _, w := range c.want {
			if !strings.Contains(joined, w) {
				t.Errorf("salientTerms(%q) = %v, missing %q", c.title, got, w)
			}
		}
		// Stopwords must never leak through.
		for _, term := range got {
			if titleStopwords[strings.ToLower(term)] {
				t.Errorf("salientTerms(%q) leaked stopword %q", c.title, term)
			}
		}
	}
}

func TestBuildMarketQuery(t *testing.T) {
	markets := []marketEntry{
		{Ticker: "KXFED", Title: "Will the Fed cut rates in July?", Volume: 100},
		{Ticker: "KXBTC", Title: "Bitcoin above $100,000 this year?", Volume: 90},
		{Ticker: "KXSCOTUS", Title: "Supreme Court ruling before recess?", Volume: 80},
	}
	q := buildMarketQuery(markets, "FALLBACK", 480, 2)
	if !strings.HasPrefix(q, "(") || !strings.Contains(q, ") -is:retweet -is:reply lang:en") {
		t.Fatalf("query not well-formed: %q", q)
	}
	if !strings.Contains(q, "Fed") || !strings.Contains(q, "Bitcoin") {
		t.Errorf("query missing expected market terms: %q", q)
	}
	if !strings.Contains(q, `"Supreme Court"`) {
		t.Errorf("multi-word entity should be quoted: %q", q)
	}
	if len(q) > 480 {
		t.Errorf("query exceeds max length: %d", len(q))
	}
}

func TestBuildMarketQuery_FallsBack(t *testing.T) {
	if got := buildMarketQuery(nil, "FALLBACK", 480, 2); got != "FALLBACK" {
		t.Errorf("empty catalog should fall back, got %q", got)
	}
}
