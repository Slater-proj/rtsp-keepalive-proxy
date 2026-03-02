# rtsp-keepalive-proxy

A lightweight RTSP proxy that keeps camera streams alive for **Frigate NVR** (or any RTSP consumer), even when battery/WiFi cameras go to sleep.

## Problem

Battery-powered WiFi cameras (e.g. Reolink Argus, Dahua battery cams) only stream when motion is detected. Between events, the RTSP stream drops and Frigate reports the camera as **offline**, flooding logs with errors.

## Solution

This proxy sits between your cameras and Frigate:

```
Camera (intermittent) → rtsp-keepalive-proxy (always-on) → Frigate (happy)
```

When a camera is **streaming**: packets are relayed transparently with zero added latency.  
When a camera **sleeps**: the proxy generates a continuous fallback stream so Frigate never sees a disconnection.  
Fallback behaviour is configurable per camera: **offline overlay**, **last captured frame**, or **disabled**.

### Features

- **H.264 & H.265 (HEVC)** — auto-detects codec from camera SDP
- **Multi-camera** — one container handles all your battery cameras
- **Instant switchover** — fallback stream starts within one retry interval
- **Configurable fallback** — `offline` overlay, `last_frame`, or `none` (per camera)
- **Keyframe capture** — stores last IDR frame for `last_frame` mode
- **Per-camera config** — override battery mode, timeouts, FPS, fallback individually
- **Low + high quality** — `source_low` auto-creates a `low_<name>` output path
- **Health & status API** — HTTP endpoints for monitoring
- **Tiny footprint** — ~30 MB Docker image (Alpine + Go binary + FFmpeg)

## Quick Start

### 1. Configure

Copy and edit the example config:

```bash
cp config.yaml.example config.yaml
# Edit config.yaml with your camera URLs
```

### 2. Run with Docker Compose

```bash
docker compose up -d
```

### 3. Point Frigate to the proxy

In your Frigate `config.yml`, change camera inputs:

```yaml
cameras:
  Camera_Jardin:
    ffmpeg:
      inputs:
        - path: rtsp://rtsp-proxy:8554/low_jardin   # detect
          roles: [detect]
        - path: rtsp://rtsp-proxy:8554/jardin        # record
          input_args: preset-rtsp-restream
          roles: [record]

  Camera_Entree:
    ffmpeg:
      inputs:
        - path: rtsp://rtsp-proxy:8554/low_entree
          roles: [detect]
        - path: rtsp://rtsp-proxy:8554/entree
          input_args: preset-rtsp-restream
          roles: [record]
```

## Configuration Reference

```yaml
server:
  rtsp_port: 8554        # Output RTSP port
  health_port: 8080      # HTTP health/status port
  log_level: info         # debug | info | warn | error

defaults:
  battery_mode: true      # Enable sleep detection + fallback
  retry_interval: 3s      # Time between reconnection attempts
  timeout: 10s            # No-packet duration before declaring sleep
  fallback_fps: 5         # FPS of the still-image fallback stream
  fallback_mode: offline  # offline | last_frame | none
  codec: auto             # auto | h264 | h265

cameras:
  my_camera:
    source: "rtsp://user:pass@ip:554/stream"
    source_low: "rtsp://user:pass@ip:554/stream_low"  # optional
    # Per-camera overrides (all optional):
    battery_mode: true
    retry_interval: 3s
    timeout: 10s
    fallback_fps: 5
    fallback_mode: offline  # override per camera
    codec: auto
```

**Environment variable expansion**: use `${VAR}` in config values. Credentials can be passed via environment variables in `docker-compose.yml`.

## Architecture

```
┌────────────────────────────────────────────────────────┐
│              rtsp-keepalive-proxy container             │
│                                                        │
│  ┌─────────────┐    ┌──────────────┐    ┌───────────┐ │
│  │ Stream      │───▶│ RTSP Server  │───▶│ Frigate / │ │
│  │ Handler     │    │ (gortsplib)  │    │ Consumer  │ │
│  │ per camera  │    │ :8554        │    │           │ │
│  └──────┬──────┘    └──────────────┘    └───────────┘ │
│         │                                              │
│    ┌────▼─────┐                                        │
│    │ Fallback │ FFmpeg (still image encode)             │
│    │Generator │                                        │
│    └──────────┘                                        │
│                                                        │
│  ┌──────────────┐                                      │
│  │ Health HTTP  │  GET /health  GET /status             │
│  │ :8080        │                                      │
│  └──────────────┘                                      │
└────────────────────────────────────────────────────────┘
```

### Stream handler lifecycle per camera

```
START
  │
  ▼
CONNECTING ──── connect to RTSP source
  │              │
  │  success     │  failure
  ▼              ▼
ONLINE         SLEEPING (if battery_mode)
  │              │
  │  timeout     │  generates fallback stream
  │              │  retries connection
  ▼              │
  └──────────────┘
```

## API Endpoints

| Endpoint  | Method | Description                          |
|-----------|--------|--------------------------------------|
| `/health` | GET    | Returns `{"status": "ok"}` if alive  |
| `/status` | GET    | JSON array of all camera states      |

### Example `/status` response

```json
[
  {"name": "jardin",     "state": "online",   "codec": "h264", "last_online": "2026-02-26T10:30:00Z"},
  {"name": "low_jardin", "state": "sleeping", "codec": "h264", "last_online": "2026-02-26T10:29:55Z"},
  {"name": "entree",     "state": "sleeping", "codec": "h265", "last_online": "2026-02-26T10:25:00Z"}
]
```

## Development

### Prerequisites

- Go 1.22+
- FFmpeg (for fallback frame encoding)

### Build & Test

```bash
make build       # compile binary
make test        # run tests with race detector
make lint        # go vet
make coverage    # generate HTML coverage report
make docker      # build Docker image
```

### Project Structure

```
├── cmd/proxy/main.go           # Entry point
├── internal/
│   ├── config/                 # YAML configuration loading
│   ├── fallback/               # Still-image NAL unit generation
│   │   ├── annexb.go           # Annex-B parser
│   │   └── generator.go        # FFmpeg-based frame encoder
│   ├── health/                 # HTTP health/status endpoints
│   ├── logger/                 # Structured logging setup
│   └── proxy/
│       ├── manager.go          # Orchestrates all handlers
│       ├── server.go           # gortsplib RTSP server
│       └── stream_handler.go   # Per-camera relay + fallback logic
├── config.yaml.example
├── Dockerfile
├── docker-compose.yml
├── Makefile
└── .github/workflows/ci.yml
```

## CI/CD

The GitHub Actions workflow (`.github/workflows/ci.yml`):

1. **Test** — `go vet` + `go test -race` on every push/PR
2. **Docker** — builds and pushes to GitHub Container Registry on `main` and tags

## License

MIT — see [LICENSE](LICENSE).
