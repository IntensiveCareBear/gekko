package ingest

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// On-demand ingest (docs/gordons-creed-proposal.md, Phase 3). Gordon asks "what is the news on this
// market?" over Redis; kahuna fetches targeted headlines and replies on a Redis list that gordon
// blocks on. Deliberately NOT put on the Kafka ingest topic (the doc's first sketch): every item
// there would become its own LLM evaluation, which is the cost this feature exists to avoid.
//
// Network scope is fixed to Google News RSS (the host the standing feeds already use and the
// compliance gatekeeper authorizes); a request can only supply keywords, never a URL.

const (
	OnDemandRequestChannel = "gekko:ingest_requests"
	OnDemandReplyPrefix    = "gekko:ingest_reply:" // + request id; a Redis list holding one JSON reply
	googleNewsBase         = "https://news.google.com/rss/search"
)

type IngestRequest struct {
	ID         string `json:"id"`
	Ticker     string `json:"ticker"`
	Title      string `json:"title"`
	DeadlineMs int64  `json:"deadline_ms"` // unix ms after which nobody is waiting
	MaxItems   int    `json:"max_items"`
}

type NewsItem struct {
	Title   string `json:"title"`
	Link    string `json:"link"`
	PubDate string `json:"pub_date"`
}

type IngestReply struct {
	ID    string     `json:"id"`
	Query string     `json:"query"`
	Items []NewsItem `json:"items"`
	Error string     `json:"error,omitempty"`
}

var (
	reTitleTail = regexp.MustCompile(`\s+[—–-]\s+.*$`) // marketdata appends " — <yes_sub_title>"
	reNonWord   = regexp.MustCompile(`[^a-zA-Z0-9$.,%' -]+`)
	reHasDigit  = regexp.MustCompile(`\d`)
)

var queryStopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`will would the a an be is are was of in on at by to for from or and than then
		above below over under more less before after during exceed exceeds exceeding least most between
		end ends close closes closed open opens week month day year weekly daily monthly yes no
		jan feb mar apr may jun jul aug sep sept oct nov dec january february march april june july august
		september october november december est edt pst pdt et pt utc am pm this that with into above-or below-or
		maximum minimum high low`) {
		m[w] = true
	}
	return m
}()

// buildNewsQuery turns a market title into search keywords: drop the yes-subtitle, numbers, dates and
// generic words, keep at most 7 distinctive terms. Pure.
func buildNewsQuery(title string) string {
	title = reTitleTail.ReplaceAllString(strings.TrimSpace(title), "")
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.Fields(reNonWord.ReplaceAllString(title, " ")) {
		tok = strings.Trim(tok, ".,%'-$")
		low := strings.ToLower(tok)
		if len(low) < 3 || reHasDigit.MatchString(low) || queryStopwords[low] || seen[low] {
			continue
		}
		seen[low] = true
		out = append(out, tok)
		if len(out) == 7 {
			break
		}
	}
	return strings.Join(out, " ")
}

var reHTML = regexp.MustCompile(`<[^>]*>`)

func cleanHeadline(s string) string {
	s = html.UnescapeString(reHTML.ReplaceAllString(s, ""))
	s = strings.Map(func(r rune) rune {
		if r < 32 {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// fetchGoogleNews returns up to max recent headlines for the keywords. base is the search endpoint.
func fetchGoogleNews(ctx context.Context, client *http.Client, base, query string, max int) ([]NewsItem, error) {
	u := base + "?q=" + url.QueryEscape(query+" when:2d") + "&hl=en-US&gl=US&ceid=US:en"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "GekkoNewsBot/1.0 (+rss)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("news search returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var feed RssFeed
	if err := xml.Unmarshal(b, &feed); err != nil {
		return nil, fmt.Errorf("news search XML: %w", err)
	}
	var items []NewsItem
	for _, it := range feed.Channel.Items {
		t := cleanHeadline(it.Title)
		if t == "" {
			continue
		}
		items = append(items, NewsItem{Title: t, Link: it.Link, PubDate: it.PubDate})
		if len(items) >= max {
			break
		}
	}
	return items, nil
}

// OnDemandIngest serves gordon's news requests.
type OnDemandIngest struct {
	rdb    *redis.Client
	client *http.Client
	base   string

	perMinute int
	mu        sync.Mutex
	recent    []time.Time // request times, for the per-minute budget
	cache     map[string]cachedNews
	sem       chan struct{}
}

type cachedNews struct {
	at    time.Time
	items []NewsItem
}

func NewOnDemandIngest(rdb *redis.Client, perMinute int) *OnDemandIngest {
	if perMinute <= 0 {
		perMinute = 6
	}
	return &OnDemandIngest{rdb: rdb, client: &http.Client{Timeout: 10 * time.Second}, base: googleNewsBase,
		perMinute: perMinute, cache: map[string]cachedNews{}, sem: make(chan struct{}, 2)}
}

// allow enforces the per-minute request budget against the news host.
func (o *OnDemandIngest) allow(now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	cut := now.Add(-time.Minute)
	kept := o.recent[:0]
	for _, t := range o.recent {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	o.recent = kept
	if len(o.recent) >= o.perMinute {
		return false
	}
	o.recent = append(o.recent, now)
	return true
}

// handle answers one request. Pure of Redis so it can be tested; Start does the plumbing.
func (o *OnDemandIngest) handle(ctx context.Context, req IngestRequest, now time.Time) IngestReply {
	rep := IngestReply{ID: req.ID}
	if req.ID == "" || len(req.ID) > 80 {
		rep.Error = "bad request id"
		return rep
	}
	if req.DeadlineMs > 0 && now.UnixMilli() > req.DeadlineMs {
		rep.Error = "deadline passed"
		return rep
	}
	rep.Query = buildNewsQuery(req.Title)
	if rep.Query == "" {
		rep.Error = "no usable keywords in title"
		return rep
	}
	max := req.MaxItems
	if max <= 0 || max > 8 {
		max = 5
	}
	o.mu.Lock()
	c, ok := o.cache[rep.Query]
	o.mu.Unlock()
	if ok && now.Sub(c.at) < 10*time.Minute {
		rep.Items = c.items
		return rep
	}
	if !o.allow(now) {
		rep.Error = "rate limited"
		return rep
	}
	items, err := fetchGoogleNews(ctx, o.client, o.base, rep.Query, max)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	o.mu.Lock()
	o.cache[rep.Query] = cachedNews{at: now, items: items}
	if len(o.cache) > 200 {
		for k, v := range o.cache {
			if now.Sub(v.at) > 10*time.Minute {
				delete(o.cache, k)
			}
		}
	}
	o.mu.Unlock()
	rep.Items = items
	return rep
}

// Start subscribes to the request channel until ctx ends.
func (o *OnDemandIngest) Start(ctx context.Context) {
	log.Printf("[INFO] On-demand ingest listening on %s (≤%d requests/min, Google News only)", OnDemandRequestChannel, o.perMinute)
	for ctx.Err() == nil {
		ps := o.rdb.Subscribe(ctx, OnDemandRequestChannel)
		for msg := range ps.Channel() {
			var req IngestRequest
			if json.Unmarshal([]byte(msg.Payload), &req) != nil {
				continue
			}
			select {
			case o.sem <- struct{}{}:
			default:
				log.Printf("[WARNING] On-demand ingest busy; dropping request %s", req.ID)
				continue
			}
			go func(req IngestRequest) {
				defer func() { <-o.sem }()
				rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				rep := o.handle(rctx, req, time.Now())
				if rep.Error != "" {
					log.Printf("[INFO] On-demand ingest %s (%s): %s", req.ID, req.Ticker, rep.Error)
				} else {
					log.Printf("[INFO] On-demand ingest %s (%s): %d headline(s) for %q", req.ID, req.Ticker, len(rep.Items), rep.Query)
				}
				b, _ := json.Marshal(rep)
				key := OnDemandReplyPrefix + req.ID
				pipe := o.rdb.Pipeline()
				pipe.RPush(rctx, key, b)
				pipe.Expire(rctx, key, 2*time.Minute)
				_, _ = pipe.Exec(rctx)
			}(req)
		}
		ps.Close()
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}
