package ingest

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/redis/go-redis/v9"
)

// Catalog keys published by the marketdata service (volume-sorted market lists).
const (
	kalshiMarketsKey     = "gekko:kalshi_markets"
	polymarketMarketsKey = "gekko:polymarket_markets"
)

// marketEntry is the subset of a discovered market we need to build Twitter
// searches. Field tags match the marketdata catalog schema.
type marketEntry struct {
	Ticker string  `json:"ticker"`
	Title  string  `json:"title"`
	Volume float64 `json:"volume"`
}

// fetchTopMarkets reads both venue catalogs, merges and re-ranks by volume, then
// returns the top `top` markets after skipping the `skip` most-liquid (the very
// top names tend to be the most efficiently priced — least edge). Returns nil when
// Redis is unavailable or the catalogs are empty (caller falls back to a static query).
func fetchTopMarkets(ctx context.Context, rdb *redis.Client, top, skip int) []marketEntry {
	if rdb == nil {
		return nil
	}
	var all []marketEntry
	for _, key := range []string{kalshiMarketsKey, polymarketMarketsKey} {
		val, err := rdb.Get(ctx, key).Result()
		if err != nil || val == "" {
			continue
		}
		var ms []marketEntry
		if json.Unmarshal([]byte(val), &ms) != nil {
			continue
		}
		all = append(all, ms...)
	}
	if len(all) == 0 {
		return nil
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Volume > all[j].Volume })
	if skip > 0 {
		if skip >= len(all) {
			return nil
		}
		all = all[skip:]
	}
	if top > 0 && top < len(all) {
		all = all[:top]
	}
	return all
}

// titleStopwords are prediction-market title boilerplate that carries no search
// signal (kept lowercase). The subject/entities are what we want to search for.
var titleStopwords = map[string]bool{
	"will": true, "the": true, "a": true, "an": true, "by": true, "in": true,
	"on": true, "of": true, "to": true, "for": true, "at": true, "be": true,
	"is": true, "are": true, "or": true, "and": true, "above": true, "below": true,
	"over": true, "under": true, "reach": true, "hit": true, "before": true,
	"after": true, "end": true, "year": true, "market": true, "price": true,
	"than": true, "this": true, "that": true, "any": true, "more": true,
	"less": true, "much": true, "many": true, "what": true, "who": true,
	"when": true, "which": true, "during": true, "between": true, "close": true,
	"high": true, "low": true, "yes": true, "no": true, "vs": true, "win": true,
	"wins": true, "next": true, "first": true, "last": true, "new": true,
}

// salientTerms extracts up to max search terms from a market title: consecutive
// capitalized words become quoted phrases (e.g. "Federal Reserve"), and standalone
// significant tokens (containing digits, or length ≥ 5) are kept as-is. Stopwords
// and short filler are dropped.
func salientTerms(title string, max int) []string {
	fields := strings.FieldsFunc(title, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '$' || r == '%')
	})
	var out []string
	var phrase []string
	flush := func() {
		if len(phrase) > 0 {
			out = append(out, strings.Join(phrase, " "))
			phrase = nil
		}
	}
	for _, tok := range fields {
		low := strings.ToLower(strings.Trim(tok, "$%"))
		if low == "" || titleStopwords[low] {
			flush()
			continue
		}
		runes := []rune(tok)
		isCap := unicode.IsUpper(runes[0])
		hasDigit := strings.IndexFunc(tok, unicode.IsDigit) >= 0
		if isCap && !hasDigit {
			phrase = append(phrase, tok) // accumulate proper-noun phrase
			continue
		}
		flush()
		if hasDigit || len(low) >= 5 {
			out = append(out, tok)
		}
	}
	flush()
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// buildMarketQuery assembles a single X recent-search query from the popular
// markets: each market contributes up to perMarketTerms salient terms, OR'd
// together within maxLen (reserving room for the filter suffix). Falls back to
// `fallback` when there are no markets or no usable terms.
func buildMarketQuery(markets []marketEntry, fallback string, maxLen, perMarketTerms int) string {
	const suffix = ") -is:retweet -is:reply lang:en"
	const prefix = "("

	seen := map[string]bool{}
	q := prefix
	added := 0
	for _, m := range markets {
		for _, term := range salientTerms(m.Title, perMarketTerms) {
			key := strings.ToLower(term)
			if seen[key] {
				continue
			}
			formatted := term
			if strings.Contains(term, " ") {
				formatted = `"` + term + `"` // exact-phrase match for multi-word entities
			}
			piece := formatted
			if added > 0 {
				piece = " OR " + formatted
			}
			if len(q)+len(piece)+len(suffix) > maxLen {
				goto done
			}
			seen[key] = true
			q += piece
			added++
		}
	}
done:
	if added == 0 {
		return fallback
	}
	return q + suffix
}
