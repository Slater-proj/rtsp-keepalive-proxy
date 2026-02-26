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
	if cfg.Defaults.FallbackFPS != 1 {
		t.Errorf("default FallbackFPS = %d, want 1", cfg.Defaults.FallbackFPS)
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
}

func TestEffectiveOverrides(t *testing.T) {
	defaults := CameraDefaults{
		BatteryMode:   true,
		RetryInterval: 5 * time.Second,
		Timeout:       15 * time.Second,
		FallbackFPS:   2,
		Codec:         "h264",
	}

	bm := false
	ri := 10 * time.Second
	fps := 5
	codec := "h265"

	cam := CameraConfig{
		Source:        "rtsp://localhost/test",
		BatteryMode:   &bm,
		RetryInterval: &ri,
		FallbackFPS:   &fps,
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
	// Timeout should come from defaults since not overridden
	if r.Timeout != 15*time.Second {
		t.Errorf("Timeout = %v, want 15s", r.Timeout)
	}
}
