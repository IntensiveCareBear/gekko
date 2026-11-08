# Gekko Systems: Hybrid Architecture Startup Manual

This manual describes how to launch the entire Gekko trading and telemetry stack in a hybrid execution model:
1. **Stateful Layer** (Kafka, Redis, Qdrant) runs inside Docker containers.
2. **Core Layer** (Broker Worker, Executive Core, Kahuna, Gordon, Liveview) runs natively on macOS as compiled Go binaries.
3. **Frontend Layer** (React / Tailwind UI) runs via Vite's development server.

---

## Architecture Port Map

| Component | Service Type | Host Address / Port | Description |
| :--- | :--- | :--- | :--- |
| **Kafka** | Docker Container | `localhost:9092` | Core message broker for event streaming |
| **Redis** | Docker Container | `localhost:6379` | Simulation state store and distributed locks |
| **Qdrant** | Docker Container | `localhost:6333` | Vector database for RAG context embeddings |
| **Broker Worker** | Mac Native | `unix:///path/to/gekko/broker.sock` | IPC exchange order execution gateway |
| **Executive Core** | Mac Native | Terminal Logs | Simulates dry-runs or routes wet trades to broker |
| **Kahuna Ingester** | Mac Native | `localhost:8085` | Ingests market channels and streams raw JSON to Kafka |
| **Gordon Engine** | Mac Native | BubbleTea TUI Console | Evaluates liquidity, checks risk limits, posts signals |
| **Liveview Server** | Mac Native | `localhost:8082` | Exposes proposals JSON API and SSE events stream |
| **React Dashboard** | Vite Dev Server | `localhost:5173` | Rich interactive React UI with glossary hover cards |

---

## Step 1: Spin Up the Stateful Stack (Docker)

Launch the message queue, caching service, and vector storage containers. This also triggers the `init-kafka` service to create Gekko's telemetry topics.

From the repository root (`/path/to/gekko`), run:
```bash
docker compose up -d kafka redis qdrant init-kafka
```

Confirm the containers are running and healthy:
```bash
docker compose ps
```

---

## Step 2: Build & Run Go Services Natively (macOS)

Open separate terminal tabs or windows for each of the core Go services.

### 1. Exchange Broker Worker
The Broker acts as the gatekeeper for exchange APIs (Polymarket & Kalshi) via Unix domain sockets.

Build and run:
```bash
cd broker-worker
go build -o broker_worker_bin
BROKER_SOCKET_PATH=/path/to/gekko/broker.sock GEKKO_BROKER_TOKEN=change-me ./broker_worker_bin
```

---

### 2. Executive Core
The executive core manages state simulations (Dry Mode) and coordinates wet-run routing to the broker socket.

Build and run:
```bash
cd executive-core
go build -o executive_core_bin
EXECUTIVE_DRY_MODE=true \
REDIS_ADDR=localhost:6379 \
KAFKA_BROKERS=localhost:9092 \
GEKKO_BROKER_TOKEN=change-me \
BROKER_SOCKET_PATH=/path/to/gekko/broker.sock \
./executive_core_bin
```

---

### 3. Kahuna Ingestion Worker
Kahuna polls simulation/live streams (Kalshi, Polymarket, Reuters, Twitter) and produces raw market lifecycle feeds into Kafka.

Build and run:
```bash
cd kahuna
go build -o kahuna_bin
KAFKA_ENABLED=true \
KAFKA_BROKERS=localhost:9092 \
KAFKA_TOPIC=kahuna.ingest.public \
QUARANTINE_DIR=/path/to/gekko/quarantine \
MONITOR_ADDR=:8085 \
./kahuna_bin
```

---

### 4. Gordon Quantitative Engine
Gordon consumes the raw market feeds from Kafka, fetches order books from Redis, performs RAG context evaluations against the local vector database, tests risk limit caps, and broadcasts calculated trade proposals to the outgoing Kafka topic.

Build and run:
```bash
cd gordon
go build -o gordon_bin
KAFKA_SIMULATED=false \
KAFKA_BROKERS=localhost:9092 \
REDIS_ADDR=localhost:6379 \
QDRANT_URL=http://localhost:6333 \
./gordon_bin
```
*Note: Gordon launches an interactive BubbleTea Terminal User Interface (TUI). Press `q` or `Ctrl+C` to shut down the engine cleanly.*

---

### 5. Liveview Backend Server
The liveview backend consumes the outgoing proposals from Kafka and streams them to connected client web browsers using Server-Sent Events (SSE).

Build and run:
```bash
cd gordon/liveview
go build -o liveview_bin
KAFKA_BROKERS=localhost:9092 \
PORT=8082 \
./liveview_bin
```

---

## Step 3: Launch the React UI Dashboard

The React frontend runs inside Vite's local dev server. It is configured to automatically proxy `/api` calls and SSE event connections directly to the liveview backend on port `8082`.

In a new terminal window:
```bash
cd frontend
npm install
npm run dev
```

Vite will start the dev server, typically at:
👉 **http://localhost:5173**

Open this address in your browser to view the rich, interactive dashboard, switch themes, edit RAG vector configurations, and hover over variables to view glossary definition tooltips!

---

## Step 4: Verification Checklist

Once the services are active, check that the pipeline is flowing:
1. **Ingestion Flow**: Check the Kahuna logs to see event polling triggers.
2. **Analysis Flow**: Verify Gordon's TUI is receiving events (`Events Consumed` count incrementing) and compiling strategy proposal entries.
3. **SSE Streams**: Open browser inspect network tools on `http://localhost:5173` to confirm the `/api/stream` event connection is open and active (receiving message payloads from the Go backend).
