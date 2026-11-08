package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeQdrant struct {
	mu      sync.Mutex
	reqs    []string          // "METHOD path"
	bodies  map[string]string // "METHOD path" -> last body
	results string            // reply to /points/search
	failAll bool
}

func (f *fakeQdrant) serve() *httptest.Server {
	f.bodies = map[string]string{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.reqs = append(f.reqs, key)
		f.bodies[key] = string(b)
		f.mu.Unlock()
		if f.failAll {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			_, _ = w.Write([]byte(f.results))
		default:
			_, _ = w.Write([]byte(`{"result":true,"status":"ok"}`))
		}
	}))
}

func (f *fakeQdrant) body(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func TestNewsPointID_StableAndUUIDShaped(t *testing.T) {
	a, b := newsPointID("Crude falls"), newsPointID("Crude falls")
	if a != b || a == newsPointID("Crude rises") {
		t.Error("id must be deterministic and text-specific")
	}
	if parts := strings.Split(a, "-"); len(parts) != 5 || len(parts[0]) != 8 || len(parts[4]) != 12 {
		t.Errorf("not UUID-shaped: %s", a)
	}
}

func TestNewsStore_PersistsAndPrunes(t *testing.T) {
	f := &fakeQdrant{}
	srv := f.serve()
	defer srv.Close()
	qc := NewQdrantClient(srv.URL, "gordon_news")
	if err := qc.ensureNews(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.body("PUT /collections/gordon_news/index"), `"ts_ms"`) {
		t.Error("ts_ms must be indexed for range filters")
	}
	at := time.UnixMilli(1_700_000_000_000)
	if err := qc.upsertNews(context.Background(), []storedNews{{"Crude falls", []float32{1, 0}, at}}); err != nil {
		t.Fatal(err)
	}
	var up struct {
		Points []struct {
			ID      string         `json:"id"`
			Payload map[string]any `json:"payload"`
		} `json:"points"`
	}
	_ = json.Unmarshal([]byte(f.body("PUT /collections/gordon_news/points")), &up)
	if len(up.Points) != 1 || up.Points[0].ID != newsPointID("Crude falls") || up.Points[0].Payload["ts_ms"].(float64) != 1_700_000_000_000 {
		t.Errorf("unexpected upsert: %s", f.body("PUT /collections/gordon_news/points"))
	}
	if err := qc.pruneNews(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if body := f.body("POST /collections/gordon_news/points/delete"); !strings.Contains(body, `"lt":42`) {
		t.Errorf("prune must delete only points older than the cutoff: %s", body)
	}
}

func TestNewsStore_SearchMergesQdrantAndMemory(t *testing.T) {
	f := &fakeQdrant{results: `{"result":[{"score":0.9,"payload":{"text":"From before the restart"}},{"score":0.7,"payload":{"text":"Also in memory"}},{"score":0.8,"payload":{"nottext":1}}]}`}
	srv := f.serve()
	defer srv.Close()
	s := newNewsStore(NewQdrantClient(srv.URL, "gordon_news"), 10)
	now := time.Now()
	s.buf.add("Also in memory", []float32{1, 0}, now)      // cosine 1.0 with the query: wins over Qdrant's 0.7 for the same text
	s.buf.add("Only just arrived", []float32{1, 0.2}, now) // not yet persisted

	got := s.search([]float32{1, 0}, 0.6, 6*time.Hour, 5, now)
	want := []string{"Also in memory", "Only just arrived", "From before the restart"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %v, want %v (merged, de-duplicated, best similarity first)", got, want)
	}
	var req struct {
		ScoreThreshold float64 `json:"score_threshold"`
		Filter         struct {
			Must []struct {
				Key   string `json:"key"`
				Range struct {
					Gte int64 `json:"gte"`
				} `json:"range"`
			} `json:"must"`
		} `json:"filter"`
	}
	_ = json.Unmarshal([]byte(f.body("POST /collections/gordon_news/points/search")), &req)
	if req.ScoreThreshold != 0.6 || len(req.Filter.Must) != 1 || req.Filter.Must[0].Key != "ts_ms" ||
		req.Filter.Must[0].Range.Gte != now.Add(-6*time.Hour).UnixMilli() {
		t.Errorf("search must filter by age and similarity server-side: %+v", req)
	}
}

func TestNewsStore_QdrantDownDegradesToMemory(t *testing.T) {
	f := &fakeQdrant{failAll: true}
	srv := f.serve()
	defer srv.Close()
	s := newNewsStore(NewQdrantClient(srv.URL, "gordon_news"), 10)
	now := time.Now()
	s.add("Crude falls", []float32{1, 0}, now)
	if got := s.search([]float32{1, 0}, 0.6, time.Hour, 3, now); len(got) != 1 || got[0] != "Crude falls" {
		t.Errorf("an outage must fall back to the in-memory buffer, got %v", got)
	}
}

func TestNewsStore_AddQueuesForPersistenceAndNilIsSafe(t *testing.T) {
	s := newNewsStore(NewQdrantClient("http://unused", "gordon_news"), 10)
	s.add("  <b>Crude</b> falls ", []float32{1, 0}, time.Now())
	s.add("", []float32{1, 0}, time.Now()) // blank: ignored
	s.add("no vector", nil, time.Now())    // no embedding: ignored
	if len(s.queue) != 1 {
		t.Fatalf("want exactly one queued write, got %d", len(s.queue))
	}
	if n := <-s.queue; n.text != "Crude falls" {
		t.Errorf("text must be sanitized before it is stored, got %q", n.text)
	}
	var nilStore *newsStore
	nilStore.add("x", []float32{1}, time.Now())
	if nilStore.search([]float32{1}, 0, time.Hour, 1, time.Now()) != nil {
		t.Error("nil store finds nothing")
	}
}
