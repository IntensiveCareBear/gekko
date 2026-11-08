package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"time"
)

// newsStore is gordon's memory of the headlines it has seen: a persistent Qdrant collection
// (gordon_news) behind an in-memory buffer. Until now the buffer was all there was, so every
// restart forgot the news and an activity look right after a relaunch found nothing. The
// events collection is not a substitute: it holds past proposals and outcomes, not headlines.
//
// Writes are asynchronous and best effort; reads merge Qdrant with the buffer, so a Qdrant outage
// degrades to the old behaviour instead of failing.
type newsStore struct {
	buf       *newsBuffer
	qc        *QdrantClient // nil = memory only
	queue     chan storedNews
	retention time.Duration
	failed    int // consecutive Qdrant errors, for log throttling
}

type storedNews struct {
	text string
	vec  []float32
	at   time.Time
}

func newNewsStore(qc *QdrantClient, bufCap int) *newsStore {
	return &newsStore{buf: newNewsBuffer(bufCap), qc: qc, queue: make(chan storedNews, 1024),
		retention: getEnvDuration("NEWS_RETENTION", 72*time.Hour)}
}

// add remembers a headline: memory now, Qdrant shortly.
func (s *newsStore) add(text string, vec []float32, at time.Time) {
	if s == nil {
		return
	}
	text = sanitizeHeadline(text)
	if text == "" || len(vec) == 0 {
		return
	}
	s.buf.add(text, vec, at)
	if s.qc != nil {
		select {
		case s.queue <- storedNews{text, vec, at}:
		default: // writer is behind: the buffer still has it
		}
	}
}

// search returns up to k relevant headlines, most similar first, from Qdrant and the buffer.
func (s *newsStore) search(vec []float32, minSim float64, maxAge time.Duration, k int, now time.Time) []string {
	if s == nil {
		return nil
	}
	cands := map[string]float64{}
	for _, h := range s.buf.searchScored(vec, minSim, maxAge, now) {
		cands[h.text] = h.sim
	}
	if s.qc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		hits, err := s.qc.searchNews(ctx, vec, minSim, now.Add(-maxAge).UnixMilli(), k*2)
		cancel()
		if err != nil {
			s.noteErr("search", err)
		} else {
			s.failed = 0
			for _, h := range hits {
				if h.sim > cands[h.text] {
					cands[h.text] = h.sim
				}
			}
		}
	}
	all := make([]newsHit, 0, len(cands))
	for t, sim := range cands {
		all = append(all, newsHit{t, sim})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].sim > all[j].sim })
	var out []string
	for _, h := range all {
		out = append(out, h.text)
		if len(out) == k {
			break
		}
	}
	return out
}

func (s *newsStore) noteErr(op string, err error) {
	s.failed++
	if s.failed == 1 || s.failed%50 == 0 { // don't spam while Qdrant is down
		log.Printf("[NEWS] store %s failed (%d in a row), using memory only: %v", op, s.failed, err)
	}
}

// start prepares the collection and runs the writer and the retention sweep until ctx ends.
func (s *newsStore) start(ctx context.Context) {
	if s == nil || s.qc == nil {
		return
	}
	go func() {
		ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.qc.ensureNews(ectx)
		cancel()
		if err != nil {
			s.noteErr("init", err)
		} else {
			log.Printf("[NEWS] persistent store ready: collection %s, retention %v", s.qc.collection, s.retention)
		}
		flush := time.NewTicker(2 * time.Second)
		sweep := time.NewTicker(time.Hour)
		defer flush.Stop()
		defer sweep.Stop()
		var batch []storedNews
		write := func() {
			if len(batch) == 0 {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := s.qc.upsertNews(wctx, batch); err != nil {
				s.noteErr("write", err)
			} else {
				s.failed = 0
			}
			cancel()
			batch = batch[:0]
		}
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-s.queue:
				batch = append(batch, n)
				if len(batch) >= 50 {
					write()
				}
			case <-flush.C:
				write()
			case <-sweep.C:
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := s.qc.pruneNews(pctx, time.Now().Add(-s.retention).UnixMilli()); err != nil {
					s.noteErr("prune", err)
				}
				cancel()
			}
		}
	}()
}

// newsPointID is a stable UUID for a headline, so re-adding it overwrites instead of duplicating.
func newsPointID(text string) string {
	h := sha1.Sum([]byte(text))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func (qc *QdrantClient) newsDo(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, qc.baseURL+"/collections/"+qc.collection+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := qc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qdrant %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(b))
	}
	return b, nil
}

// ensureNews creates the collection if needed and an integer index on ts_ms for range filters.
func (qc *QdrantClient) ensureNews(ctx context.Context) error {
	if _, err := qc.EnsureCollection(ctx); err != nil {
		return err
	}
	_, err := qc.newsDo(ctx, "PUT", "/index?wait=true", map[string]string{"field_name": "ts_ms", "field_schema": "integer"})
	return err
}

func (qc *QdrantClient) upsertNews(ctx context.Context, items []storedNews) error {
	points := make([]map[string]any, 0, len(items))
	for _, it := range items {
		points = append(points, map[string]any{
			"id": newsPointID(it.text), "vector": it.vec,
			"payload": map[string]any{"text": it.text, "ts_ms": it.at.UnixMilli()},
		})
	}
	_, err := qc.newsDo(ctx, "PUT", "/points?wait=false", map[string]any{"points": points})
	return err
}

func (qc *QdrantClient) searchNews(ctx context.Context, vec []float32, minSim float64, cutoffMs int64, limit int) ([]newsHit, error) {
	b, err := qc.newsDo(ctx, "POST", "/points/search", map[string]any{
		"vector": vec, "limit": limit, "with_payload": true, "score_threshold": minSim,
		"filter": map[string]any{"must": []map[string]any{{"key": "ts_ms", "range": map[string]any{"gte": cutoffMs}}}},
	})
	if err != nil {
		return nil, err
	}
	var r struct {
		Result []struct {
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	var out []newsHit
	for _, p := range r.Result {
		if t, _ := p.Payload["text"].(string); t != "" {
			out = append(out, newsHit{t, p.Score})
		}
	}
	return out, nil
}

func (qc *QdrantClient) pruneNews(ctx context.Context, cutoffMs int64) error {
	_, err := qc.newsDo(ctx, "POST", "/points/delete?wait=false", map[string]any{
		"filter": map[string]any{"must": []map[string]any{{"key": "ts_ms", "range": map[string]any{"lt": cutoffMs}}}},
	})
	return err
}
