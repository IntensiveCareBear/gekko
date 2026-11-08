#!/usr/bin/env bash
#
# nightly.sh — rebuild Lou and run one learning pass from the repo root.
# Invoked by the launchd agent com.gekko.lou.nightly (daily at 01:00), but also
# runnable by hand:  ./lou/nightly.sh
#
# Output (build + digest) is appended to logs/lou-nightly.log.
set -o pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG="$ROOT/logs/lou-nightly.log"

# launchd starts with a minimal PATH; make sure Go (Homebrew) is reachable.
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

mkdir -p "$ROOT/logs"

{
  echo "===== lou nightly $(date '+%Y-%m-%d %H:%M:%S %z') ====="
  cd "$ROOT/lou" || exit 1
  if ! go build -o lou . ; then
    echo "[ERROR] build failed — skipping run"
    exit 1
  fi
  cd "$ROOT" || exit 1
  ./lou/lou -autotune
  echo "===== lou nightly done (exit $?) ====="
} >>"$LOG" 2>&1
