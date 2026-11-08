package main

import (
	"strconv"
	"strings"
	"time"
)

// monthAbbr maps Kalshi's 3-letter month codes to a month number.
var monthAbbr = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

// kalshiTZ is the exchange clock (US Eastern, with DST), resolved once with a
// fixed-EST fallback when tzdata is unavailable at runtime.
var kalshiTZ = func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.FixedZone("EST", -5*3600)
}()

// tickerExpiry parses the real resolution instant encoded in a Kalshi ticker's
// event segment (2nd dash-part: YYMMMDD optionally followed by an intraday time)
// and returns (expiry, true). It mirrors gordon's tickerExpiry and lou's
// contractExpiryUnixMS — including the H-prefixed, bare-HHMM, and bare-HH intraday
// forms — so breen ages positions by the SAME resolution clock the entry guards
// use. Tickers with no parseable date (POLY-<token>, JPMCEOCHANGE-27, synthetics)
// return (_, false) so the caller falls back to the random paper-expiry window.
// Times are US Eastern (Kalshi's convention); a dateless intraday defaults to end
// of day so "resolves today" reads as hours away rather than already past.
func tickerExpiry(contractID string) (time.Time, bool) {
	parts := strings.Split(contractID, "-")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	seg := strings.ToUpper(parts[1])
	if len(seg) < 7 {
		return time.Time{}, false
	}
	yy, err1 := strconv.Atoi(seg[0:2])
	mon, ok := monthAbbr[seg[2:5]]
	dd, err2 := strconv.Atoi(seg[5:7])
	if err1 != nil || !ok || err2 != nil || dd < 1 || dd > 31 {
		return time.Time{}, false
	}
	hour, min := 23, 59 // default: end of day so a dateless intraday reads as hours away
	switch rest := seg[7:]; {
	case len(rest) >= 5 && rest[0] == 'H':
		hour, min = parseHHMM(rest[1:5], hour, min)
	case len(rest) == 4:
		hour, min = parseHHMM(rest, hour, min)
	case len(rest) == 2:
		if h, e := strconv.Atoi(rest); e == nil && h < 24 {
			hour, min = h, 0
		}
	}
	return time.Date(2000+yy, time.Month(mon), dd, hour, min, 0, 0, kalshiTZ), true
}

// parseHHMM parses a 4-char "HHMM" string, returning (defHour, defMin) unchanged
// if it isn't a valid 24h clock time. Pure.
func parseHHMM(s string, defHour, defMin int) (int, int) {
	if len(s) < 4 {
		return defHour, defMin
	}
	h, e1 := strconv.Atoi(s[0:2])
	m, e2 := strconv.Atoi(s[2:4])
	if e1 != nil || e2 != nil || h >= 24 || m >= 60 {
		return defHour, defMin
	}
	return h, m
}

// resolveExpiry returns the expiry to stamp on a newly opened paper position:
// the contract's REAL resolution time when the ticker encodes one, else a random
// draw from the configured paper-expiry window. Using the real date is the fix for
// the 2026-07-10 forensics finding — every position was previously given a random
// 2–48h life regardless of the contract, so forceExitNearExpiry churned multi-week
// and multi-year contracts within a day and settleExpired coin-flipped them.
func (pt *PositionTracker) resolveExpiry(contractID string) time.Time {
	if exp, ok := tickerExpiry(contractID); ok {
		return exp
	}
	return time.Now().Add(pt.randomExpiry())
}
