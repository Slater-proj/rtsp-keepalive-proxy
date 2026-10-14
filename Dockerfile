# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
RUN go mod download

COPY . .

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

# The proxy needs no privileges: ports are > 1024 and it only writes temp files.
RUN addgroup -S -g 10001 proxy && adduser -S -u 10001 -G proxy -H -s /sbin/nologin proxy
RUN mkdir -p /data && chown proxy:proxy /data

COPY --from=builder /rtsp-keepalive-proxy /usr/local/bin/rtsp-keepalive-proxy

# Default config mount point
VOLUME ["/etc/rtsp-proxy", "/data"]

EXPOSE 8554/tcp
EXPOSE 8080/tcp

USER proxy

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
      CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["rtsp-keepalive-proxy"]
CMD ["-config", "/etc/rtsp-proxy/config.yaml"]
