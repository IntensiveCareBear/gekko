# Build Stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Copy dependency manifests from parent (gordon) folder
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Compile the liveview package specifically
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags="-w -s" -o liveview_bin ./liveview

# Final Stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/liveview_bin /app/liveview_bin

# Default environments (overridden by compose)
ENV KAFKA_BROKERS=kafka:29092
ENV KAFKA_OUTBOUND_TOPIC=gordon.strategies.proposed
ENV PORT=8082

EXPOSE 8082

ENTRYPOINT ["/app/liveview_bin"]
