#!/bin/sh
# Push gekko P&L metrics to a TRMNL private-plugin webhook.
#
# Pulls the pre-formatted payload from liveview's /api/trmnl and POSTs it
# straight to TRMNL. Gekko stays local; only this outbound HTTPS call leaves
# the box. Intended to run from cron every ~15 min (TRMNL free-tier refresh).
#
# Config (env overrides, otherwise defaults):
#   LIVEVIEW_URL   base URL of the liveview API      (default http://localhost:8082)
#   TRMNL_UUID     private-plugin UUID               (default: read trmnl_plugin_uuid.txt)
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
LIVEVIEW_URL="${LIVEVIEW_URL:-http://localhost:8082}"

# UUID from env or the untracked file (same convention as kalshi_api_key.txt etc.).
UUID="${TRMNL_UUID:-}"
if [ -z "$UUID" ]; then
  UUID_FILE="$ROOT/trmnl_plugin_uuid.txt"
  if [ ! -f "$UUID_FILE" ]; then
    echo "trmnl_push: no TRMNL_UUID env and $UUID_FILE missing" >&2
    exit 1
  fi
  UUID="$(tr -d ' \t\r\n' < "$UUID_FILE")"
fi

# 1. Pull metrics from the local endpoint.
BODY="$(curl -fsS --max-time 10 "$LIVEVIEW_URL/api/trmnl")" || {
  echo "trmnl_push: failed to reach $LIVEVIEW_URL/api/trmnl" >&2
  exit 1
}

# Guard against pushing an empty/HTML fallback (e.g. old binary without the route).
case "$BODY" in
  *merge_variables*) : ;;
  *) echo "trmnl_push: unexpected /api/trmnl response, not pushing" >&2; exit 1 ;;
esac

# 2. Forward it verbatim to TRMNL.
curl -fsS --max-time 10 -X POST \
  -H 'Content-Type: application/json' \
  --data "$BODY" \
  "https://usetrmnl.com/api/custom_plugins/$UUID" >/dev/null || {
  echo "trmnl_push: POST to TRMNL failed" >&2
  exit 1
}
