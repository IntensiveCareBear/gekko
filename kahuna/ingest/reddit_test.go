package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newMockReddit stands up a server that answers the OAuth token endpoint and a
// /r/<sub>/new listing, and points the package URLs at it for the test's lifetime.
func newMockReddit(t *testing.T, children []map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_token") {
			if user, _, ok := r.BasicAuth(); !ok || user == "" {
				t.Error("token request missing basic auth")
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "tok-123", "token_type": "bearer", "expires_in": 3600,
			})
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			t.Errorf("listing request bad auth header: %q", r.Header.Get("Authorization"))
		}
		wrapped := make([]map[string]interface{}, len(children))
		for i, c := range children {
			wrapped[i] = map[string]interface{}{"kind": "t3", "data": c}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"children": wrapped},
		})
	}))
	oldTok, oldBase := redditTokenURL, redditAPIBase
	redditTokenURL = srv.URL + "/api/v1/access_token"
	redditAPIBase = srv.URL
	t.Cleanup(func() { redditTokenURL, redditAPIBase = oldTok, oldBase })
	return srv
}

func TestRedditSource_ParsesAndDedupes(t *testing.T) {
	now := float64(time.Now().Unix())
	srv := newMockReddit(t, []map[string]interface{}{
		{"id": "a1", "name": "t3_a1", "title": "Fed holds rates steady", "selftext": "discussion", "author": "econ_nerd", "subreddit": "economics", "permalink": "/r/economics/comments/a1/fed/", "score": 120, "num_comments": 45, "created_utc": now},
		{"id": "a2", "name": "t3_a2", "title": "Bitcoin breaks resistance", "author": "hodler", "subreddit": "cryptocurrency", "permalink": "/r/cryptocurrency/comments/a2/btc/", "score": 300, "num_comments": 88, "created_utc": now},
	})
	defer srv.Close()

	r := NewRedditSource(RedditConfig{ClientID: "id", ClientSecret: "secret", MaxCallsPerMin: 600})
	out := make(chan *GekkoIngestEvent, 8)
	ctx := context.Background()

	r.pollSubreddit(ctx, "economics", out)
	if got := len(out); got != 2 {
		t.Fatalf("first poll: expected 2 events, got %d", got)
	}
	ev := <-out
	if ev.SourceID != SourceReddit {
		t.Errorf("SourceID = %q, want REDDIT", ev.SourceID)
	}
	if !strings.HasPrefix(ev.SourceURL, "https://www.reddit.com/r/") {
		t.Errorf("SourceURL should be a www.reddit.com permalink, got %q", ev.SourceURL)
	}
	if ev.PayloadHash != ev.ComputeHash() {
		t.Error("payload hash mismatch (would be quarantined)")
	}
	var post RedditPost
	if err := json.Unmarshal([]byte(ev.RawContent), &post); err != nil {
		t.Fatalf("payload not valid RedditPost: %v", err)
	}
	if post.Title == "" || !strings.HasPrefix(post.Author, "u/") {
		t.Errorf("payload missing/malformed fields: %+v", post)
	}

	// Second poll of the same posts must emit nothing (dedupe by fullname).
	<-out
	r.pollSubreddit(ctx, "economics", out)
	if got := len(out); got != 0 {
		t.Errorf("second poll: expected 0 new events (deduped), got %d", got)
	}
}

func TestRedditSource_SkipsNSFWStickiedAndFiltersKeywords(t *testing.T) {
	now := float64(time.Now().Unix())
	srv := newMockReddit(t, []map[string]interface{}{
		{"id": "b1", "name": "t3_b1", "title": "CPI report shows cooling inflation", "author": "x", "subreddit": "economics", "permalink": "/r/economics/comments/b1/cpi/", "created_utc": now},
		{"id": "b2", "name": "t3_b2", "title": "Weekend off-topic thread", "author": "mod", "subreddit": "economics", "permalink": "/r/economics/comments/b2/ot/", "stickied": true, "created_utc": now},
		{"id": "b3", "name": "t3_b3", "title": "NSFW spam", "author": "spam", "subreddit": "economics", "permalink": "/r/economics/comments/b3/x/", "over_18": true, "created_utc": now},
		{"id": "b4", "name": "t3_b4", "title": "My cat is cute", "author": "y", "subreddit": "economics", "permalink": "/r/economics/comments/b4/cat/", "created_utc": now},
	})
	defer srv.Close()

	// Keyword filter keeps only macro-relevant posts; stickied + NSFW always dropped.
	r := NewRedditSource(RedditConfig{ClientID: "id", ClientSecret: "secret", MaxCallsPerMin: 600,
		Keywords: []string{"cpi", "inflation", "fed"}})
	out := make(chan *GekkoIngestEvent, 8)
	r.pollSubreddit(context.Background(), "economics", out)
	if got := len(out); got != 1 {
		t.Fatalf("expected only the CPI post to pass (stickied/NSFW/off-topic dropped), got %d", got)
	}
	var post RedditPost
	_ = json.Unmarshal([]byte((<-out).RawContent), &post)
	if !strings.Contains(strings.ToLower(post.Title), "cpi") {
		t.Errorf("wrong post survived filtering: %q", post.Title)
	}
}

func TestRedditSource_RSSParsesAndDedupes(t *testing.T) {
	const atom = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <author><name>/u/econ_nerd</name></author>
    <category term="Economics" label="r/Economics"/>
    <id>t3_a1</id>
    <link href="https://www.reddit.com/r/Economics/comments/a1/cpi_cools/"/>
    <published>2026-07-09T20:02:48+00:00</published>
    <title>CPI cools more than expected</title>
  </entry>
  <entry>
    <author><name>/u/hodler</name></author>
    <category term="Economics" label="r/Economics"/>
    <id>t3_a2</id>
    <link href="https://www.reddit.com/r/Economics/comments/a2/fed_holds/"/>
    <published>2026-07-09T20:05:10+00:00</published>
    <title>Fed holds rates steady</title>
  </entry>
</feed>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" || r.Header.Get("User-Agent") == "Go-http-client/1.1" {
			t.Errorf("RSS request needs a custom UA, got %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(atom))
	}))
	defer srv.Close()
	old := redditRSSBase
	redditRSSBase = srv.URL
	defer func() { redditRSSBase = old }()

	r := NewRedditSource(RedditConfig{RSS: true, MaxCallsPerMin: 600})
	out := make(chan *GekkoIngestEvent, 8)
	ctx := context.Background()

	r.pollSubredditRSS(ctx, "economics", out)
	if got := len(out); got != 2 {
		t.Fatalf("first RSS poll: expected 2 events, got %d", got)
	}
	ev := <-out
	if ev.SourceID != SourceReddit {
		t.Errorf("SourceID = %q, want REDDIT", ev.SourceID)
	}
	if !strings.HasPrefix(ev.SourceURL, "https://www.reddit.com/r/") {
		t.Errorf("SourceURL should be a permalink, got %q", ev.SourceURL)
	}
	if ev.PayloadHash != ev.ComputeHash() {
		t.Error("payload hash mismatch (would be quarantined)")
	}
	var post RedditPost
	if err := json.Unmarshal([]byte(ev.RawContent), &post); err != nil {
		t.Fatalf("payload not valid RedditPost: %v", err)
	}
	if post.Title == "" || post.Author != "u/econ_nerd" {
		t.Errorf("bad payload: %+v", post)
	}

	// Second poll of the same feed must emit nothing (dedupe by fullname).
	<-out
	r.pollSubredditRSS(ctx, "economics", out)
	if got := len(out); got != 0 {
		t.Errorf("second RSS poll: expected 0 new events (deduped), got %d", got)
	}
}

func TestRedditSource_IdleWithoutCredsOrRSS(t *testing.T) {
	r := NewRedditSource(RedditConfig{ClientID: "", ClientSecret: ""})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx, make(chan *GekkoIngestEvent, 1)) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("idle Start returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle Start did not return after cancel")
	}
}
