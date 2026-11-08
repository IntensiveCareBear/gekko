# Gekko

An event-driven, LLM-assisted trading system for prediction markets (Kalshi and Polymarket).
News, macro data and weather observations are ingested, matched to live markets, priced by
LLM "personas", and run through a risk/sizing engine. It ships as a **paper-trading** stack.

> **Not financial advice.** This is research software. It runs in dry (paper) mode by default.
> Trading real money with it can lose real money; read
> [docs/kalshi-live-execution-checklist.md](docs/kalshi-live-execution-checklist.md) before
> considering it.

## Architecture

```
 kahuna ──► Kafka ──► gordon (1..N personas) ──► Kafka ──► breen ──► broker-worker
 (ingest)  kahuna.     (match market, RAG,      gordon.    (risk,     (Kalshi/Polymarket,
            ingest.     LLM pricing, edge)       strategies sizing,    unix socket)
            public                               .proposed  ledger)
                           ▲                                  │
 marketdata ──► Redis ─────┴──────────────────────────────────┘
 (Kalshi/Polymarket books)        Qdrant (RAG)   liveview API ──► React frontend
```

| Service | Dir | Port | Role |
|---|---|---|---|
| kahuna | `kahuna/` | 8085 | Ingests news (Finnhub, Reuters RSS, Reddit, X), FRED macro, METAR weather → Kafka |
| marketdata | `marketdata/` | 8093 | Polls Kalshi/Polymarket order books into Redis |
| gordon | `gordon/` | 8092 (+8101…) | Matches events to markets, RAG over Qdrant, LLM (Ollama or Gemini) proposals |
| breen | `breen/` | 8091 | Executive: consensus, risk gates, sizing, settlement, hash-chained ledger |
| broker-worker | `broker-worker/` | 8090 | Order gateway over a Unix socket; only loads exchange secrets when `LIVE_TRADING=true` |
| liveview | `gordon/liveview/` | 8082 | JSON/SSE API and health aggregation for the UI |
| frontend | `frontend/` | 5173 | React + Vite dashboard |
| lou | `lou/` | — | Offline learning loop: analyzes exported runs, proposes/applies knob tweaks |
| Kafka / Redis / Qdrant | `docker-compose.yml` | 9092 / 6379 / 6333 | Stateful layer (Docker) |

## Requirements

- macOS or Linux
- Docker (Docker Desktop or colima) with `docker compose`
- Go 1.25+ (one module needs 1.26 — use the latest Go)
- Node.js 20+ and npm
- Optional: [Ollama](https://ollama.com) for local LLMs (without it Gordon falls back to rules-based proposals)

## Quick start (paper trading)

```bash
git clone https://github.com/IntensiveCareBear/gekko.git
cd gekko
./gordon/setup_models.sh   # optional: pulls the Ollama models (large downloads)
./run.sh
```

`run.sh` starts Kafka/Redis/Qdrant in Docker, builds and supervises every Go service
(auto-restart, logs in `logs/`), then runs the frontend in the foreground. **Ctrl+C tears
everything down**, including the containers.

Then open:

- Dashboard: http://localhost:5173
- Stack status (JSON): http://localhost:8082/api/status
- Kahuna feed monitor: http://localhost:8085
- Qdrant UI: http://localhost:6333/dashboard

With no API keys, Polymarket market data (public APIs) and keyless Reddit RSS are live;
everything else is simulated or disabled. The startup summary prints which sources are live.

## Credentials

All credentials are read from files that are **gitignored** — never commit them. Every file
path can also be overridden by the matching `*_FILE` env var, and most keys can be passed as a
plain env var instead.

| Source | File (repo root) | Env override | Needed for |
|---|---|---|---|
| Kalshi key ID | `kalshi_api_key.txt` | `KALSHI_API_KEY` | Live Kalshi market data |
| Kalshi RSA private key | `gekko.txt` | `KALSHI_PRIVATE_KEY_FILE` | Live Kalshi market data |
| Finnhub | `finhub.txt` | `FINNHUB_API_KEY_FILE` | News |
| FRED | `fred.txt` | `FRED_API_KEY_FILE` | Macro series + econ calendar blackout |
| Gemini | `GEMINI_API_KEY.txt` | `GEMINI_API_KEY_FILE` | `gemini-*` personas |
| X / Twitter bearer | `twitter.txt` | `TWITTER_BEARER_TOKEN_FILE` | Only with `TWITTER_ENABLED=true` |
| Reddit OAuth | `reddit.txt` (`client_id:client_secret`) | `REDDIT_CREDENTIALS_FILE` | Optional upgrade from RSS |
| Polymarket wallet + API creds | `secrets/polymarket_*.txt` | `POLYMARKET_*_FILE` | Live trading only |

`chmod 600` these files. The broker socket is authenticated with `GEKKO_BROKER_TOKEN`
(`run.sh` defaults to a dev token; set your own if anything else can reach the socket).

## Configuration

Everything is configured through environment variables; `run.sh` documents each one inline
and passes `${VAR:-default}`, so you override by exporting before launch:

```bash
STARTING_CAPITAL=50000 GORDON_PERSONAS="bull,bear" WEATHER_ENABLED=true ./run.sh
```

Commonly used:

| Variable | Default | Effect |
|---|---|---|
| `STARTING_CAPITAL` | `100000` | Paper bankroll |
| `GORDON_PERSONAS` | unset (one gordon) | Comma list of personas; each gets its own consumer group, health port and Qdrant collection, and enables consensus |
| `OLLAMA_MODEL` | `qwen2.5:32b` | Proposal model (`OLLAMA_CHAT_MODEL` for the assistant tab) |
| `WEATHER_ENABLED` | `false` | Turns on METAR ingest, Kalshi high-temp market discovery and the deterministic weather trader |
| `POLYMARKET_ENABLED` | `true` | Read-only Polymarket data |
| `KALSHI_CATEGORIES` | `Crypto,Economics,Financials,Commodities` | Kalshi market discovery scope |
| `EXPIRY_MIN` / `EXPIRY_MAX` | `2h` / `48h` | Only trade markets settling in this window |
| `PORTFOLIO_MAX_EXP` / `PORTFOLIO_MAX_SIZE` | `8000` / `2500` | Gordon-side exposure caps |

[scripts/relaunch.sh](scripts/relaunch.sh) is a complete, tuned example of an override set.

## Deploying on a server

The stack is designed to run on one always-on host (a Mac mini or a Linux box):

1. Install the requirements above, clone the repo, and drop credential files in the repo root.
2. Run it in a persistent session, e.g. `tmux new -s gekko ./run.sh` (or `./scripts/relaunch.sh`
   for your tuned settings). On macOS `run.sh` holds a `caffeinate` assertion so the machine
   doesn't idle-sleep.
3. State that must survive restarts:
   - `breen/ledger.db` — the append-only, hash-chained trade ledger
   - Docker volume `gekko_redis_data` — account, positions, equity (AOF-persisted)
   - `qdrant_storage/` — RAG vectors
   - `runs/` — exported paper runs, Lou lessons and overrides

   Back these up; all are gitignored.
4. Don't expose the service ports publicly. None of the HTTP services have authentication;
   reach the dashboard over SSH port-forwarding or a VPN:
   ```bash
   ssh -L 5173:localhost:5173 -L 8082:localhost:8082 user@your-host
   ```

### Optional pieces

- **Containerized UI**: after `./run.sh` is up, `docker compose -f docker-compose.liveview.yml up -d --build`
  serves liveview behind nginx on :8080 and a production frontend build on :8081.
- **Lou nightly learning pass**: schedule [lou/nightly.sh](lou/nightly.sh) daily (cron or launchd).
  `lou -autotune` writes `runs/lou/overrides.env`, which `run.sh` loads on the next launch;
  `lou -rollback` removes it.
- **Terminal HUD**: `python3 scripts/gekko-monitor.py` (reads the liveview API on :8082).
- **TRMNL e-ink dashboard**: see [docs/trmnl-plugin.md](docs/trmnl-plugin.md).

## Live trading

`run.sh` always starts breen with `EXECUTIVE_DRY_MODE=true`, and broker-worker refuses to load
exchange secrets unless `LIVE_TRADING=true`. Going live is a deliberate code/config change;
work through [docs/kalshi-live-execution-checklist.md](docs/kalshi-live-execution-checklist.md) first.

## Development

Each service is its own Go module:

```bash
for d in breen broker-worker gordon kahuna lou marketdata; do (cd $d && go test ./...); done
cd frontend && npm install && npm run build
```

Design notes, proposals and post-mortems of past paper runs live in [docs/](docs/).

## License

[MIT](LICENSE)
