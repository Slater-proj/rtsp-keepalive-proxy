# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod ./
COPY go.sum* ./

COPY . .
RUN go mod tidy

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /rtsp-keepalive-proxy \
      ./cmd/proxy

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache \
      ffmpeg \
      ttf-dejavu \
      tzdata \
      ca-certificates

COPY --from=builder /rtsp-keepalive-proxy /usr/local/bin/rtsp-keepalive-proxy

# Default config mount point
VOLUME ["/etc/rtsp-proxy"]

EXPOSE 8554/tcp
EXPOSE 8080/tcp

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
      CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["rtsp-keepalive-proxy"]
CMD ["-config", "/etc/rtsp-proxy/config.yaml"]
