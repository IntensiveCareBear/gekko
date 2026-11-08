# Proposal: "Lou" — Gekko's Daily Learning Loop

_High-level design. Codename **Lou** (after Lou Mannheim, the mentor in_ Wall Street _who dispenses the lessons). Rename freely._

## The idea in one line

Every ~24 hours, Lou reads the books and Gordon's decisions from the day, distills a small number of durable **lessons**, persists them, and feeds the highest-confidence ones back into Gekko — so the bot is measurably a little smarter each day instead of relying on a human to run the post-mortem.

## Why

The `gekko_improvement_tasks.md` file _is_ this loop, done by hand. After each run someone reads the trades, spots a pattern ("losses concentrate in the 0.50–0.65 entry band," "strike-ladder evasion dodged the loss limit"), and turns it into a fix. That process is slow, irregular, and depends on a person being in the loop. Lou automates the recurring 80%: notice the pattern, write it down, and — within tight guardrails — act on it.

## What Lou looks at

Two sources, matching where the signal actually lives:

- **The trade & P&L books** — the `runs/` archives (`trades.jsonl`, `equity.jsonl`, `run_summary.txt`): what was entered, at what price, how it resolved, what it made or lost, drawdown, win rate, per-family concentration.
- **Gordon's decisions** — the proposals and their rationale (model used, confidence, entry band, momentum/sentiment inputs) matched against outcomes: which _reasoning_ made money and which didn't. This is where entry-quality lessons come from, which the improvement log flagged as the remaining lever.

Operational health (LLM fallback ratios, timeouts, slippage) is explicitly **out of scope** for v1 — Lou is about trading judgment, not reliability.

## What a "lesson" is

A lesson is a small, structured, evidence-backed claim — not a vibe. Conceptually each one carries:

- a **pattern** ("BUYs at entry price ≥ 0.65 in the macro category revert within 6h"),
- the **evidence** that supports it (trade count, win rate, PnL, the runs it came from),
- a **confidence** and a **sample size**,
- an optional **suggested action** (e.g. "tighten `MAX_ENTRY_PRICE` from 0.70 → 0.60"), and
- a **lifecycle**: proposed → validated → active → (later) decayed or retired if the edge stops showing up.

Lessons are cumulative and versioned. A new run can strengthen, weaken, or contradict an existing lesson rather than starting from a blank page each day.

## Where lessons live

Gekko already runs **qdrant** (a vector store) and Redis. Lou persists lessons to a durable store (the natural home is a `lessons` collection in qdrant, so they're both queryable by similarity for RAG _and_ auditable as a plain ledger). The key property: lessons outlive any single run and accumulate — that persistence is what "smarter every day" actually means.

## How a lesson changes Gekko's behavior

You chose the **auto-tune** path, so the loop closes without a human in it — but deliberately narrowly and safely:

1. **Extract** — Lou analyzes the day's books + decisions and drafts candidate lessons.
2. **Validate** — each candidate is checked against a minimum sample size and, where possible, back-checked against prior runs. Weak or contradicted candidates are shelved, not applied.
3. **Act, within a whitelist** — a validated lesson may adjust only **pre-approved, bounded config knobs** (the env-based risk/sizing params like `MAX_ENTRY_PRICE`, `entrySkewSizeMult`, `PER_CONTRACT_LOSS_LIMIT`, category-exposure caps). Each knob has a hard min/max and a max step per day. Lou can nudge; it cannot rewrite trading logic, touch code, or invent new parameters.
4. **Feed context** — lessons also flow back into Gordon's RAG context so the LLM sees "here's what burned us before" at proposal time — an influence path that doesn't require changing any number.

The config change and the RAG injection are complementary: one adjusts the guardrails, the other improves the judgment inside them.

## Guardrails (the important part, given auto-tune)

Auto-tuning a system that moves money needs brakes:

- **Whitelist + bounds** — Lou can only move knobs on an allowlist, each clamped to a safe range with a capped daily step. No knob, no change.
- **Every change is logged and reversible** — each adjustment records the lesson, the evidence, the old value, and the new value. One-command rollback; full audit trail.
- **Paper-first** — auto-tune runs against paper trading until it has a track record. Promotion to live is a separate, explicit gate.
- **Sample-size floors** — no action on thin evidence; the improvement log already shows how a handful of trades can mislead.
- **Circuit breaker** — if lessons applied over the last N days correlate with _worse_ performance, Lou pauses auto-tune and reverts to advisory (write-only) mode.
- **Human digest** — every cycle emits a short "what I learned and what I changed" summary you can read in one minute.

## Cadence

A scheduled daily pass (24h is a sensible default; could also trigger at the end of each archived run). Cheap enough to run more often once trusted.

## How it fits the existing architecture

Lou is a **new peripheral component**, not a rewrite. It reads artifacts the system already produces (`runs/`, Gordon's proposal logs), writes to stores that already exist (qdrant, Redis), and influences behavior only through Gordon's context and breen's existing config surface. It sits alongside `gordon`/`breen`/`kahuna` rather than inside them, which keeps the trading hot-path untouched and lets Lou fail safe (if Lou is down, Gekko trades exactly as it does today).

## Suggested phased rollout

1. **Advisory** — Lou extracts and persists lessons and emits the daily digest. Zero write-back. Builds trust and a lesson corpus.
2. **RAG feedback** — lessons flow into Gordon's context. Still no config changes.
3. **Auto-tune (paper)** — whitelist knob-nudging on paper trading, fully logged and reversible.
4. **Auto-tune (live)** — explicit promotion once phase 3 shows the loop helps.

## Open questions

- **What's on the whitelist for v1?** Start with the two or three knobs the improvement log already proved matter (entry-price ceiling, skew sizing, loss limit)?
- **What counts as "validated"** — a fixed sample-size floor, or a lightweight backtest against prior `runs/`?
- **Lesson decay** — how fast should a lesson lose weight if recent runs stop confirming it?
- **Who arbitrates conflicts** between a new lesson and an active one — newest wins, highest-evidence wins, or flag for human review?

---

_Next step, on your nod: turn this into a concrete v1 spec — the lesson schema, the knob whitelist with bounds, and the digest format._
