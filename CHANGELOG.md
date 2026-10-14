# Changelog

## 0.2.0-rc3

### Added
- **`fallback_mode: neutral`** for detection streams (`low_<name>`): the last
  real frame is kept for `fallback_hold` (default 5 s, per default / per
  camera), then a neutral image (`fallback_neutral`: `offline`, `black`,
  `grey` or `file:<path>`) is served at 2 fps. Fixes a person caught in the
  last keyframe being replayed for hours and seen as a permanent person by the
  object detector (spurious events and presence).
- `/status`: `fallback_kind` (`last_keyframe` | `neutral` | `none`) and
  `fallback_since` (RFC 3339).
- Log lines for the transitions: `fallback: holding the last camera keyframe
  before the neutral image`, `fallback: switching to neutral`, `fallback:
  serving the neutral image`.

### Behaviour
- The neutral image applies to the **low** stream only. The main (recorded)
  stream keeps replaying the camera keyframe because an MP4 recorder needs the
  camera's own VPS/SPS/PPS. The neutral frame carries its own parameter sets
  (decoded, never recorded); its H.264 profile/level and size follow the
  camera's SPS.
- After a restart, the keyframe restored from `data_dir` is never shown on a
  neutral stream (it can contain a person): the neutral image is served
  immediately.
- `offline`, `last_frame` and `none` are unchanged.

## 0.2.0-rc2

### Fixed
- Recordings made through the proxy were undecodable after a cold start: the
  SDP advertised the generated fallback's parameter sets, and consumers that
  were already connected kept them. The stream is now rebuilt when the
  camera's VPS/SPS/PPS differ from the advertised ones.
- The camera port is probed explicitly before the RTSP client and the handshake
  gate (the "camera unreachable" log is now written once per sleep).

## 0.2.0-rc1

### Added
- TCP transport by default, `transport`, `dial_timeout`, `stall_timeout`,
  `audio`, `data_dir` settings.
- Immediate fallback when the camera closes the connection or stops sending
  video; the relay takes back on a keyframe.
- Handshakes of one camera are serialised and spaced.
- The last camera keyframe is persisted to `data_dir` and restored at start.
- G.711 camera audio is relayed (µ-law converted); other codecs are replaced
  by silence.
- Credentials are masked in logs; the container runs as non-root.
- One `session ended` log line per wake-up.

### Changed
- Configuration validation (codec, transport, audio, fps, durations, camera
  names, output paths); `${VAR}` only, undefined variables are an error;
  `battery_mode` defaults to `true`.
- `/status` is sorted and exposes `fallback_active`.
