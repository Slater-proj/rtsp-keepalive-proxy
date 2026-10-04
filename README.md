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
- **Fast takeover** — the fallback starts when the camera closes the connection, or after `stall_timeout` (3 s by default) without video, instead of waiting for the full `timeout`
- **Safe audio** — G.711 camera audio is relayed (µ-law is converted to A-law); any other codec is replaced by silence rather than corrupting the track
- **Small footprint** — Alpine + a static Go binary + FFmpeg, runs as a non-root user

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
        - path: rtsp://rtsp-proxy:8554/low_jardin   # detect (H.264 sub stream)
          roles: [detect]
        - path: rtsp://rtsp-proxy:8554/jardin        # record (H.265 main stream)
          input_args: preset-rtsp-restream
          roles: [record]
      output_args:
        record: preset-record-generic-audio-aac

  Camera_Entree:
    ffmpeg:
      inputs:
        - path: rtsp://rtsp-proxy:8554/low_entree
          roles: [detect]
        - path: rtsp://rtsp-proxy:8554/entree
          input_args: preset-rtsp-restream
          roles: [record]
      output_args:
        record: preset-record-generic-audio-aac
```

## Configuration Reference

```yaml
server:
  rtsp_port: 8554        # Output RTSP port
  health_port: 8080      # HTTP health/status port
  log_level: info         # debug | info | warn | error
  data_dir: /data         # optional: persist the last camera keyframe across restarts

defaults:
  battery_mode: true      # Enable sleep detection + fallback
  retry_interval: 3s      # Time between reconnection attempts (default 3s)
  timeout: 10s            # No packet at all before the connection is dropped (default 10s)
  dial_timeout: 0s        # TCP connect timeout, 0 = automatic (3s battery / 5s)
  stall_timeout: 3s       # No VIDEO before the fallback takes over (default 3s)
  fallback_fps: 5         # FPS of the still-image fallback stream (1-60, default 5)
  fallback_mode: offline  # offline | last_frame | none
  codec: auto             # auto | h264 | h265
  transport: tcp          # tcp | udp | auto  (default tcp)
  audio: auto             # auto (relay G.711, else silence) | none

cameras:
  my_camera:
    source: "rtsp://user:pass@ip:554/stream"
    source_low: "rtsp://user:pass@ip:554/stream_low"  # optional second path
    # Codec per stream (avoids rebuild delay on first connect):
    codec: h265           # main stream codec (auto | h264 | h265)
    codec_low: h264       # low stream codec (defaults to codec if unset)
    # Resolution hints for fallback frames (before camera first connects):
    width: 1920           # main stream width
    height: 1080          # main stream height
    width_low: 640        # low stream width
    height_low: 480       # low stream height
    # Per-camera overrides (all optional — inherits from defaults):
    battery_mode: true
    retry_interval: 1s
    timeout: 5s
    stall_timeout: 2s
    fallback_fps: 5
    fallback_mode: offline
    transport: tcp
    audio: auto
```

If `defaults.battery_mode` is omitted it is `true`. The configuration is validated at start-up: unknown codecs, transports or fallback modes, out-of-range values, invalid camera names and colliding output paths (`low_x` produced by `x` + `source_low` and a camera named `low_x`) are rejected with an explicit error.

### Fallback modes

| Mode | What consumers receive while the camera sleeps |
|------|-----------------------------------------------|
| `offline` | A generated `<NAME> - OFFLINE` image **until the camera has streamed once**. After that, the camera's **last keyframe** is replayed (see below). |
| `last_frame` | Same replay of the camera's last keyframe; additionally a PNG snapshot is kept (costs one short FFmpeg run per 5 s of live video). |
| `none` | Nothing: consumers see a disconnect. |

Why the last camera keyframe is preferred over the generated image once available: a generated frame carries its own SPS/PPS (or VPS/SPS/PPS), and MP4 recording in Frigate (`-c copy`) writes one codec configuration per segment. Mixing two encoders' parameter sets in one segment makes it unplayable.

### Audio

The output always carries a G.711 A-law (PCMA, 8 kHz, mono) audio track so consumers never lose it. Camera audio is relayed only when it is G.711 8 kHz mono; µ-law is converted on the fly. For AAC, Opus or other codecs the proxy logs a warning and keeps the silent track (use `audio: none` to silence the warning). Check your camera's codec with `ffprobe` if the audio sounds wrong.

### Codec configuration

For **Dahua battery cameras** (H.265 main + H.264 sub), set explicit codecs:

```yaml
codec: h265       # main stream = H.265
codec_low: h264   # sub stream = H.264
```

This eliminates a stream rebuild on the first camera connection. With `codec: auto`, the proxy starts in H.264 mode and must rebuild the entire RTSP session when it discovers H.265 — go2rtc/Frigate must reconnect, adding ~2s delay.

**Environment variable expansion**: use `${VAR}` in config values. Credentials can be passed via environment variables in `docker-compose.yml`. Only the braced form is expanded (a `$` inside a password is left alone), and a reference to an undefined variable is a start-up error instead of a silently empty password. Credentials are masked (`rtsp://user:***@host`) in all logs.

**File permissions**: the container runs as a non-root user (uid 10001 by default). Make sure `config.yaml` is readable by it, or set `user: "<uid>:<gid>"` in your compose file.

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
| `/status` | GET    | JSON array of all camera states, sorted by name |

### Example `/status` response

```json
[
  {"name": "entree",     "state": "sleeping", "codec": "h265", "last_online": "2026-02-26T10:25:00Z", "fallback_active": true},
  {"name": "jardin",     "state": "online",   "codec": "h265", "last_online": "2026-02-26T10:30:00Z", "fallback_active": false},
  {"name": "low_jardin", "state": "online",   "codec": "h264", "last_online": "2026-02-26T10:30:00Z", "fallback_active": false}
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
