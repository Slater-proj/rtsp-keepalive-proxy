package config

import (
	"testing"
	"time"
)

const validYAML = `
server:
  rtsp_port: 9554
  health_port: 9080
  log_level: debug
defaults:
  battery_mode: true
  retry_interval: 5s
  timeout: 15s
  fallback_fps: 2
  codec: h264
cameras:
  test_cam:
    source: "rtsp://user:pass@10.0.0.1:554/stream"
    source_low: "rtsp://user:pass@10.0.0.1:554/stream_low"
`

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.RTSPPort != 9554 {
		t.Errorf("RTSPPort = %d, want 9554", cfg.Server.RTSPPort)
	}
	if cfg.Server.HealthPort != 9080 {
		t.Errorf("HealthPort = %d, want 9080", cfg.Server.HealthPort)
	}
	if cfg.Server.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.Server.LogLevel)
	}
	if cfg.Defaults.RetryInterval != 5*time.Second {
		t.Errorf("RetryInterval = %v, want 5s", cfg.Defaults.RetryInterval)
	}
	if cfg.Defaults.FallbackFPS != 2 {
		t.Errorf("FallbackFPS = %d, want 2", cfg.Defaults.FallbackFPS)
	}

	cam, ok := cfg.Cameras["test_cam"]
	if !ok {
		t.Fatal("camera test_cam not found")
	}
	if cam.Source == "" {
		t.Error("Source is empty")
	}
	if cam.SourceLow == "" {
		t.Error("SourceLow is empty")
	}
}

func TestParseServerDefaults(t *testing.T) {
	yaml := `
cameras:
  c1:
    source: "rtsp://localhost/test"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.RTSPPort != 8554 {
		t.Errorf("default RTSPPort = %d, want 8554", cfg.Server.RTSPPort)
	}
	if cfg.Server.HealthPort != 8080 {
		t.Errorf("default HealthPort = %d, want 8080", cfg.Server.HealthPort)
	}
	if cfg.Server.LogLevel != "info" {
		t.Errorf("default LogLevel = %q, want info", cfg.Server.LogLevel)
	}
	if cfg.Defaults.RetryInterval != 3*time.Second {
		t.Errorf("default RetryInterval = %v, want 3s", cfg.Defaults.RetryInterval)
	}
	if cfg.Defaults.Timeout != 10*time.Second {
		t.Errorf("default Timeout = %v, want 10s", cfg.Defaults.Timeout)
	}
	if cfg.Defaults.FallbackFPS != 5 {
		t.Errorf("default FallbackFPS = %d, want 5", cfg.Defaults.FallbackFPS)
	}
	if cfg.Defaults.FallbackMode != "offline" {
		t.Errorf("default FallbackMode = %q, want offline", cfg.Defaults.FallbackMode)
	}
	if cfg.Defaults.Codec != "auto" {
		t.Errorf("default Codec = %q, want auto", cfg.Defaults.Codec)
	}
}

func TestParseNoCameras(t *testing.T) {
	yaml := `
server:
  rtsp_port: 8554
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for missing cameras")
	}
}

func TestParseMissingSource(t *testing.T) {
	yaml := `
cameras:
  bad:
    source_low: "rtsp://localhost/low"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestEffectiveDefaults(t *testing.T) {
	defaults := CameraDefaults{
		BatteryMode:   true,
		RetryInterval: 5 * time.Second,
		Timeout:       15 * time.Second,
		FallbackFPS:   2,
		FallbackMode:  "offline",
		Codec:         "h264",
	}

	cam := CameraConfig{
		Source: "rtsp://localhost/test",
	}

	r := cam.Effective(defaults)
	if !r.BatteryMode {
		t.Error("BatteryMode should be true from defaults")
	}
	if r.RetryInterval != 5*time.Second {
		t.Errorf("RetryInterval = %v, want 5s", r.RetryInterval)
	}
	if r.FallbackFPS != 2 {
		t.Errorf("FallbackFPS = %d, want 2", r.FallbackFPS)
	}
	if r.Codec != "h264" {
		t.Errorf("Codec = %q, want h264", r.Codec)
	}
	if r.FallbackMode != "offline" {
		t.Errorf("FallbackMode = %q, want offline", r.FallbackMode)
	}
}

func TestEffectiveOverrides(t *testing.T) {
	defaults := CameraDefaults{
		BatteryMode:   true,
		RetryInterval: 5 * time.Second,
		Timeout:       15 * time.Second,
		FallbackFPS:   2,
		FallbackMode:  "offline",
		Codec:         "h264",
	}

	bm := false
	ri := 10 * time.Second
	fps := 5
	fmode := "last_frame"
	codec := "h265"

	cam := CameraConfig{
		Source:        "rtsp://localhost/test",
		BatteryMode:   &bm,
		RetryInterval: &ri,
		FallbackFPS:   &fps,
		FallbackMode:  &fmode,
		Codec:         &codec,
	}

	r := cam.Effective(defaults)
	if r.BatteryMode {
		t.Error("BatteryMode should be overridden to false")
	}
	if r.RetryInterval != 10*time.Second {
		t.Errorf("RetryInterval = %v, want 10s", r.RetryInterval)
	}
	if r.FallbackFPS != 5 {
		t.Errorf("FallbackFPS = %d, want 5", r.FallbackFPS)
	}
	if r.Codec != "h265" {
		t.Errorf("Codec = %q, want h265", r.Codec)
	}
	if r.FallbackMode != "last_frame" {
		t.Errorf("FallbackMode = %q, want last_frame", r.FallbackMode)
	}
	// Timeout should come from defaults since not overridden
	if r.Timeout != 15*time.Second {
		t.Errorf("Timeout = %v, want 15s", r.Timeout)
	}
}

func TestInvalidFallbackMode(t *testing.T) {
	yaml := `
defaults:
  fallback_mode: invalid
cameras:
  c1:
    source: "rtsp://localhost/test"
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for invalid fallback_mode")
	}
}

func TestInvalidCameraFallbackMode(t *testing.T) {
	yaml := `
cameras:
  c1:
    source: "rtsp://localhost/test"
    fallback_mode: bad
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for invalid per-camera fallback_mode")
	}
}

func TestBatteryModeDefaultsToTrueWhenOmitted(t *testing.T) {
	cfg, err := Parse([]byte(`
cameras:
  c1:
    source: "rtsp://localhost/test"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Defaults.BatteryMode {
		t.Error("battery_mode should default to true when omitted")
	}
}

func TestBatteryModeExplicitFalseIsKept(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults:
  battery_mode: false
cameras:
  c1:
    source: "rtsp://localhost/test"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Defaults.BatteryMode {
		t.Error("explicit battery_mode: false must be preserved")
	}
}

func TestNewDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
cameras:
  c1:
    source: "rtsp://localhost/test"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Defaults.Transport != "tcp" {
		t.Errorf("default Transport = %q, want tcp", cfg.Defaults.Transport)
	}
	if cfg.Defaults.Audio != "auto" {
		t.Errorf("default Audio = %q, want auto", cfg.Defaults.Audio)
	}
	if cfg.Defaults.StallTimeout != 3*time.Second {
		t.Errorf("default StallTimeout = %v, want 3s", cfg.Defaults.StallTimeout)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("RTSP_TEST_PASS", "s3cr$t")
	cfg, err := Parse([]byte(`
cameras:
  c1:
    source: "rtsp://admin:${RTSP_TEST_PASS}@10.0.0.1/s"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Cameras["c1"].Source; got != "rtsp://admin:s3cr$t@10.0.0.1/s" {
		t.Errorf("Source = %q", got)
	}
}

func TestEnvExpansionKeepsBareDollar(t *testing.T) {
	cfg, err := Parse([]byte(`
cameras:
  c1:
    source: "rtsp://admin:pa$word@10.0.0.1/s"
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Cameras["c1"].Source; got != "rtsp://admin:pa$word@10.0.0.1/s" {
		t.Errorf("a bare $ must not be expanded, got %q", got)
	}
}

func TestEnvExpansionMissingVariable(t *testing.T) {
	_, err := Parse([]byte(`
cameras:
  c1:
    source: "rtsp://admin:${DEFINITELY_NOT_SET_XYZ}@10.0.0.1/s"
`))
	if err == nil {
		t.Fatal("expected error for undefined environment variable")
	}
}

func TestInvalidValues(t *testing.T) {
	cases := map[string]string{
		"codec typo":     "codec: H265",
		"transport":      "transport: sctp",
		"audio":          "audio: aac",
		"fps zero-ish":   "fallback_fps: 500",
		"negative":       "timeout: -5s",
		"codec_low":      "codec_low: vp9",
		"negative stall": "stall_timeout: -1s",
		"negative retry": "retry_interval: -1s",
		"fallback_fps<0": "fallback_fps: -2",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte("cameras:\n  c1:\n    source: \"rtsp://x/y\"\n    " + line + "\n"))
			if err == nil {
				t.Fatalf("expected an error for %q", line)
			}
		})
	}
}

func TestInvalidCameraName(t *testing.T) {
	for _, name := range []string{"has space", "a/b", "it's", "-lead"} {
		_, err := Parse([]byte("cameras:\n  \"" + name + "\":\n    source: \"rtsp://x/y\"\n"))
		if err == nil {
			t.Errorf("expected an error for camera name %q", name)
		}
	}
}

func TestOutputPathCollision(t *testing.T) {
	_, err := Parse([]byte(`
cameras:
  jardin:
    source: "rtsp://x/main"
    source_low: "rtsp://x/low"
  low_jardin:
    source: "rtsp://y/main"
`))
	if err == nil {
		t.Fatal("expected a collision between jardin/source_low and low_jardin")
	}
}

func TestDialTimeout(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults:
  dial_timeout: 1s
cameras:
  a:
    source: "rtsp://x/a"
  b:
    source: "rtsp://x/b"
    dial_timeout: 500ms
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Cameras["a"].Effective(cfg.Defaults).DialTimeout; got != time.Second {
		t.Errorf("a: DialTimeout = %v, want 1s (from defaults)", got)
	}
	if got := cfg.Cameras["b"].Effective(cfg.Defaults).DialTimeout; got != 500*time.Millisecond {
		t.Errorf("b: DialTimeout = %v, want 500ms (override)", got)
	}
	if _, err := Parse([]byte("cameras:\n  a:\n    source: \"rtsp://x/a\"\n    dial_timeout: -1s\n")); err == nil {
		t.Error("negative dial_timeout must be rejected")
	}
}
