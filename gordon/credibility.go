package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// Source credibility (docs/data-sourcing-and-credibility-proposal.md, phase 1): an
// anonymous tweet and a wire story with the same embedding match used to size
// identically. A static per-source tier gives a base credibility, and an optional
// hand-edited file overrides it per account/outlet. 0 means "unknown" (market-feed
// and synthetic sources) and leaves confidence/size untouched.

// tierCredibility maps source tier 1 (primary/official) … 4 (anonymous social) to [0,1].
var tierCredibility = map[int]float64{1: 0.95, 2: 0.75, 3: 0.55, 4: 0.30}

var sourceTier = map[string]int{"FRED": 1, "REUTERS": 2, "FINNHUB": 2, "TWITTER": 4, "REDDIT": 4}

// reputationFile is {"scores": {"<substring of SourceURL>": 0.9}} — e.g. a twitter
// handle or news domain. First match wins; scores are clamped to [0.01,1].
var reputation = &repBook{path: getEnv("SOURCE_REPUTATION_FILE", "config/source_reputation.json")}

type repBook struct {
	mu     sync.Mutex
	path   string
	mtime  time.Time
	scores map[string]float64
}

func (r *repBook) lookup(url string) (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, err := os.Stat(r.path); err != nil {
		r.scores, r.mtime = nil, time.Time{}
	} else if !st.ModTime().Equal(r.mtime) {
		var f struct {
			Scores map[string]float64 `json:"scores"`
		}
		if raw, err := os.ReadFile(r.path); err == nil && json.Unmarshal(raw, &f) == nil {
			r.scores, r.mtime = f.Scores, st.ModTime()
		}
	}
	url = strings.ToLower(url)
	for k, v := range r.scores {
		if k != "" && strings.Contains(url, strings.ToLower(k)) {
			return clamp(v, 0.01, 1), true
		}
	}
	return 0, false
}

// sourceCredibility returns [0,1], or 0 when the source is unranked.
func sourceCredibility(sourceID, sourceURL string) float64 {
	if v, ok := reputation.lookup(sourceURL); ok {
		return v
	}
	return tierCredibility[sourceTier[strings.ToUpper(sourceID)]]
}
