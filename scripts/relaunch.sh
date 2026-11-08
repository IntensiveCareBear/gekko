#!/usr/bin/env bash
#
# relaunch.sh — the live launch command, reconstructed from the running stack on
# 2026-09-03 and re-issued with the knobs that day's forensics turned on.
#
# Everything set here is a deliberate override of a run.sh default; anything
# run.sh already defaults correctly is deliberately absent. To regenerate after a
# manual launch, diff a running service's environment against run.sh's
# ${VAR:-default} values and keep only what differs.
#
# Usage:  ./scripts/relaunch.sh     (holds the terminal; Ctrl+C tears down cleanly)
#
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# ── data sources and credentials ────────────────────────────────────────────
# Read from the gitignored key file, never inlined — this script is tracked and
# the repo has a remote. KALSHI_API_KEY is the key ID; gekko.txt holds the
# private key it pairs with.
if [ ! -r kalshi_api_key.txt ]; then
  echo "relaunch.sh: kalshi_api_key.txt is missing — Kalshi market data would run unauthenticated." >&2
  exit 1
fi
KALSHI_API_KEY="$(tr -d '[:space:]' < kalshi_api_key.txt)"
export KALSHI_API_KEY
export KALSHI_PRIVATE_KEY_FILE=gekko.txt
export FINNHUB_API_KEY_FILE="$PWD/finhub.txt"
export FRED_API_KEY_FILE="$PWD/fred.txt"
# Twitter/X is off (credits depleted); run.sh gates it on TWITTER_ENABLED=true.
export ECON_CALENDAR_URL=http://localhost:8085/econ_calendar.json

# ── fleet ───────────────────────────────────────────────────────────────────
export WEATHER_ENABLED=true
# 2026-10-15: activity trigger (docs/gordons-creed-proposal.md Phase 2). marketdata publishes a
# market-activity event when a mid moves >=8% (and >=3c) in a minute, or top-of-book size surges
# 3x; gordon-default then evaluates that market through the normal funnel (pre-gate, RAG, LLM,
# edge gates), at most once per contract family per 10 minutes. Weather gordon ignores it.
export ACTIVITY_TRIGGER=true
# Phase 3: before an activity-triggered LLM call, look through news gordon has already seen; if
# nothing relevant, ask kahuna for headlines on the market (Google News, <=6 requests/min) and wait up
# to 45s. No news found => skip the event (ONDEMAND_ON_MISS=proceed to evaluate anyway).
export ONDEMAND_INGEST=true
# (no GORDON_WEATHER_MODE here: run.sh sets it for gordon-weather only; a global export
# turned gordon-default into a second weather trader)
# 2026-10-01: the fleet is one gordon-default on llama3.3:70b (run.sh's defaults: one
# slot, 180s deadlines), trading LIVE. The gemma trio and its 45s/2-slot limits are
# gone; to shadow the 70B persona instead, set SHADOW_PERSONAS=gordon-default.
# Trade size. Every
# weather proposal leaves gordon at exactly this many contracts; breen's consensus
# then scales it by conviction (0.5×–2.0×). At 100 that produced 50–150 contracts,
# i.e. $20–$57 notional — too small to be worth the round trip. 1000 puts entries in
# the $200–$1,100 band, still inside the ~$2.0k per-family cap (2% of NAV).
export WEATHER_BASE_SIZE=1000
# 2026-10-15: predictive picks under 5c ask hit 0 of 26 over 7 settled days (model
# said ~19%, P(0 hits) < 0.5%; scripts/weather_review.py). Default was 0.03. No winner in
# the sample sat below 0.05. Revisit at ~20 settled days.
export WEATHER_PREDICTIVE_MIN_ASK=0.05
# 2026-10-19: predictive weather off. 169 settled picks over 11 days lost in every ask and
# edge band (mean fair 0.26, hit rate 0.089): the 3°F sigma floor spreads probability over
# ~5 buckets and buys the cheap outer ones. Cleared-bucket trading stays on. Re-enable only
# after a per-station sigma refit scores positive in scripts/weather_review.py.
export WEATHER_PREDICTIVE=false
# 2026-10-19: gordon-default trades paper again, by operator decision, after a brief shadow
# (scripts/gordon_review.py: LLM fair Brier 0.2101 vs book mid 0.2019; 41 trades since 10-09
# lost ≈$3.5k, mostly fees and spread). What changed for this run: fees on Kalshi crypto
# (3dc5fb4), the 2× friction gate below, ONDEMAND_MIN_SIM 0.73 and no weather buckets on the
# activity path. To shadow it again: export SHADOW_PERSONAS=gordon-default.

# ── entry gates ─────────────────────────────────────────────────────────────
export MIN_EDGE=0.07
# 2026-10-21: back to 1x (docs/forensics-2026-10-21.md). Gordon has no edge, so 10x only
# multiplied fees, and 15 of 21 proposals walked >3c through thin books and were rejected.
# Judge after 100 first-per-ticker taken BUYs in scripts/gordon_review.py.
# Breen's 2%-of-NAV per-contract and 6% category caps stay as the backstop.
export BET_SIZE_MULT=1
export PORTFOLIO_MAX_SIZE=25000
export PORTFOLIO_MAX_EXP=30000
export MIN_ENTRY_PRICE=0.12
export MAX_ENTRY_PRICE=0.70
export BEARISH_COOLDOWN=10m
export DECLINE_COOLDOWN=2m
export MID_BAND_MIN_CONFIDENCE=0.45
# 2026-10-15: was 0.15, equal to the -15% stop: gordon buys the ask and stops mark the
# bid/mid, so a book at the cap sat on its stop at the first mark (3 of 18 trades stopped
# at 0m held, 7 inside 30m). 0.05 keeps the spread well inside the stop.
export ENTRY_MAX_SPREAD_PCT=0.05
export MAX_ENTRIES_PER_DAY=40
export MAX_FAMILY_ENTRIES_PER_DAY=2
export CONSENSUS_MIN_CONVICTION=0.25

# ── exits, costs, policy ────────────────────────────────────────────────────
export TAKE_PROFIT_PCT=40
export KALSHI_FEE_CHARGE=true
# 2026-10-19: breen re-checks edge at the fill against 2× round-trip spread + fees (desk
# review 08-25 fix 4, built in a31118a, never enabled). Needs 3dc5fb4, or Kalshi crypto
# is priced at zero fee. Inert for proposals without a fair value.
export ENTRY_EDGE_FRICTION_MULT=2
# 2026-10-15: graduated de-risking. BUY fills shrink linearly from x1.0 at 4% drawdown to
# x0.25 at the 10% halt (breen/sizing.go) instead of full size until the cliff. Drawdown was 4.0%.
export SIZING_ENABLED=true
export SETTLEMENT_HOLD_PERSONAS=weather
# 2026-10-15: trailing stop. Once a position is +20% it exits after giving back 10% of its
# entry price (about +10% banked). Four gordon-default trades peaked at +22-25% and were then
# stopped at -16% to -36% (about -$915). Weather is in SETTLEMENT_HOLD_PERSONAS above, so it is
# skipped: its +14 net rests on two tails a trail would cap. Thin evidence (4 trades); revisit.
export TRAIL_ARM_PCT=20
export TRAIL_RETRACE_PCT=10

# ── 2026-09-03 forensics ────────────────────────────────────────────────────
# Bank a held weather bucket once the book BIDS it at near-certainty. Ten of the
# 38 weather trades resolved between 08-26 and 09-02 reached a 0.97+ mark and SIX
# of those ten settled at $0.00; selling at 0.95 turns that sample from +$96.63
# into +$467.30, at a cost of $10.46 to the four genuine winners.
export SETTLEMENT_HOLD_TP_PRICE=0.95
# The shadow re-activation bar also needs breadth. On 09-03 the book's 118
# actionable proposals spanned 27 distinct contracts, 65 of them one weekly Brent
# series carrying all of the profit. 0 restores the old count-and-edge bar.
export SHADOW_BAR_MIN_CONTRACTS=50

exec ./run.sh
