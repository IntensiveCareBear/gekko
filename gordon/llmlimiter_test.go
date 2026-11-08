package main

import (
	"context"
	"testing"
	"time"
)

// A nil limiter must be a usable no-op so callers need no nil checks.
func TestLLMLimiter_NilIsNoop(t *testing.T) {
	var l *LLMLimiter
	release, err := l.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("nil limiter Acquire errored: %v", err)
	}
	release() // must not panic
	if w, _, _, _ := l.Stats(); w != 0 {
		t.Errorf("nil limiter Stats should be zero, got waits=%d", w)
	}
}

// With no Redis, the in-process semaphore alone must enforce the concurrency cap.
func TestLLMLimiter_LocalCapBlocksAndReleases(t *testing.T) {
	l := NewLLMLimiter(nil, 1, time.Minute)

	rel1, err := l.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	// Second acquire must give up cleanly once the (short) wait elapses.
	start := time.Now()
	if _, err := l.Acquire(context.Background(), 60*time.Millisecond); err == nil {
		t.Fatal("expected second acquire to time out while the only slot is held")
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Errorf("expected the acquire to wait for its deadline, returned after %v", waited)
	}
	if _, _, _, timeouts := l.Stats(); timeouts != 1 {
		t.Errorf("expected 1 timeout recorded, got %d", timeouts)
	}

	// Releasing the first slot must let a subsequent acquire succeed.
	rel1()
	rel2, err := l.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	rel2()
}

// Release is idempotent: calling it twice must not free a slot it doesn't own.
func TestLLMLimiter_ReleaseIdempotent(t *testing.T) {
	l := NewLLMLimiter(nil, 1, time.Minute)
	rel, err := l.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	rel()
	rel() // double release — must be a no-op

	// Fill the single slot, then confirm a second acquire still blocks (i.e. the
	// double release above did not leak an extra permit).
	held, err := l.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("re-acquire failed: %v", err)
	}
	defer held()
	if _, err := l.Acquire(context.Background(), 30*time.Millisecond); err == nil {
		t.Fatal("double release leaked a slot — concurrency cap breached")
	}
}

// max < 1 is clamped so the limiter is always usable.
func TestLLMLimiter_ClampsMax(t *testing.T) {
	l := NewLLMLimiter(nil, 0, 0)
	rel, err := l.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("acquire on clamped limiter failed: %v", err)
	}
	rel()
}
