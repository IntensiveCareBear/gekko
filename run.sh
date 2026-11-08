#!/usr/bin/env bash
#
# run.sh — Bring up the full Gekko stack and run the frontend, then tear
#          everything down cleanly on exit (Ctrl+C).
#
#   Stateful layer (Docker) : Kafka, Redis, Qdrant
#   Core layer   (native Go): broker-worker, breen, kahuna,
#                             gordon (headless), liveview
#   Frontend     (Vite)     : React dashboard on http://localhost:5173
#
# Usage:  ./run.sh
# Stop:   press Ctrl+C — all services and containers are stopped automatically.
#
set -o pipefail

# ── Configuration ────────────────────────────────────────────────────────────
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG_DIR="$ROOT/logs"
SOCKET_PATH="${BROKER_SOCKET_PATH:-/tmp/gekko-broker.sock}"
BROKER_TOKEN="${GEKKO_BROKER_TOKEN:-gekko-dev-token}"
KAFKA_BROKERS="localhost:9092"
REDIS_ADDR="localhost:6379"
QDRANT_URL="http://localhost:6333"
FRONTEND_PORT="${FRONTEND_PORT:-5173}"
LIVEVIEW_PORT="${LIVEVIEW_PORT:-8082}"
DOCKER_SERVICES="kafka redis qdrant init-kafka"

# Lou auto-tune overrides (phase 3, paper): whitelisted knob nudges written by
# `lou -autotune`. Exported so every service below inherits them; explicit env
# on the command line (or already-exported vars) still wins. Delete the file
# (or `lou -rollback`) to run on defaults.
if [ -f "$ROOT/runs/lou/overrides.env" ]; then
  while IFS='=' read -r _lk _lv; do
    case "$_lk" in ''|'#'*) continue ;; esac
    if [ -z "$(printenv "$_lk")" ]; then
      export "$_lk=$_lv"
      echo "[lou] override: $_lk=$_lv"
    else
      echo "[lou] override skipped ($_lk already set to $(printenv "$_lk"))"
    fi
  done < "$ROOT/runs/lou/overrides.env"
fi

# Parallel arrays tracking backgrounded native-service SUPERVISORS for teardown.
BG_PIDS=()
BG_NAMES=()
CLEANED_UP=0

# Process supervision: each native service runs under a supervisor that restarts
# it (bounded) if it crashes. SHUTDOWN_FLAG tells supervisors to stop restarting.
SHUTDOWN_FLAG="${TMPDIR:-/tmp}/gekko-shutdown.$$"
MAX_RESTARTS="${MAX_RESTARTS:-5}"
RESTART_BACKOFF="${RESTART_BACKOFF:-2}"
rm -f "$SHUTDOWN_FLAG"

# Keep the Mac awake for as long as this script lives. On 2026-10-01 a 12h "overnight"
# run got 3h of uptime: the machine idle-slept and only surfaced for maintenance wakes.
command -v caffeinate >/dev/null && caffeinate -is -w $$ &

# ── Logging helpers ──────────────────────────────────────────────────────────
step()    { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()      { printf '\033[1;32m✓ %s\033[0m\n' "$*"; }
warn()    { printf '\033[1;33m⚠ %s\033[0m\n' "$*"; }
err()     { printf '\033[1;31m✗ %s\033[0m\n' "$*"; }

# ── Teardown ─────────────────────────────────────────────────────────────────
cleanup() {
  [ "$CLEANED_UP" -eq 1 ] && return
  CLEANED_UP=1
  trap - EXIT INT TERM

  step "Shutting down Gekko stack..."

  # Tell supervisors to stop restarting before we kill anything.
  touch "$SHUTDOWN_FLAG" 2>/dev/null

  # Kill the actual service processes (the supervisors are wait-ing on these).
  for cpidf in "$LOG_DIR"/.*.cpid; do
    [ -f "$cpidf" ] || continue
    cpid="$(cat "$cpidf" 2>/dev/null)"
    [ -n "$cpid" ] && kill "$cpid" 2>/dev/null
  done

  # Stop the supervisors in reverse start order.
  if [ "${#BG_PIDS[@]}" -gt 0 ]; then
    for (( i=${#BG_PIDS[@]}-1; i>=0; i-- )); do
      pid="${BG_PIDS[$i]}"
      if kill -0 "$pid" 2>/dev/null; then
        printf '  stopping %s (supervisor pid %s)\n' "${BG_NAMES[$i]}" "$pid"
        kill "$pid" 2>/dev/null
      fi
    done
    sleep 1
    # Force-kill anything still alive (supervisors + any lingering children).
    for pid in "${BG_PIDS[@]}"; do
      kill -9 "$pid" 2>/dev/null
    done
    for cpidf in "$LOG_DIR"/.*.cpid; do
      [ -f "$cpidf" ] || continue
      cpid="$(cat "$cpidf" 2>/dev/null)"
      [ -n "$cpid" ] && kill -9 "$cpid" 2>/dev/null
    done
  fi
  rm -f "$SHUTDOWN_FLAG" "$LOG_DIR"/.*.cpid 2>/dev/null

  # Remove the broker Unix socket.
  rm -f "$SOCKET_PATH" 2>/dev/null

  # Tear down the Docker stateful services.
  if command -v docker >/dev/null 2>&1; then
    printf '  docker compose down...\n'
    ( cd "$ROOT" && docker compose down --remove-orphans >/dev/null 2>&1 )
  fi

  ok "Teardown complete."
}
trap cleanup EXIT INT TERM

# ── Preflight ────────────────────────────────────────────────────────────────
require() {
  command -v "$1" >/dev/null 2>&1 || { err "Missing required tool: $1"; exit 1; }
}

wait_until() {  # timeout_secs cmd... — poll a command until it succeeds or times out
  local deadline=$(( $(date +%s) + $1 )); shift
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$deadline" ] && return 1
    sleep 2
  done
  return 0
}

step "Checking prerequisites..."
require docker
require go
require npm
require node
mkdir -p "$LOG_DIR"

if ! docker info >/dev/null 2>&1; then
  # The docker CLI is present but its daemon isn't reachable — figure out
  # what's supposed to be providing it and start that instead of just failing.
  if command -v colima >/dev/null 2>&1 && docker context show 2>/dev/null | grep -q colima; then
    warn "Docker daemon (colima) is not running — starting it..."
    colima start || { err "colima start failed. Run 'colima start' manually and retry."; exit 1; }
  elif [ -d "/Applications/Docker.app" ]; then
    warn "Docker Desktop is not running — launching it..."
    open -a Docker
    printf '  waiting for Docker to come up (up to 90s)...\n'
    wait_until 90 docker info || { err "Docker Desktop did not become ready within 90s. Start it manually and retry."; exit 1; }
  else
    err "Docker daemon is not running. Start Docker Desktop (or 'colima start') and retry."
    exit 1
  fi
fi

# Ollama is optional — full LLM proposal generation needs it, but Gordon falls
# back to rules-based proposals and keeps running without it. Try to bring it
# up on macOS (Homebrew installs the CLI only, no background service) but
# never block the stack on it.
if ! curl -s -m 2 http://localhost:11434/api/tags >/dev/null 2>&1; then
  if [ -d "/Applications/Ollama.app" ]; then
    warn "Ollama is not running — launching it..."
    open -a Ollama
  elif command -v ollama >/dev/null 2>&1; then
    warn "Ollama is not running — starting 'ollama serve' in the background..."
    nohup ollama serve >"$LOG_DIR/ollama.log" 2>&1 &
    disown
  fi
  if command -v ollama >/dev/null 2>&1 || [ -d "/Applications/Ollama.app" ]; then
    wait_until 30 curl -s -m 2 http://localhost:11434/api/tags \
      && ok "Ollama is up." \
      || warn "Ollama still not responding after 30s — Gordon will run on rules-based fallback until it is."
  else
    warn "Ollama not found — Gordon will run on rules-based fallback until it is installed and running."
  fi
fi

ok "All prerequisites present."

# ── 1. Stateful services (Docker) ────────────────────────────────────────────
step "Starting stateful services (Kafka, Redis, Qdrant)..."
( cd "$ROOT" && docker compose up -d $DOCKER_SERVICES ) || { err "docker compose up failed"; exit 1; }

step "Waiting for Kafka to become healthy..."
deadline=$(( $(date +%s) + 120 ))
until [ "$(docker inspect -f '{{.State.Health.Status}}' gekko-kafka 2>/dev/null)" = "healthy" ]; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    err "Kafka did not become healthy within 120s. See: docker compose logs kafka"
    exit 1
  fi
  printf '.'; sleep 3
done
printf '\n'; ok "Kafka healthy. Topics initialized by init-kafka."

# ── 2. Build native Go services ──────────────────────────────────────────────
build_go() {
  local dir="$1" out="$2"
  printf '  building %-22s' "$dir"
  if ( cd "$ROOT/$dir" && go build -o "$out" . ) 2>"$LOG_DIR/build-$(basename "$dir").log"; then
    printf '\033[1;32mok\033[0m\n'
  else
    printf '\033[1;31mFAILED\033[0m\n'
    err "Build failed for $dir — see $LOG_DIR/build-$(basename "$dir").log"
    exit 1
  fi
}

step "Building native Go services..."
build_go broker-worker  broker_worker_bin
build_go breen         breen_bin
build_go kahuna         kahuna_bin
build_go gordon         gordon_bin
build_go gordon/liveview liveview_bin
# Twitter/X is OFF by default: the account's API credits are depleted (HTTP 402 every
# cycle, 57 errors per 22h). Set TWITTER_ENABLED=true to go live: then a bearer token file
# (default: twitter.txt in the repo root) switches the source to live. With no token,
# kahuna's source is simulated and skipped, so no synthetic tweets reach gordon.
if [ "${TWITTER_ENABLED:-false}" = "true" ]; then
  if [ -z "${TWITTER_BEARER_TOKEN_FILE:-}" ] && [ -f "$ROOT/twitter.txt" ]; then
    TWITTER_BEARER_TOKEN_FILE="$ROOT/twitter.txt"
  fi
else
  unset TWITTER_BEARER_TOKEN_FILE TWITTER_BEARER_TOKEN
fi

# Finnhub news goes live when an API key file is present (default: finhub.txt in the
# repo root). Free tier is 60 calls/min; kahuna defaults pace well under it.
if [ -z "${FINNHUB_API_KEY_FILE:-}" ] && [ -f "$ROOT/finhub.txt" ]; then
  FINNHUB_API_KEY_FILE="$ROOT/finhub.txt"
fi

# FRED (official macro data + release calendar) goes live when an API key file is
# present (default: fred.txt in the repo root; free key from fredaccount.stlouisfed.org).
# When live, kahuna also serves the FRED-derived econ calendar at :8085/econ_calendar.json,
# so default Gordon's ECON_CALENDAR_URL there (drives its pre-release blackout window).
if [ -z "${FRED_API_KEY_FILE:-}" ] && [ -f "$ROOT/fred.txt" ]; then
  FRED_API_KEY_FILE="$ROOT/fred.txt"
fi
if [ -n "${FRED_API_KEY_FILE:-}" ] || [ -n "${FRED_API_KEY:-}" ]; then
  if [ -z "${ECON_CALENDAR_URL:-}" ] && [ -z "${ECON_CALENDAR_FILE:-}" ]; then
    ECON_CALENDAR_URL="http://localhost:8085/econ_calendar.json"
  fi
fi

# Reddit (social/tier-4) is live by default via the KEYLESS RSS path (REDDIT_RSS=true
# below) — Reddit closed self-serve API app signup in 2026, so RSS needs no account.
# If you have OAuth creds (grandfathered app, or an approved request), drop them in
# reddit.txt (one line "client_id:client_secret") to auto-upgrade to the richer API.
if [ -z "${REDDIT_CREDENTIALS_FILE:-}" ] && [ -f "$ROOT/reddit.txt" ]; then
  REDDIT_CREDENTIALS_FILE="$ROOT/reddit.txt"
fi

# Weather trading (Kalshi daily high-temperature markets) is dark by default. One
# master switch, WEATHER_ENABLED=true, turns the whole stack on: the keyless METAR
# ingest source (kahuna), high-temp market discovery (marketdata), and the gordon
# weather strategy. Individual WEATHER_SIMULATED / KALSHI_WEATHER_ENABLED overrides
# still win if set explicitly.
WEATHER_ENABLED="${WEATHER_ENABLED:-false}"
if [ "$WEATHER_ENABLED" = "true" ]; then
  WEATHER_SIMULATED="${WEATHER_SIMULATED:-false}"
else
  WEATHER_SIMULATED="${WEATHER_SIMULATED:-true}"
fi
KALSHI_WEATHER_ENABLED="${KALSHI_WEATHER_ENABLED:-$WEATHER_ENABLED}"

# Phase 2 forecast-bias model files. Absolute paths so kahuna (which writes the forward
# corpus + bias table) and the weather gordon (which reads bias.json) agree regardless
# of each process's working directory. The model self-gates: it lands dark and only
# starts predictive trading once a station has enough forward (forecast, realized) pairs.
WEATHER_FORECAST_STORE="${WEATHER_FORECAST_STORE:-$ROOT/runs/weather/forecasts.json}"
WEATHER_HISTORY_FILE="${WEATHER_HISTORY_FILE:-$ROOT/runs/weather/history.jsonl}"
WEATHER_BIAS_FILE="${WEATHER_BIAS_FILE:-$ROOT/runs/weather/bias.json}"
export WEATHER_FORECAST_STORE WEATHER_HISTORY_FILE WEATHER_BIAS_FILE

# Kalshi API key ID: read from kalshi_api_key.txt (repo root) when not already set in
# the environment, so it needn't be pasted inline. The RSA key stays in
# KALSHI_PRIVATE_KEY_FILE; this is just the access-key ID (whitespace stripped).
if [ -z "${KALSHI_API_KEY:-}" ] && [ -f "$ROOT/kalshi_api_key.txt" ]; then
  KALSHI_API_KEY="$(tr -d '[:space:]' < "$ROOT/kalshi_api_key.txt")"
fi

# RSA private key: gekko.txt in the repo root (kalshi_api_key.txt holds only the key ID).
if [ -z "${KALSHI_PRIVATE_KEY_FILE:-}" ] && [ -f "$ROOT/gekko.txt" ]; then
  KALSHI_PRIVATE_KEY_FILE="$ROOT/gekko.txt"
fi

# marketdata runs when a Kalshi key is provided (read-only Kalshi) and/or when
# Polymarket is enabled (read-only, no credentials — public Gamma + CLOB APIs).
POLYMARKET_ENABLED="${POLYMARKET_ENABLED:-true}"
if [ -n "${KALSHI_PRIVATE_KEY_FILE:-}" ] || [ "$POLYMARKET_ENABLED" != "false" ]; then
  MARKETDATA_ENABLED=1
else
  MARKETDATA_ENABLED=0
fi
if [ "$MARKETDATA_ENABLED" = "1" ]; then
  build_go marketdata marketdata_bin
fi

# ── 3. Launch native Go services ─────────────────────────────────────────────
start_native() {
  local name="$1"; shift
  local logf="$LOG_DIR/${name}.log"
  local cpidf="$LOG_DIR/.${name}.cpid"
  rm -f "$cpidf"
  # Supervisor: (re)launch the service until shutdown or the restart cap is hit.
  (
    attempts=0
    while [ ! -f "$SHUTDOWN_FLAG" ]; do
      "$@" >>"$logf" 2>&1 &
      cpid=$!
      echo "$cpid" > "$cpidf"
      wait "$cpid" 2>/dev/null
      [ -f "$SHUTDOWN_FLAG" ] && break
      attempts=$((attempts + 1))
      if [ "$attempts" -ge "$MAX_RESTARTS" ]; then
        printf '\n[run.sh] %s exited and hit the restart cap (%s). Not restarting.\n' "$name" "$MAX_RESTARTS" >>"$logf"
        break
      fi
      printf '\n[run.sh] %s exited unexpectedly — restarting (#%s) in %ss...\n' "$name" "$attempts" "$RESTART_BACKOFF" >>"$logf"
      sleep "$RESTART_BACKOFF"
    done
    rm -f "$cpidf"
  ) &
  local pid=$!
  BG_PIDS+=("$pid")
  BG_NAMES+=("$name")
  printf '  %-16s supervisor pid %-7s → %s\n' "$name" "$pid" "$logf"
}

step "Launching native Go services..."
rm -f "$SOCKET_PATH"

# Broker first — it owns the Unix socket the executive talks to.
start_native broker-worker \
  env BROKER_SOCKET_PATH="$SOCKET_PATH" GEKKO_BROKER_TOKEN="$BROKER_TOKEN" \
      HEALTH_ADDR=:8090 \
      LIVE_TRADING="${LIVE_TRADING:-false}" \
      POLYMARKET_PRIVATE_KEY_FILE="${POLYMARKET_PRIVATE_KEY_FILE:-$ROOT/secrets/polymarket_wallet.txt}" \
      POLYMARKET_API_KEY_FILE="${POLYMARKET_API_KEY_FILE:-$ROOT/secrets/polymarket_api_key.txt}" \
      POLYMARKET_API_SECRET_FILE="${POLYMARKET_API_SECRET_FILE:-$ROOT/secrets/polymarket_api_secret.txt}" \
      POLYMARKET_API_PASSPHRASE_FILE="${POLYMARKET_API_PASSPHRASE_FILE:-$ROOT/secrets/polymarket_api_passphrase.txt}" \
      "$ROOT/broker-worker/broker_worker_bin"

# Wait for the broker socket to appear (max ~5s).
for _ in $(seq 1 20); do [ -S "$SOCKET_PATH" ] && break; sleep 0.25; done
[ -S "$SOCKET_PATH" ] || warn "Broker socket not detected yet at $SOCKET_PATH (continuing)."

# Auto-enable the consensus engine when running multiple personas (they produce
# competing proposals worth consolidating). Single-Gordon runs leave it off so
# orders aren't delayed by the consensus window.
if [ -n "${GORDON_PERSONAS:-}" ]; then
  CONSENSUS_ENABLED="${CONSENSUS_ENABLED:-true}"
  # Adaptive allocation (Phase 4) is safe to auto-enable: the min-trades gate keeps
  # weights neutral until each persona has enough settled trades to learn from.
  ADAPTIVE_ALLOCATION_ENABLED="${ADAPTIVE_ALLOCATION_ENABLED:-true}"
else
  CONSENSUS_ENABLED="${CONSENSUS_ENABLED:-false}"
  ADAPTIVE_ALLOCATION_ENABLED="${ADAPTIVE_ALLOCATION_ENABLED:-false}"
fi

start_native breen \
  env EXECUTIVE_DRY_MODE=true REDIS_ADDR="$REDIS_ADDR" KAFKA_BROKERS="$KAFKA_BROKERS" \
      GEKKO_BROKER_TOKEN="$BROKER_TOKEN" BROKER_SOCKET_PATH="$SOCKET_PATH" \
      LEDGER_FILE_PATH="$ROOT/breen/ledger.db" HEALTH_ADDR=:8091 \
      STARTING_CAPITAL="${STARTING_CAPITAL:-100000}" \
      EXECUTIVE_AUTO_APPROVE="${EXECUTIVE_AUTO_APPROVE:-true}" \
      EXPIRY_MIN="${EXPIRY_MIN:-2h}" EXPIRY_MAX="${EXPIRY_MAX:-48h}" \
      SETTLE_PRIOR="${SETTLE_PRIOR:-mark}" \
      SNAPSHOT_INTERVAL="${SNAPSHOT_INTERVAL:-5m}" \
      RUN_EXPORT_DIR="${RUN_EXPORT_DIR:-$ROOT/runs}" \
      CONSENSUS_ENABLED="$CONSENSUS_ENABLED" \
      ADAPTIVE_ALLOCATION_ENABLED="$ADAPTIVE_ALLOCATION_ENABLED" \
      BOOK_FRESH_MAX_AGE="${BOOK_FRESH_MAX_AGE:-4m}" \
      MID_BAND_LOW="${MID_BAND_LOW:-0.42}" \
      MID_BAND_HIGH="${MID_BAND_HIGH:-0.58}" \
      MID_BAND_MIN_CONFIDENCE="${MID_BAND_MIN_CONFIDENCE:-0.60}" \
      CALIBRATION_ENABLED="${CALIBRATION_ENABLED:-true}" \
      "$ROOT/breen/breen_bin"

start_native kahuna \
  env KAFKA_ENABLED=true KAFKA_BROKERS="$KAFKA_BROKERS" KAFKA_TOPIC=kahuna.ingest.public \
      REDIS_ADDR="$REDIS_ADDR" \
      QUARANTINE_DIR="$ROOT/kahuna/quarantine" QUARANTINE_FALLBACK="$ROOT/kahuna/quarantine" \
      MONITOR_ADDR=:8085 \
      REUTERS_SIMULATED="${REUTERS_SIMULATED:-false}" NEWS_POLL_INTERVAL="${NEWS_POLL_INTERVAL:-60s}" \
      ${TWITTER_BEARER_TOKEN_FILE:+TWITTER_BEARER_TOKEN_FILE="$TWITTER_BEARER_TOKEN_FILE"} \
      TWITTER_DAILY_TWEET_BUDGET="${TWITTER_DAILY_TWEET_BUDGET:-1500}" \
      TWITTER_BUDGET_FILE="${TWITTER_BUDGET_FILE:-$ROOT/.twitter_budget.json}" \
      ${FINNHUB_API_KEY_FILE:+FINNHUB_API_KEY_FILE="$FINNHUB_API_KEY_FILE"} \
      FINNHUB_NEWS_CATEGORIES="${FINNHUB_NEWS_CATEGORIES:-general,forex,crypto,merger}" \
      FINNHUB_POLL_INTERVAL="${FINNHUB_POLL_INTERVAL:-60s}" \
      FINNHUB_MAX_CALLS_PER_MIN="${FINNHUB_MAX_CALLS_PER_MIN:-30}" \
      ${FRED_API_KEY_FILE:+FRED_API_KEY_FILE="$FRED_API_KEY_FILE"} \
      FRED_SERIES="${FRED_SERIES:-CPIAUCSL,CPILFESL,PCEPI,UNRATE,PAYEMS,ICSA,FEDFUNDS,DGS10,DGS2,T10Y2Y,GDPC1,VIXCLS,UMCSENT}" \
      FRED_POLL_INTERVAL="${FRED_POLL_INTERVAL:-30m}" \
      FRED_MAX_CALLS_PER_MIN="${FRED_MAX_CALLS_PER_MIN:-60}" \
      FRED_CALENDAR_DAYS="${FRED_CALENDAR_DAYS:-14}" \
      ${REDDIT_CREDENTIALS_FILE:+REDDIT_CREDENTIALS_FILE="$REDDIT_CREDENTIALS_FILE"} \
      REDDIT_RSS="${REDDIT_RSS:-true}" \
      REDDIT_SUBREDDITS="${REDDIT_SUBREDDITS:-economics,finance,cryptocurrency,wallstreetbets,stocks,politics}" \
      REDDIT_POLL_INTERVAL="${REDDIT_POLL_INTERVAL:-5m}" \
      REDDIT_MAX_RESULTS="${REDDIT_MAX_RESULTS:-25}" \
      REDDIT_MAX_CALLS_PER_MIN="${REDDIT_MAX_CALLS_PER_MIN:-15}" \
      WEATHER_SIMULATED="${WEATHER_SIMULATED:-true}" \
      ${WEATHER_STATIONS:+WEATHER_STATIONS="$WEATHER_STATIONS"} \
      WEATHER_POLL_INTERVAL="${WEATHER_POLL_INTERVAL:-10m}" \
      WEATHER_MAX_CALLS_PER_MIN="${WEATHER_MAX_CALLS_PER_MIN:-15}" \
      WEATHER_FORECAST_POLL="${WEATHER_FORECAST_POLL:-1h}" \
      WEATHER_FORECAST_STORE="$WEATHER_FORECAST_STORE" \
      WEATHER_HISTORY_FILE="$WEATHER_HISTORY_FILE" \
      WEATHER_BIAS_FILE="$WEATHER_BIAS_FILE" \
      "$ROOT/kahuna/kahuna_bin"

# Gordon runs headless (no TUI) so it can live in the background.
# Note: full proposal generation also needs Ollama on :11434 — optional; the
# worker logs errors and keeps running if Ollama is absent.
#
# LLM defaults tuned 2026-07-06: local CPU inference of qwen2.5:7b under fleet
# contention routinely blew the 30s deadline (~90% rules-fallback rate), so
# generations are serialized (1 fleet-wide slot) with a 90s per-call deadline
# and a matching 90s slot wait — fewer, real LLM answers over many timeouts.
#
# Single Gordon by default (tracks the Home model-control knob). Set
# GORDON_PERSONAS="bull,bear,gemini-bull,gemini-bear" to launch multiple non-identical
# personas — each gets its OWN Kafka consumer group (so every persona sees every event),
# health port, and Qdrant markets collection (the matcher recreates that collection on
# start, so they must not share one). That list is a clean 2×2 backend A/B: bull+bear on
# local Ollama, gemini-bull+gemini-bear on the hosted Gemini backend (the gemini-* personas
# carry Provider=gemini in the registry, so no GEMINI_PERSONAS override is needed).
launch_gordon() {  # name persona health_port consumer_group markets_collection [weather_mode=false]
  start_native "$1" \
    env HEADLESS=true KAFKA_SIMULATED=false KAFKA_BROKERS="$KAFKA_BROKERS" \
        REDIS_ADDR="$REDIS_ADDR" QDRANT_URL="$QDRANT_URL" HEALTH_ADDR=":$3" \
        GORDON_PERSONA="$2" KAFKA_CONSUMER_GROUP="$4" KALSHI_MARKETS_COLLECTION="$5" \
        GORDON_WEATHER_MODE="${6:-false}" \
        WEATHER_BIAS_FILE="${WEATHER_BIAS_FILE:-$ROOT/runs/weather/bias.json}" \
        ${WEATHER_PREDICTIVE:+WEATHER_PREDICTIVE="$WEATHER_PREDICTIVE"} \
        PORTFOLIO_MAX_EXP="${PORTFOLIO_MAX_EXP:-8000}" PORTFOLIO_MAX_SIZE="${PORTFOLIO_MAX_SIZE:-2500}" \
        OLLAMA_MODEL="${OLLAMA_MODEL:-qwen2.5:32b}" CONSENSUS_ENABLED="$CONSENSUS_ENABLED" \
        OLLAMA_PROPOSAL_TIMEOUT="${OLLAMA_PROPOSAL_TIMEOUT:-180s}" \
        OLLAMA_MAX_CONCURRENT="${OLLAMA_MAX_CONCURRENT:-1}" RAG_TOPK="${RAG_TOPK:-3}" \
        OLLAMA_ACQUIRE_TIMEOUT="${OLLAMA_ACQUIRE_TIMEOUT:-180s}" \
        GORDON_FLEET_RATE="${GORDON_FLEET_RATE:-0.2}" \
        GEMINI_API_KEY_FILE="${GEMINI_API_KEY_FILE:-$ROOT/GEMINI_API_KEY.txt}" \
        ${GEMINI_PERSONAS:+GEMINI_PERSONAS="$GEMINI_PERSONAS"} \
        ${GEMINI_MODEL:+GEMINI_MODEL="$GEMINI_MODEL"} \
        GEMINI_MAX_RPM="${GEMINI_MAX_RPM:-20}" \
        GEMINI_MAX_RPD="${GEMINI_MAX_RPD:-2000}" \
        ${ECON_CALENDAR_FILE:+ECON_CALENDAR_FILE="$ECON_CALENDAR_FILE"} \
        ${ECON_CALENDAR_URL:+ECON_CALENDAR_URL="$ECON_CALENDAR_URL"} \
        ${ECON_BLACKOUT_WINDOW:+ECON_BLACKOUT_WINDOW="$ECON_BLACKOUT_WINDOW"} \
        ${NEWS_SENTIMENT_URL:+NEWS_SENTIMENT_URL="$NEWS_SENTIMENT_URL"} \
        LOU_LESSONS_FILE="${LOU_LESSONS_FILE:-$ROOT/runs/lou/lessons.json}" \
        "$ROOT/gordon/gordon_bin"
}

# GORDON_HEALTH_URLS accumulates every persona's health endpoint so liveview can
# aggregate proposal counts across all of them (not just the first).
if [ -z "${GORDON_PERSONAS:-}" ]; then
  launch_gordon gordon gordon-default 8092 gordon-group kalshi_markets
  GORDON_HEALTH_URLS="http://localhost:8092/healthz"
else
  gidx=0
  GORDON_HEALTH_URLS=""
  IFS=',' read -ra _personas <<< "$GORDON_PERSONAS"
  for p in "${_personas[@]}"; do
    p="$(echo "$p" | xargs)"; [ -z "$p" ] && continue
    # First persona keeps :8092 so liveview's status page still finds a Gordon.
    if [ "$gidx" -eq 0 ]; then gport=8092; else gport=$((8100 + gidx)); fi
    launch_gordon "gordon-$p" "$p" "$gport" "gordon-$p" "kalshi_markets_$p"
    GORDON_HEALTH_URLS="${GORDON_HEALTH_URLS:+$GORDON_HEALTH_URLS,}http://localhost:$gport/healthz"
    gidx=$((gidx + 1))
  done
fi
# Dedicated deterministic weather trader: consumes only WEATHER observations and
# prices cleared Kalshi high-temp buckets (no LLM). Launched alongside the LLM fleet
# when the weather stack is on.
if [ "$WEATHER_ENABLED" = "true" ]; then
  launch_gordon gordon-weather weather 8097 gordon-weather kalshi_markets_weather true
  GORDON_HEALTH_URLS="${GORDON_HEALTH_URLS:+$GORDON_HEALTH_URLS,}http://localhost:8097/healthz"
fi
export GORDON_HEALTH_URLS

# Liveview exposes the /api + SSE endpoints the frontend proxies to on :8082,
# and aggregates every component's /healthz at /api/status for the status page.
start_native liveview \
  env KAFKA_BROKERS="$KAFKA_BROKERS" KAFKA_OUTBOUND_TOPIC=gordon.strategies.proposed \
      REDIS_ADDR="$REDIS_ADDR" PORT="$LIVEVIEW_PORT" LOG_DIR="$LOG_DIR" LEDGER_FILE_PATH="$ROOT/breen/ledger.db" \
      BROKER_HEALTH_URL=http://localhost:8090/healthz \
      EXECUTIVE_HEALTH_URL=http://localhost:8091/healthz \
      KAHUNA_HEALTH_URL=http://localhost:8085/healthz \
      GORDON_HEALTH_URL=http://localhost:8092/healthz \
      GORDON_HEALTH_URLS="$GORDON_HEALTH_URLS" \
      OLLAMA_URL=http://localhost:11434 OLLAMA_CHAT_MODEL="${OLLAMA_CHAT_MODEL:-qwen2.5:7b}" \
      RUN_EXPORT_DIR="${RUN_EXPORT_DIR:-$ROOT/runs}" \
      MARKETDATA_HEALTH_URL="$([ "$MARKETDATA_ENABLED" = "1" ] && echo http://localhost:8093/healthz)" \
      "$ROOT/gordon/liveview/liveview_bin"

# Read-only market-data poller: Kalshi (when a key file is provided) and/or
# Polymarket (public APIs, no credentials). Both write to the same Redis keys.
if [ "$MARKETDATA_ENABLED" = "1" ]; then
  start_native marketdata \
    env KALSHI_API_BASE="${KALSHI_API_BASE:-https://api.elections.kalshi.com/trade-api/v2}" \
        KALSHI_API_KEY="${KALSHI_API_KEY:-}" \
        ${KALSHI_PRIVATE_KEY_FILE:+KALSHI_PRIVATE_KEY_FILE="$KALSHI_PRIVATE_KEY_FILE"} \
        KALSHI_WATCH_TICKERS="${KALSHI_WATCH_TICKERS:-}" \
        REDIS_ADDR="$REDIS_ADDR" KALSHI_POLL_INTERVAL="${KALSHI_POLL_INTERVAL:-5s}" \
        KALSHI_RATE_LIMIT="${KALSHI_RATE_LIMIT:-8}" HEALTH_ADDR=:8093 \
        ORDERBOOK_TTL="${ORDERBOOK_TTL:-8m}" \
        KALSHI_DISCOVERY_INTERVAL="${KALSHI_DISCOVERY_INTERVAL:-5m}" \
        KALSHI_MAX_MARKETS="${KALSHI_MAX_MARKETS:-800}" \
        KALSHI_MAX_SERIES="${KALSHI_MAX_SERIES:-200}" \
        KALSHI_CATEGORIES="${KALSHI_CATEGORIES:-Crypto,Economics,Financials,Commodities}" \
        KALSHI_WEATHER_ENABLED="$KALSHI_WEATHER_ENABLED" \
        ${KALSHI_SERIES:+KALSHI_SERIES="$KALSHI_SERIES"} \
        POLYMARKET_ENABLED="$POLYMARKET_ENABLED" \
        POLYMARKET_GAMMA_URL="${POLYMARKET_GAMMA_URL:-https://gamma-api.polymarket.com}" \
        POLYMARKET_CLOB_URL="${POLYMARKET_CLOB_URL:-https://clob.polymarket.com}" \
        POLYMARKET_POLL_INTERVAL="${POLYMARKET_POLL_INTERVAL:-5s}" \
        POLYMARKET_RATE_LIMIT="${POLYMARKET_RATE_LIMIT:-8}" \
        POLYMARKET_DISCOVERY_INTERVAL="${POLYMARKET_DISCOVERY_INTERVAL:-5m}" \
        POLYMARKET_MAX_MARKETS="${POLYMARKET_MAX_MARKETS:-150}" \
        "$ROOT/marketdata/marketdata_bin"
else
  printf '  %-16s %s\n' "marketdata" "skipped (set KALSHI_PRIVATE_KEY_FILE or POLYMARKET_ENABLED=true)"
fi

# Give services a moment, then report any that exited immediately.
sleep 2
for i in "${!BG_PIDS[@]}"; do
  kill -0 "${BG_PIDS[$i]}" 2>/dev/null || \
    warn "${BG_NAMES[$i]} exited early — see $LOG_DIR/${BG_NAMES[$i]}.log"
done

# ── 4. Frontend dependencies ─────────────────────────────────────────────────
step "Preparing frontend..."
if [ ! -d "$ROOT/frontend/node_modules" ]; then
  printf '  installing npm dependencies (first run)...\n'
  ( cd "$ROOT/frontend" && npm install ) || { err "npm install failed"; exit 1; }
fi

# ── Summary ──────────────────────────────────────────────────────────────────
printf '\n\033[1;32m─────────────────────────────────────────────\033[0m\n'
printf '\033[1;32m Gekko stack is UP\033[0m\n'
printf '\033[1;32m─────────────────────────────────────────────\033[0m\n'
printf '  Frontend       : http://localhost:%s   (System Status tab → live health)\n' "$FRONTEND_PORT"
printf '  Liveview API   : http://localhost:%s/api/stream\n' "$LIVEVIEW_PORT"
printf '  Status (JSON)  : http://localhost:%s/api/status\n' "$LIVEVIEW_PORT"
printf '  Health checks  : broker :8090  exec :8091  kahuna :8085  gordon :8092  liveview :%s  (/healthz)\n' "$LIVEVIEW_PORT"
printf '  Kahuna monitor : http://localhost:8085\n'
printf '  Qdrant UI      : http://localhost:6333/dashboard\n'
printf '  Assistant tab  : needs Ollama on :11434 + a chat model (gordon/setup_models.sh)\n'
printf '  Paper run      : $%s, auto-approve=%s, expiry %s–%s — watch it in Portfolio Analytics\n' \
  "${STARTING_CAPITAL:-100000}" "${EXECUTIVE_AUTO_APPROVE:-true}" "${EXPIRY_MIN:-2h}" "${EXPIRY_MAX:-48h}"
printf '  Run exports    : %s (download from System tab)\n' "${RUN_EXPORT_DIR:-$ROOT/runs}"
if [ -n "${KALSHI_PRIVATE_KEY_FILE:-}" ]; then
  printf '  Kalshi data    : LIVE read-only on :8093 — auto-discovering top %s markets; Gordon trades matched real tickers\n' "${KALSHI_MAX_MARKETS:-150}"
else
  printf '  Kalshi data    : simulated (set KALSHI_PRIVATE_KEY_FILE + KALSHI_API_KEY, or drop the key ID in kalshi_api_key.txt, for live)\n'
fi
if [ "$POLYMARKET_ENABLED" != "false" ]; then
  printf '  Polymarket data: LIVE read-only on :8093 — public Gamma+CLOB, top %s markets; Gordon trades matched POLY- tickers\n' "${POLYMARKET_MAX_MARKETS:-150}"
else
  printf '  Polymarket data: disabled (set POLYMARKET_ENABLED=true for live read-only data; no credentials needed)\n'
fi
if [ -n "${TWITTER_BEARER_TOKEN_FILE:-}" ] || [ -n "${TWITTER_BEARER_TOKEN:-}" ]; then
  printf '  Twitter/X data : LIVE — X API v2 recent search (needs Basic tier+; tune TWITTER_QUERY / TWITTER_POLL_INTERVAL)\n'
else
  printf '  Twitter/X data : disabled (set TWITTER_ENABLED=true, with twitter.txt or TWITTER_BEARER_TOKEN, for live)\n'
fi
if [ -n "${FINNHUB_API_KEY_FILE:-}" ] || [ -n "${FINNHUB_API_KEY:-}" ]; then
  printf '  Finnhub data   : LIVE — market news (%s), ≤%s calls/min (free tier is 60/min)\n' "${FINNHUB_NEWS_CATEGORIES:-general,forex,crypto,merger}" "${FINNHUB_MAX_CALLS_PER_MIN:-30}"
else
  printf '  Finnhub data   : disabled (add finhub.txt with a Finnhub API key, or set FINNHUB_API_KEY, for live news)\n'
fi
if [ -n "${FRED_API_KEY_FILE:-}" ] || [ -n "${FRED_API_KEY:-}" ]; then
  printf '  FRED data      : LIVE — macro series (%s) + econ calendar at :8085/econ_calendar.json\n' "${FRED_SERIES:-CPIAUCSL,CPILFESL,PCEPI,UNRATE,PAYEMS,ICSA,FEDFUNDS,DGS10,DGS2,T10Y2Y,GDPC1,VIXCLS,UMCSENT}"
else
  printf '  FRED data      : disabled (add fred.txt with a free FRED API key, or set FRED_API_KEY, for macro data + econ calendar)\n'
fi
if [ -n "${REDDIT_CREDENTIALS_FILE:-}" ] || { [ -n "${REDDIT_CLIENT_ID:-}" ] && [ -n "${REDDIT_CLIENT_SECRET:-}" ]; }; then
  printf '  Reddit data    : LIVE (OAuth API) — subreddits (%s) — tier-4/social, needs credibility weighting\n' "${REDDIT_SUBREDDITS:-economics,finance,cryptocurrency,wallstreetbets,stocks,politics}"
elif [ "${REDDIT_RSS:-true}" = "true" ]; then
  printf '  Reddit data    : LIVE (keyless RSS) — subreddits (%s); no score/comment metadata. Add reddit.txt creds to upgrade to the API — tier-4/social, needs credibility weighting\n' "${REDDIT_SUBREDDITS:-economics,finance,cryptocurrency,wallstreetbets,stocks,politics}"
else
  printf '  Reddit data    : disabled (set REDDIT_RSS=true for keyless feeds, or add reddit.txt OAuth creds)\n'
fi
if [ "${WEATHER_SIMULATED:-true}" = "false" ]; then
  printf '  Weather data   : LIVE (keyless METAR) — stations (%s) for Kalshi high-temp markets\n' "${WEATHER_STATIONS:-default liquid set}"
else
  printf '  Weather data   : disabled (set WEATHER_SIMULATED=false to poll aviationweather.gov METAR for high-temp markets)\n'
fi
printf '  Service logs   : %s/\n' "$LOG_DIR"
warn "Press Ctrl+C to stop everything and tear down cleanly."

# ── 5. Run the frontend in the foreground ────────────────────────────────────
# Vite holds the terminal; when you Ctrl+C, the EXIT/INT trap runs cleanup().
step "Starting Vite dev server..."
( cd "$ROOT/frontend" && npm run dev -- --port "$FRONTEND_PORT" )

# If Vite exits on its own, cleanup() still runs via the EXIT trap.
