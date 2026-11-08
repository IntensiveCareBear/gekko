package main

import (
	"testing"
	"time"
)

func TestTickerExpiry(t *testing.T) {
	et := func(y int, mon time.Month, d, h, mi int) time.Time {
		return time.Date(y, mon, d, h, mi, 0, 0, kalshiTZ)
	}
	cases := []struct {
		id   string
		want time.Time
		ok   bool
	}{
		// Date-only → end of day ET.
		{"KXCPI-26JUN-T0.5", et(2026, 6, 1, 23, 59), false}, // "26JUN" has no DD (len 5) → unparseable
		{"KXINX-26JUN26", et(2026, 6, 26, 23, 59), true},
		{"KXCITRINI-28JUL01", et(2028, 7, 1, 23, 59), true},
		// H-prefixed intraday.
		{"KXINXY-26DEC31H1600-B8300", et(2026, 12, 31, 16, 0), true},
		// Bare-HHMM sub-hourly.
		{"KXETH15M-26JUL090845", et(2026, 7, 9, 8, 45), true},
		// Bare-HH hourly.
		{"KXBTCD-26JUL0917", et(2026, 7, 9, 17, 0), true},
		// Undateable → not ok.
		{"POLY-123456789", time.Time{}, false},
		{"JPMCEOCHANGE-27", time.Time{}, false},
		{"FEDHIKE-26DEC31", et(2026, 12, 31, 23, 59), true},
		{"NODASH", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := tickerExpiry(c.id)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v", c.id, ok, c.ok)
			continue
		}
		if ok && !got.Equal(c.want) {
			t.Errorf("%s: got %s want %s", c.id, got, c.want)
		}
	}
}

func TestResolveExpiryFallback(t *testing.T) {
	pt := &PositionTracker{expiryMin: 2 * time.Hour, expiryMax: 48 * time.Hour}

	// Dateable ticker → real resolution instant, ignoring the random window.
	got := pt.resolveExpiry("KXCITRINI-28JUL01")
	if want := time.Date(2028, 7, 1, 23, 59, 0, 0, kalshiTZ); !got.Equal(want) {
		t.Errorf("dateable: got %s want %s", got, want)
	}

	// Undateable ticker → random window in [now+min, now+max].
	before := time.Now()
	poly := pt.resolveExpiry("POLY-999")
	lo, hi := before.Add(2*time.Hour), time.Now().Add(48*time.Hour)
	if poly.Before(lo) || poly.After(hi) {
		t.Errorf("poly fallback %s outside [%s, %s]", poly, lo, hi)
	}
}
