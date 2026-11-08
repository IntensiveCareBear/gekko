package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Shadow mode (2026-08-25 desk review, fix #3).
//
// The LLM persona book has no demonstrated edge. Lou's validated lessons put every
// persona between −7% and −12% on deployed capital, the current period has the
// non-weather book at −$419 over 41 trades, and the long tape is ≈ −$52k across ~3,700
// trades. The system already "knows" this and keeps trading them anyway, because
// ADAPTIVE_EXPLORE_FLOOR / ADAPTIVE_MIN_WEIGHT hold a two-month loser at quarter weight
// forever — the kill-switch bottoms out instead of reaching zero.
//
// Shadow mode is the blunt version of that decision, and deliberately not a deletion:
// a shadowed persona keeps proposing, so its proposals are still recorded, still
// scoreable, and still able to earn their way back — but nothing it says reaches the
// book. It is the difference between "we think you are bad" and "prove it on paper".
//
// The shadow check sits at the Kafka consumer, BEFORE the proposal is routed, so a
// shadowed persona cannot influence a filling order indirectly through consensus
// (contributing conviction to a consolidated BUY, or vetoing someone else's with a
// bearish signal). Dropping the bearish signals too is intentional: a veto from a
// persona with no measured alpha is noise, not a brake.
//
// Re-activation is a written bar, not a vibe. The review's suggested wording: positive
// net-of-fee expectancy over 200 shadow proposals. Scoring the shadow book against
// realized outcomes is a follow-on job — this file records everything that job needs
// (contract, side, price, size, author, conviction, timestamp) and nothing else.

const (
	shadowProposalsKey = "gekko:shadow_proposals"
	maxShadowProposals = 2000
)

// ShadowProposal is one proposal a shadowed persona would have traded, captured at the
// moment it was suppressed.
type ShadowProposal struct {
	ContractID string   `json:"contract_id"`
	Action     string   `json:"action"`
	Price      float64  `json:"price"`
	Size       int      `json:"size"`
	Notional   float64  `json:"notional"`
	Personas   []string `json:"personas"`
	Confidence float64  `json:"confidence"`
	Bearish    bool     `json:"bearish,omitempty"`
	ProposedAt int64    `json:"proposed_at_ms"`
	StrategyID string   `json:"strategy_id,omitempty"`
	EventHash  string   `json:"event_hash,omitempty"`
}

// isShadowed reports whether EVERY persona credited for a proposal is shadowed.
//
// "Every" rather than "any", for the same reason the settlement-hold check uses it: a
// consolidated order that mixes a shadowed persona with a live one is not purely the
// shadowed persona's idea, and suppressing it would silently mute the live persona too.
// In practice this is not a live distinction — the check runs before consensus, on
// individual proposals, so each one has a single author.
func (pt *PositionTracker) isShadowed(p *StrategyProposal) bool {
	if p == nil || len(pt.shadowPersonas) == 0 {
		return false
	}
	credited := creditedPersonas(p.Persona, proposalContributors(p))
	if len(credited) == 0 {
		return false
	}
	for _, name := range credited {
		if !pt.shadowPersonas[strings.ToLower(name)] {
			return false
		}
	}
	return true
}

// captureShadowProposal records a suppressed proposal and reports whether the caller
// must stop routing it. False means "not shadowed — carry on as normal", so the call
// site reads as a guard rather than a branch.
//
// Recording is best-effort: a Redis hiccup must never turn into an executed trade, so
// the suppression stands whether or not the write lands.
func (pt *PositionTracker) captureShadowProposal(p *StrategyProposal) bool {
	if !pt.isShadowed(p) {
		return false
	}
	pt.shadowProposals.Add(1)

	sp := ShadowProposal{
		ContractID: p.ContractID,
		Action:     strings.ToUpper(p.Action),
		Price:      p.AveragePrice,
		Size:       p.TotalSize,
		Notional:   p.AveragePrice * float64(p.TotalSize),
		Personas:   creditedPersonas(p.Persona, proposalContributors(p)),
		Confidence: p.Confidence,
		Bearish:    p.Bearish,
		ProposedAt: time.Now().UnixMilli(),
		StrategyID: p.StrategyID,
		EventHash:  p.EventHash,
	}
	// The "signal" / "trade" split is the same one the scorer counts on: a bearish
	// signal-only proposal is sizeless by construction, so it has no expectancy and does
	// not advance the re-activation bar. Saying so at capture time keeps the log honest
	// about how much scoreable evidence is actually accumulating.
	kind := "would-be trade"
	if !sp.actionable() {
		kind = "signal only, not scoreable"
	}
	log.Printf("[SHADOW] %s %s size=%d @ %.4f ($%.2f notional) persona=%s conf=%.2f — recorded (%s), not routed",
		sp.Action, sp.ContractID, sp.Size, sp.Price, sp.Notional,
		strings.Join(sp.Personas, "+"), sp.Confidence, kind)

	// Ledger receipt so the suppressed flow is auditable alongside real orders and
	// [RISK] rejections; replayLedger ignores SHADOW (no cash or position moves).
	if pt.ledger != nil {
		_, _ = pt.ledger.AppendTransaction(sp.ContractID, sp.Action, sp.Size, sp.Price, "SHADOW", 0,
			strings.Join(sp.Personas, "+"))
	}
	if pt.redisClient != nil {
		if data, err := json.Marshal(sp); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			pipe := pt.redisClient.Pipeline()
			pipe.LPush(ctx, shadowProposalsKey, string(data))
			pipe.LTrim(ctx, shadowProposalsKey, 0, maxShadowProposals-1)
			_, _ = pipe.Exec(ctx)
		}
	}
	return true
}

// dayKey is the calendar day an entry counts against. Local time on purpose: the desk
// and the venues it trades run on a wall-clock day, not a UTC one.
func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// entriesTodayLocked returns how many entries have filled on the CURRENT day, treating
// a stale day-stamp as zero rather than mutating under a read lock. Caller holds pt.mu
// (read or write).
func (pt *PositionTracker) entriesTodayLocked() int {
	if pt.entriesDay != dayKey(time.Now()) {
		return 0
	}
	return pt.entriesToday
}

// familyEntriesTodayLocked is the same, per contract family. Caller holds pt.mu.
func (pt *PositionTracker) familyEntriesTodayLocked(family string) int {
	if pt.entriesDay != dayKey(time.Now()) {
		return 0
	}
	return pt.familyEntriesToday[family]
}

// noteEntryFilledLocked records that a BUY actually filled, rolling the counters over
// when the day has changed. Called only on a real fill, so proposals the gate rejects or
// the book refuses never spend the day's budget. Caller holds pt.mu (write).
func (pt *PositionTracker) noteEntryFilledLocked(contractID string) {
	today := dayKey(time.Now())
	if pt.entriesDay != today {
		pt.entriesDay, pt.entriesToday = today, 0
		pt.familyEntriesToday = map[string]int{}
	}
	if pt.familyEntriesToday == nil {
		pt.familyEntriesToday = map[string]int{}
	}
	pt.entriesToday++
	pt.familyEntriesToday[contractFamily(contractID)]++
}

// shadowProposalsSnapshot reads back the recorded shadow book, newest first. Used by
// the API/scoring job; returns nil when Redis is absent.
func (pt *PositionTracker) shadowProposalsSnapshot(ctx context.Context, limit int64) []ShadowProposal {
	if pt.redisClient == nil {
		return nil
	}
	if limit <= 0 || limit > maxShadowProposals {
		limit = maxShadowProposals
	}
	items, err := pt.redisClient.LRange(ctx, shadowProposalsKey, 0, limit-1).Result()
	if err != nil && err != redis.Nil {
		return nil
	}
	out := make([]ShadowProposal, 0, len(items))
	for _, it := range items {
		var sp ShadowProposal
		if json.Unmarshal([]byte(it), &sp) == nil {
			out = append(out, sp)
		}
	}
	return out
}
