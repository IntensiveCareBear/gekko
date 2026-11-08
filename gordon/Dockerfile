# Build Stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Copy dependency files
COPY go.mod ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Compile static binary without CGO (very secure and portable)
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags="-w -s" -o gordon .

# Final Stage
FROM alpine:3.19

# Install redis-server, bash, and ncurses (essential for terminal TUI terminfo databases)
RUN apk --no-cache add redis bash ncurses ca-certificates

WORKDIR /app

# Copy the compiled binary from the builder stage
COPY --from=builder /app/gordon /app/gordon

# Copy the entrypoint script
COPY docker/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# Environment variables setup
ENV REDIS_ADDR=localhost:6379
ENV KAFKA_BROKERS=localhost:9092
ENV KAFKA_INBOUND_TOPIC=kahuna.ingest.public
ENV KAFKA_OUTBOUND_TOPIC=gordon.strategies.proposed
ENV KAFKA_CONSUMER_GROUP=gordon-group
ENV KAFKA_SIMULATED=true
ENV TERM=xterm-256color

# Run under the entrypoint script
ENTRYPOINT ["/app/entrypoint.sh"]
