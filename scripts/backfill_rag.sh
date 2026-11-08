#!/usr/bin/env bash
# One-shot: replay archived closed trades (runs/*/trades.jsonl + runs/trades.jsonl)
# onto gekko:rag_feedback so the running gordon consumer embeds them into Qdrant.
# Run ONCE per fresh collection — re-running duplicates points.
# Usage: scripts/backfill_rag.sh [--skip-dry]   (REDIS_CLI="docker exec -i <ctr> redis-cli" to override)
set -euo pipefail
cd "$(dirname "$0")/.."
REDIS_CLI=${REDIS_CLI:-redis-cli}
SKIP_DRY=0; [[ "${1:-}" == "--skip-dry" ]] && SKIP_DRY=1
cat runs/archive-*/trades.jsonl runs/trades.jsonl 2>/dev/null | sort -u | SKIP_DRY=$SKIP_DRY python3 -c '
import sys, json, os
skip = os.environ["SKIP_DRY"] == "1"
n = 0
for line in sys.stdin:
    try: t = json.loads(line)
    except ValueError: continue
    if skip and t.get("status") == "DRY_CLOSED": continue
    cid, ent, ex, ret = t["contract_id"], t["entry_price"], t["exit_price"], t["return_pct"]
    side = t.get("side")
    closing = f"resolved {side} at {ex:.2f}" if side in ("YES", "NO") else f"sold at {ex:.2f}"
    fb = json.dumps({"contract_id": cid, "entry_price": ent, "exit_price": ex, "return_pct": ret,
        "action": "BUY", "outcome": "WIN" if ret > 0 else "LOSS",
        "text": f"Bought {cid} at {ent:.2f}, {closing} — {ret:+.0f}% return"})
    sys.stdout.write(f"*3\r\n$5\r\nLPUSH\r\n$18\r\ngekko:rag_feedback\r\n${len(fb.encode())}\r\n{fb}\r\n")
    n += 1
sys.stderr.write(f"queued {n} outcomes\n")
' | $REDIS_CLI --pipe
