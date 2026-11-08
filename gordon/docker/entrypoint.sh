#!/bin/bash
set -e

# Default REDIS_ADDR if not set
REDIS_ADDR=${REDIS_ADDR:-"localhost:6379"}

# Parse host and port from REDIS_ADDR
IFS=':' read -r REDIS_HOST REDIS_PORT <<< "$REDIS_ADDR"
REDIS_PORT=${REDIS_PORT:-"6379"}

# If host is localhost or 127.0.0.1, spin up local redis-server
if [ "$REDIS_HOST" = "localhost" ] || [ "$REDIS_HOST" = "127.0.0.1" ]; then
    echo "[INFO] Starting local daemonized Redis server on port $REDIS_PORT..."
    redis-server --port "$REDIS_PORT" --daemonize yes
    
    # Wait for Redis to start up
    until redis-cli -p "$REDIS_PORT" ping >/dev/null 2>&1; do
        echo "[INFO] Waiting for local Redis to initialize..."
        sleep 0.5
    done
    echo "[INFO] Local Redis initialized successfully."
else
    echo "[INFO] Using external Redis server at $REDIS_ADDR"
fi

# Run gordon binary
exec /app/gordon
