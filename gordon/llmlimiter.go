package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// LLMLimiter caps how many Ollama generate calls run at once. A local Ollama
// serves generations from a small pool (often one model at a time), so an
// unbounded multi-persona fleet just queues requests inside Ollama until they
// blow their deadline and fall back to keyword rules — the 2026-07-05 forensics
// found ~88% of proposals were rules fallbacks (18k "LLM generation failed" vs
// ~2.5k successes) for exactly this reason.
//
// It enforces two layers:
//   - an in-process semaphore (always on) bounding THIS persona process, and
//   - an optional Redis-backed distributed semaphore bounding the whole fleet of
//     persona processes that share one Ollama.
//
// The Redis limb fails OPEN: if Redis is unreachable the call proceeds (guarded
// only by the local semaphore) rather than blocking trading on a datastore
// hiccup. Fleet slots are leased with a TTL so a crashed holder's slot auto-frees
// instead of deadlocking the fleet.
type LLMLimiter struct {
	local chan struct{}
	rdb   *redis.Client
	key   string
	max   int
	lease time.Duration // slot TTL: must exceed one generate call's worst case
	poll  time.Duration // retry spacing while waiting for a fleet slot

	logOnce sync.Once

	waits    atomic.Int64 // acquisitions that had to wait meaningfully for a slot
	waitMs   atomic.Int64 // cumulative wait time across those acquisitions
	failOpen atomic.Int64 // acquisitions that proceeded despite a Redis error
	timeouts atomic.Int64 // acquisitions that gave up (no slot before deadline)
}

// NewLLMLimiter builds a limiter allowing max concurrent generate calls. rdb may
// be nil (in-process limiting only). lease is the per-slot TTL and should be at
// least one generate call's worst-case duration plus headroom.
func NewLLMLimiter(rdb *redis.Client, max int, lease time.Duration) *LLMLimiter {
	if max < 1 {
		max = 1
	}
	if lease <= 0 {
		lease = 60 * time.Second
	}
	return &LLMLimiter{
		local: make(chan struct{}, max),
		rdb:   rdb,
		key:   "gekko:llm_slots",
		max:   max,
		lease: lease,
		poll:  200 * time.Millisecond,
	}
}

// acquireScript atomically reaps expired slots, and if the set is under capacity,
// claims one for the caller's token. Returns 1 on success, 0 when at capacity.
var acquireScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local max = tonumber(ARGV[2])
local expiry = tonumber(ARGV[3])
local token = ARGV[4]
local keyttl = tonumber(ARGV[5])
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, now)
if redis.call('ZCARD', KEYS[1]) < max then
  redis.call('ZADD', KEYS[1], expiry, token)
  redis.call('PEXPIRE', KEYS[1], keyttl)
  return 1
end
return 0
`)

// Acquire blocks until a slot is free (fleet-wide, then in-process) or maxWait
// elapses, returning a release func that MUST be called exactly once (defer it).
// A nil limiter is a no-op returning a no-op release, so callers need no nil
// checks. On a clean give-up (no slot within maxWait) it returns a non-nil error
// and the caller should fall back rather than call the model.
func (l *LLMLimiter) Acquire(ctx context.Context, maxWait time.Duration) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	start := time.Now()

	waitCtx := ctx
	if maxWait > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, maxWait)
		defer cancel()
	}

	// 1. In-process slot first (cheap, always enforced even without Redis).
	select {
	case l.local <- struct{}{}:
	case <-waitCtx.Done():
		l.timeouts.Add(1)
		return func() {}, fmt.Errorf("llm limiter: no local slot within %v: %w", maxWait, waitCtx.Err())
	}

	// 2. Fleet-wide slot (optional). Fail open on a Redis error; give up cleanly
	//    (releasing the local slot) if we can't get one before the deadline.
	var token string
	if l.rdb != nil {
		t, err := l.acquireRedis(waitCtx)
		switch {
		case err == nil:
			token = t
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
			l.releaseLocal()
			l.timeouts.Add(1)
			return func() {}, fmt.Errorf("llm limiter: no fleet slot within %v: %w", maxWait, err)
		default:
			// Redis transport error: proceed under the local semaphore only.
			l.failOpen.Add(1)
			l.logOnce.Do(func() {
				log.Printf("[LLM-LIMIT] Redis unreachable (%v) — falling back to in-process concurrency limiting only.", err)
			})
		}
	}

	if waited := time.Since(start); waited >= l.poll {
		l.waits.Add(1)
		l.waitMs.Add(waited.Milliseconds())
	}
	return l.releaseFunc(token), nil
}

// acquireRedis polls the distributed semaphore until it claims a slot, the ctx
// expires (ctx error → clean give-up), or Redis errors (transport error → the
// caller fails open).
func (l *LLMLimiter) acquireRedis(ctx context.Context) (string, error) {
	token := newToken()
	for {
		ok, err := l.tryRedis(ctx, token, time.Now())
		if err != nil {
			return "", err
		}
		if ok {
			return token, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(l.poll):
		}
	}
}

func (l *LLMLimiter) tryRedis(ctx context.Context, token string, now time.Time) (bool, error) {
	nowMs := now.UnixMilli()
	expiry := now.Add(l.lease).UnixMilli()
	keyTTL := l.lease.Milliseconds() + 1000
	res, err := acquireScript.Run(ctx, l.rdb, []string{l.key}, nowMs, l.max, expiry, token, keyTTL).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

func (l *LLMLimiter) releaseLocal() {
	select {
	case <-l.local:
	default:
	}
}

// releaseFunc returns an idempotent release for a held slot (and its fleet token,
// if any).
func (l *LLMLimiter) releaseFunc(token string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if token != "" && l.rdb != nil {
				rc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = l.rdb.ZRem(rc, l.key, token).Err()
				cancel()
			}
			l.releaseLocal()
		})
	}
}

// Stats returns a health snapshot: total waits, mean wait ms, fail-open count,
// and clean-give-up (timeout) count. Nil-safe.
func (l *LLMLimiter) Stats() (waits, avgWaitMs, failOpen, timeouts int64) {
	if l == nil {
		return 0, 0, 0, 0
	}
	w := l.waits.Load()
	avg := int64(0)
	if w > 0 {
		avg = l.waitMs.Load() / w
	}
	return w, avg, l.failOpen.Load(), l.timeouts.Load()
}

func newToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d-%x", time.Now().UnixNano(), b)
}
