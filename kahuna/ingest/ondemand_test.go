package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildNewsQuery(t *testing.T) {
	cases := map[string]string{
		"Will the maximum temperature be 78-79° on Oct 14, 2026? — 78° to 79°":      "temperature",
		"Will EIA crude oil inventories exceed 415 million barrels? — 415 or above": "EIA crude oil inventories million barrels",
		"Will Bitcoin close above $81,749.99 on Oct 15 at 5pm EDT?":                 "Bitcoin",
		"Will the Fed cut interest rates at the December 2026 meeting?":             "Fed cut interest rates meeting",
		"   ":             "",
		"Will 2026 be 5?": "",
	}
	for in, want := range cases {
		if got := buildNewsQuery(in); got != want {
			t.Errorf("buildNewsQuery(%q) = %q, want %q", in, got, want)
		}
	}
	long := buildNewsQuery("alpha bravo charlie delta echo foxtrot golf hotel india juliet")
	if n := len(strings.Fields(long)); n != 7 {
		t.Errorf("query must be capped at 7 terms, got %d: %q", n, long)
	}
}

func TestCleanHeadline(t *testing.T) {
	got := cleanHeadline("  Oil <b>jumps</b> &amp; stocks\nfall\x00 - Reuters ")
	if got != "Oil jumps & stocks fall - Reuters" {
		t.Errorf("got %q", got)
	}
}

const rssFixture = `<?xml version="1.0"?><rss><channel><title>t</title><link>l</link>
<item><title>Crude stocks fall more than expected - Reuters</title><link>https://example.com/1</link><pubDate>Wed, 15 Oct 2026 10:00:00 GMT</pubDate></item>
<item><title>  </title><link>https://example.com/2</link></item>
<item><title>EIA: inventories draw &amp; refiners ramp - AP</title><link>https://example.com/3</link></item>
<item><title>Third</title><link>https://example.com/4</link></item></channel></rss>`

func newsServer(hits *atomic.Int64, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.Contains(r.URL.Query().Get("q"), "when:2d") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(rssFixture))
	}))
}

func TestFetchGoogleNews(t *testing.T) {
	var hits atomic.Int64
	srv := newsServer(&hits, 200)
	defer srv.Close()
	items, err := fetchGoogleNews(context.Background(), srv.Client(), srv.URL, "crude oil", 2)
	if err != nil || len(items) != 2 || items[1].Title != "EIA: inventories draw & refiners ramp - AP" {
		t.Fatalf("got %+v, %v (the blank-title item must be skipped, max respected)", items, err)
	}
	bad := newsServer(&hits, 503)
	defer bad.Close()
	if _, err := fetchGoogleNews(context.Background(), bad.Client(), bad.URL, "x", 3); err == nil {
		t.Error("HTTP 503 must be an error")
	}
}

func TestOnDemandHandle(t *testing.T) {
	var hits atomic.Int64
	srv := newsServer(&hits, 200)
	defer srv.Close()
	o := NewOnDemandIngest(nil, 2)
	o.base = srv.URL
	now := time.Now()
	req := IngestRequest{ID: "r1", Ticker: "KXEIA", Title: "Will EIA crude oil inventories exceed 415 million barrels?", MaxItems: 5}

	rep := o.handle(context.Background(), req, now)
	if rep.Error != "" || len(rep.Items) != 3 || rep.Query == "" {
		t.Fatalf("want 3 headlines, got %+v", rep)
	}
	req.ID = "r2"
	if rep := o.handle(context.Background(), req, now.Add(time.Minute)); len(rep.Items) != 3 || hits.Load() != 1 {
		t.Errorf("identical query inside 10 minutes must be served from cache (hits=%d)", hits.Load())
	}
	// budget: 2 per minute, only on cache misses
	a := IngestRequest{ID: "a", Title: "Will uranium prices rise?"}
	b := IngestRequest{ID: "b", Title: "Will lithium prices rise?"}
	c := IngestRequest{ID: "c", Title: "Will copper prices rise?"}
	t0 := now.Add(20 * time.Minute)
	o.handle(context.Background(), a, t0)
	o.handle(context.Background(), b, t0)
	if rep := o.handle(context.Background(), c, t0); rep.Error != "rate limited" {
		t.Errorf("third fetch inside a minute must be rate limited, got %+v", rep)
	}
	if rep := o.handle(context.Background(), c, t0.Add(61*time.Second)); rep.Error != "" {
		t.Errorf("budget must reset after a minute, got %+v", rep)
	}
}

func TestOnDemandRejectsBadRequests(t *testing.T) {
	o := NewOnDemandIngest(nil, 6)
	now := time.Now()
	cases := map[string]IngestRequest{
		"no id":       {Title: "Will oil rise?"},
		"expired":     {ID: "x", Title: "Will oil rise?", DeadlineMs: now.Add(-time.Second).UnixMilli()},
		"no keywords": {ID: "x", Title: "Will 2026 be 5?"},
		"id too long": {ID: strings.Repeat("a", 81), Title: "Will oil rise?"},
	}
	for name, req := range cases {
		if rep := o.handle(context.Background(), req, now); rep.Error == "" || len(rep.Items) != 0 {
			t.Errorf("%s: want an error and no items, got %+v", name, rep)
		}
	}
}
