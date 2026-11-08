# Build Stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Copy dependency files
COPY go.mod ./
# Note: we run tidy/download inside the build script or builder
RUN go mod download

# Copy the rest of the source code
COPY . .

# Compile static binary without CGO (very secure and portable)
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags="-w -s" -o kahuna .

# Final Stage
FROM alpine:3.19

# Install certificates for SSL/HTTPS connections (essential for Reuters live RSS feeds)
RUN apk --no-cache add ca-certificates

# Pre-create the required quarantine log directory
RUN mkdir -p /var/log/gekko/quarantine && chmod 777 /var/log/gekko/quarantine

WORKDIR /app

# Copy the compiled binary from the builder stage
COPY --from=builder /app/kahuna /app/kahuna

# Environment variables setup
ENV LOG_LEVEL=info
ENV KAFKA_ENABLED=false
ENV QUARANTINE_DIR=/var/log/gekko/quarantine
ENV QUARANTINE_FALLBACK=/app/quarantine

# Run as non-root user for security compliance
USER 10001:10001

CMD ["/app/kahuna"]
