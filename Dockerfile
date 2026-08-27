# Build stage
FROM golang:1.27-bookworm AS builder

WORKDIR /app

# Cache deps
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o whatsmeow-app .

# Runtime stage
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libc6 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /app/whatsmeow-app .

# Persistent volume mount point
RUN mkdir -p /data
ENV DATA_DIR=/data

EXPOSE 8080

CMD ["./whatsmeow-app"]
